package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障备件重复登记在“备件已被承诺并部分使用”场景下的行为：
//
// 主例数据（与用户描述一致）：备件 part-dupr 首次登记十二件；两笔在保的
// 合格请求分别取得七件（c-seven，属于 r-dupr-seven）和三件（c-three，属于
// r-dupr-three）的有效承诺，再从七件承诺中使用两件。此时实物剩余十件、
// 有效占用八件（七件承诺未用五件加三件承诺未用三件）、可承诺两件。
//
// 随后以原编号再次提交明显不同的初始库存（相同的十二件、更大的九十九件、
// 更小的五件、零件与负一件）：编号已存在优先于本次库存校验，每一次都必须
// 返回 ErrDuplicateID——负数在首次登记时虽然非法，编号已存在时也只能报重复，
// 不能改报 ErrInvalidParam。重复登记既不是补货也不会清零库存：失败后直接
// 查询备件（Part）与查询库存占用（PartStatus）必须继续反映实物剩余十件、
// 有效占用八件、可承诺两件；两笔承诺仍属于原请求，原定数量、已用数量、
// 未用数量和到期时刻都不能改变，按请求查看承诺（RequestView）同样保留。
//
// 重复登记被拒绝后，新来的合格请求仍按原来的两件可承诺数量处理：申请三件
// 返回 ErrInsufficientStock，不生成承诺、不增加占用；申请两件可以成功，
// 成功后实物剩余仍是十件、有效占用十件、可承诺数量为零。所有操作时刻均
// 早于承诺到期时刻，库存差异只来自真实使用，而非到期释放。
//
// 另区分首次登记失败与重复登记：未登记的非空编号首次提交负库存返回
// ErrInvalidParam 且不占用编号（查询仍报不存在）；随后以同一编号提交零
// 库存可以正常登记，实物剩余、有效占用与可承诺数量均为零。

const (
	duprPartID       = "part-dupr"
	duprProductID    = "p-dupr"
	duprSevenRequest = "r-dupr-seven"
	duprThreeRequest = "r-dupr-three"
	duprLateRequest  = "r-dupr-late"
	duprSevenCommit  = "c-seven"
	duprThreeCommit  = "c-three"
	duprLateCommit2  = "c-late-two"
	duprLateCommit3  = "c-late-three"
	duprStock        = 12
)

var (
	duprExpirySeven = t0.Add(40 * day)
	duprExpiryThree = t0.Add(45 * day)
	duprExpiryLate  = t0.Add(35 * day)
)

// duprNewStore 构造主例状态：登记十二件的备件与在保产品，两笔合格请求分别
// 预留七件和三件，再从七件承诺中使用两件。完成时库存账目为 10/8/2。
func duprNewStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct(duprProductID, t0, 120, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(duprPartID, duprStock); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(duprSevenRequest, duprProductID, "NOISE"); err != nil {
		t.Fatalf("submit seven request: %v", err)
	}
	if err := s.SubmitRequest(duprThreeRequest, duprProductID, "NOISE"); err != nil {
		t.Fatalf("submit three request: %v", err)
	}
	if _, err := s.Reserve(duprSevenCommit, duprSevenRequest, duprPartID, 7, duprExpirySeven, nowOK); err != nil {
		t.Fatalf("reserve c-seven: %v", err)
	}
	if _, err := s.Reserve(duprThreeCommit, duprThreeRequest, duprPartID, 3, duprExpiryThree, nowOK); err != nil {
		t.Fatalf("reserve c-three: %v", err)
	}
	if _, err := s.Use("u-seven-2", duprSevenCommit, 2, nowOK); err != nil {
		t.Fatalf("use 2 from c-seven: %v", err)
	}
	return s
}

// assertDuprAccount 断言 PartStatus 的库存三项为期望值。
func assertDuprAccount(t *testing.T, st *PartStatus, physical, occupied, committable int) {
	t.Helper()
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock account phys=%d occupied=%d committable=%d, want phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// assertDuprDetail 断言一条明细完整反映一笔承诺的查询当时事实：备件归属、
// 请求归属、原定、已用、未用、active 状态与到期时刻。
func assertDuprDetail(t *testing.T, d CommitmentDetail, commitID, requestID string, original, used, remaining int, expiry time.Time) {
	t.Helper()
	if d.CommitmentID != commitID || d.RequestID != requestID || d.PartID != duprPartID ||
		d.OriginalQuantity != original || d.UsedQuantity != used || d.RemainingQuantity != remaining ||
		d.Status != CommitmentActive || !d.Expiry.Equal(expiry) {
		t.Fatalf("detail %q: part=%s req=%s orig=%d used=%d rem=%d status=%s expiry=%v; "+
			"want part=%s req=%s orig=%d used=%d rem=%d status=%s expiry=%v",
			d.CommitmentID, d.PartID, d.RequestID, d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status, d.Expiry,
			duprPartID, requestID, original, used, remaining, CommitmentActive, expiry)
	}
}

// assertDuprOriginalFacts 在重复登记冲击后核对全部原有事实：
//   - Part 直接查询：备件仍存在，实物库存为十件；
//   - PartStatus：10/8/2，恰好两条按编号排序的 active 明细，七件承诺
//     原定 7、已用 2、未用 5，三件承诺原定 3、已用 0、未用 3，到期时刻不变；
//   - 仓库中的承诺记录归属、原定、已用、未用、到期时刻与未取消/未到期状态不变；
//   - RequestView 按请求查看：每笔承诺仍属于原请求，数量与到期时刻不变。
func assertDuprOriginalFacts(t *testing.T, s *Store, when time.Time) {
	t.Helper()

	if p, err := s.Part(duprPartID); err != nil || p.Stock != 10 {
		t.Fatalf("part after duplicate submission: stock=%d err=%v, want 10", p.Stock, err)
	}

	st, err := s.PartStatus(duprPartID, when)
	if err != nil {
		t.Fatalf("part status after duplicate submission: %v", err)
	}
	assertDuprAccount(t, st, 10, 8, 2)
	if len(st.Details) != 2 {
		t.Fatalf("details = %d, want exactly 2: %+v", len(st.Details), st.Details)
	}
	if st.Details[0].CommitmentID != duprSevenCommit || st.Details[1].CommitmentID != duprThreeCommit {
		t.Fatalf("details order = %q, %q, want sorted %q, %q",
			st.Details[0].CommitmentID, st.Details[1].CommitmentID, duprSevenCommit, duprThreeCommit)
	}
	assertDuprDetail(t, st.Details[0], duprSevenCommit, duprSevenRequest, 7, 2, 5, duprExpirySeven)
	assertDuprDetail(t, st.Details[1], duprThreeCommit, duprThreeRequest, 3, 0, 3, duprExpiryThree)

	for _, tc := range []struct {
		id        string
		requestID string
		quantity  int
		used      int
		expiry    time.Time
	}{
		{duprSevenCommit, duprSevenRequest, 7, 2, duprExpirySeven},
		{duprThreeCommit, duprThreeRequest, 3, 0, duprExpiryThree},
	} {
		c, err := s.Commitment(tc.id)
		if err != nil {
			t.Fatalf("commitment %s: %v", tc.id, err)
		}
		if c.RequestID != tc.requestID || c.PartID != duprPartID ||
			c.Quantity != tc.quantity || c.Used != tc.used || c.Unused() != tc.quantity-tc.used ||
			c.Canceled || c.Expired || !c.Expiry.Equal(tc.expiry) {
			t.Fatalf("stored commitment %s disturbed: %+v", tc.id, c)
		}
	}

	// 按请求查看承诺：两笔承诺仍分别属于原请求，字段全部保留。
	v7, err := s.RequestView(duprSevenRequest, when)
	if err != nil {
		t.Fatalf("request view seven: %v", err)
	}
	if v7.Eligibility == nil || !v7.Eligibility.Eligible {
		t.Fatalf("seven request eligibility disturbed: %+v", v7.Eligibility)
	}
	if len(v7.Commitments) != 1 {
		t.Fatalf("seven request commitments = %d, want 1: %+v", len(v7.Commitments), v7.Commitments)
	}
	assertDuprDetail(t, v7.Commitments[0], duprSevenCommit, duprSevenRequest, 7, 2, 5, duprExpirySeven)

	v3, err := s.RequestView(duprThreeRequest, when)
	if err != nil {
		t.Fatalf("request view three: %v", err)
	}
	if v3.Eligibility == nil || !v3.Eligibility.Eligible {
		t.Fatalf("three request eligibility disturbed: %+v", v3.Eligibility)
	}
	if len(v3.Commitments) != 1 {
		t.Fatalf("three request commitments = %d, want 1: %+v", len(v3.Commitments), v3.Commitments)
	}
	assertDuprDetail(t, v3.Commitments[0], duprThreeCommit, duprThreeRequest, 3, 0, 3, duprExpiryThree)
}

// TestRegisterPartDuplicateAfterCommitmentAndUse 已登记十二件、两笔承诺共占用
// 八件且七件承诺已使用两件之后，以原编号提交相同、更大、更小、零和负的
// 初始库存：全部返回 ErrDuplicateID（负数也不能报 ErrInvalidParam），库存
// 账目、承诺事实与按请求/按备件两类查询全部保持原值。
func TestRegisterPartDuplicateAfterCommitmentAndUse(t *testing.T) {
	s := duprNewStore(t)

	// 冲击前先确认主例事实：实物剩余十件、有效占用八件、可承诺两件。
	assertDuprOriginalFacts(t, s, nowOK)

	// 相同库存、更大库存、更小的非负库存、零和负数：编号已存在优先于本次
	// 库存校验，一律报重复，且不能同时被识别为参数非法。
	for _, stock := range []int{12, 99, 5, 0, -1} {
		err := s.RegisterPart(duprPartID, stock)
		if !errors.Is(err, ErrDuplicateID) {
			t.Fatalf("re-register with stock %d: got %v, want ErrDuplicateID", stock, err)
		}
		if errors.Is(err, ErrInvalidParam) {
			t.Fatalf("re-register with stock %d: err %v matches ErrInvalidParam; duplicate id must win", stock, err)
		}
		// 每次失败后直接查询备件与库存占用，都继续反映原来的库存事实；
		// 查询时刻早于两笔到期时刻，差异不来自到期释放。
		assertDuprOriginalFacts(t, s, nowOK.Add(time.Duration(stock)*time.Minute))
	}
}

// TestReservationsAfterRejectedDuplicateRegistration 重复登记被拒绝后，新来的
// 合格请求仍按原来的两件可承诺数量处理：申请三件库存不足且不留承诺，申请
// 两件成功；失败提交中的初始库存既不是补货也不是清零。
func TestReservationsAfterRejectedDuplicateRegistration(t *testing.T) {
	s := duprNewStore(t)

	// 用明显不同的初始库存（零与负数）重复登记，全部被拒绝。
	for _, stock := range []int{0, -1, 99} {
		if err := s.RegisterPart(duprPartID, stock); !errors.Is(err, ErrDuplicateID) {
			t.Fatalf("re-register with stock %d: got %v, want ErrDuplicateID", stock, err)
		}
	}
	assertDuprOriginalFacts(t, s, nowOK)

	if err := s.SubmitRequest(duprLateRequest, duprProductID, "NOISE"); err != nil {
		t.Fatalf("submit late request: %v", err)
	}

	// 申请三件超过两件可承诺数量：ErrInsufficientStock，不生成承诺、不增加占用。
	later := nowOK.Add(time.Hour)
	if c, err := s.Reserve(duprLateCommit3, duprLateRequest, duprPartID, 3, duprExpiryLate, later); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve 3 with committable 2: commitment=%+v err=%v, want ErrInsufficientStock", c, err)
	} else if c != (Commitment{}) {
		t.Fatalf("rejected reserve returned non-empty commitment: %+v", c)
	}
	if _, err := s.Commitment(duprLateCommit3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected commitment %q exists: err=%v, want ErrNotFound", duprLateCommit3, err)
	}
	assertDuprOriginalFacts(t, s, later)

	// 申请两件恰好成功：成功后实物剩余仍是十件、有效占用十件、可承诺为零。
	c2, err := s.Reserve(duprLateCommit2, duprLateRequest, duprPartID, 2, duprExpiryLate, later)
	if err != nil {
		t.Fatalf("reserve 2 with committable 2: %v", err)
	}
	if c2.Quantity != 2 || c2.Used != 0 || c2.Unused() != 2 || c2.RequestID != duprLateRequest ||
		c2.PartID != duprPartID || c2.Canceled || c2.Expired || !c2.Expiry.Equal(duprExpiryLate) {
		t.Fatalf("successful late commitment: %+v, want qty=2 used=0 unused=2 active", c2)
	}

	st, err := s.PartStatus(duprPartID, later)
	if err != nil {
		t.Fatalf("part status after successful late reserve: %v", err)
	}
	assertDuprAccount(t, st, 10, 10, 0)
	if len(st.Details) != 3 {
		t.Fatalf("details = %d, want exactly 3: %+v", len(st.Details), st.Details)
	}
	assertDuprDetail(t, mustDetailByID(t, st.Details, duprSevenCommit), duprSevenCommit, duprSevenRequest, 7, 2, 5, duprExpirySeven)
	assertDuprDetail(t, mustDetailByID(t, st.Details, duprThreeCommit), duprThreeCommit, duprThreeRequest, 3, 0, 3, duprExpiryThree)
	assertDuprDetail(t, mustDetailByID(t, st.Details, duprLateCommit2), duprLateCommit2, duprLateRequest, 2, 0, 2, duprExpiryLate)
	if p, err := s.Part(duprPartID); err != nil || p.Stock != 10 {
		t.Fatalf("physical stock = %d (err %v), want 10", p.Stock, err)
	}

	// 可承诺已为零：再来一个合格请求申请一件也无法预留。
	if err := s.SubmitRequest("r-dupr-last", duprProductID, "NOISE"); err != nil {
		t.Fatalf("submit last request: %v", err)
	}
	if _, err := s.Reserve("c-late-one", "r-dupr-last", duprPartID, 1, duprExpiryLate, later); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve 1 with committable 0: got %v, want ErrInsufficientStock", err)
	}
}

// TestRegisterPartFirstTimeNegativeThenZero 首次登记失败与重复登记区分清楚：
// 未登记编号提交负库存返回 ErrInvalidParam 且不保存记录、不占用编号；随后
// 以同一编号提交零库存可以正常登记，三项库存数字均为零。
func TestRegisterPartFirstTimeNegativeThenZero(t *testing.T) {
	s := NewStore()
	const freshPart = "part-dupr-fresh"

	if err := s.RegisterPart(freshPart, -1); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("first registration with negative stock: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Part(freshPart); !errors.Is(err, ErrNotFound) {
		t.Fatalf("part after failed first registration: got %v, want ErrNotFound", err)
	}
	if _, err := s.PartStatus(freshPart, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("part status after failed first registration: got %v, want ErrNotFound", err)
	}

	// 同一编号修正为零库存后可以正常登记：零库存也是有效登记。
	if err := s.RegisterPart(freshPart, 0); err != nil {
		t.Fatalf("register with zero stock after negative rejection: %v", err)
	}
	if p, err := s.Part(freshPart); err != nil || p.Stock != 0 {
		t.Fatalf("part zero registration: stock=%d err=%v, want 0", p.Stock, err)
	}
	st, err := s.PartStatus(freshPart, nowOK)
	if err != nil {
		t.Fatalf("part status zero registration: %v", err)
	}
	assertDuprAccount(t, st, 0, 0, 0)
	if len(st.Details) != 0 {
		t.Fatalf("zero-stock details = %d, want 0", len(st.Details))
	}

	// 登记成功之后再提交负库存，编号已存在优先：报重复而不是参数非法。
	if err := s.RegisterPart(freshPart, -1); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("negative stock after successful registration: got %v, want ErrDuplicateID", err)
	}
	if p, err := s.Part(freshPart); err != nil || p.Stock != 0 {
		t.Fatalf("part after duplicate negative submission: stock=%d err=%v, want 0", p.Stock, err)
	}
}
