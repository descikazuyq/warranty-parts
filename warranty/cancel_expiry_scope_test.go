package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障取消承诺（Cancel）在调用方传入较晚当前时刻时的作用范围：
// 取消只处理指定的那一笔记录，即使本次时刻已经晚于同备件其他承诺的到期时刻，
// 也不能顺带确认那些承诺到期或释放它们的占用。到期确认仍只由涉及相应承诺的
// 库存查询、请求查询、新预留核算或新使用判断完成（由 expire_test.go、
// part_status_expiry_scope_test.go 等各自保障），取消不是到期确认入口。
//
// 业务主例：同一备件初始实物十二件；甲承诺五件并已领取两件，乙承诺四件且
// 尚未领取，两笔均未取消、均未确认到期。取消前实物十件、有效占用七件、
// 可承诺三件。用晚于两笔到期时刻的当前时刻取消甲，只释放甲未用的三件
// （已领取的两件不回补实物库存），乙的记录与占用保持原样。

// cxPart 等是本文件专用的编号，避免与其他测试文件的编号混用。
const (
	cxPart    = "cx-part1"
	cxReqA    = "cx-rA"
	cxReqB    = "cx-rB"
	cxCommitA = "cx-cA5"
	cxCommitB = "cx-cB4"
	cxMissing = "cx-missing"
)

// cxExpA、cxExpB 是甲乙两笔承诺的到期时刻；cxLate 晚于两者（用于取消），
// cxEarly 早于两者（用于取消后的查看与领取）。全部时刻都在保修期内。
var (
	cxExpA  = t0.Add(20 * day)
	cxExpB  = t0.Add(30 * day)
	cxLate  = t0.Add(40 * day)
	cxEarly = t0.Add(15 * day)
)

// cxStore 构造主例：产品保修 60 天，备件初始库存十二件，两个合格请求；
// 甲承诺五件（已领取两件），乙承诺四件（未领取），两笔均未取消、均未确认
// 到期。返回前断言取消前的账目：实物十件、有效占用七件、可承诺三件。
func cxStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("cx-p1", t0, 60, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(cxPart, 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, id := range []string{cxReqA, cxReqB} {
		if err := s.SubmitRequest(id, "cx-p1", "FAULTY"); err != nil {
			t.Fatalf("submit %s: %v", id, err)
		}
	}
	if _, err := s.Reserve(cxCommitA, cxReqA, cxPart, 5, cxExpA, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", cxCommitA, err)
	}
	if _, err := s.Reserve(cxCommitB, cxReqB, cxPart, 4, cxExpB, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", cxCommitB, err)
	}
	if _, err := s.Use("cx-uA-used2", cxCommitA, 2, nowOK); err != nil {
		t.Fatalf("use 2 from %s: %v", cxCommitA, err)
	}
	st, err := s.PartStatus(cxPart, nowOK)
	if err != nil {
		t.Fatalf("part status before cancel: %v", err)
	}
	assertScopeAccount(t, st, 10, 7, 3)
	return s
}

// TestCancelAtLateTimeDoesNotConfirmOthersExpiry 对应主例：用晚于两笔到期
// 时刻的当前时刻取消甲，正常返回已取消记录（原定五件、已用两件、未用三件、
// 原到期时刻），只释放未用三件；乙没有被这次取消确认到期，随后在早于乙
// 到期时刻的当前时刻查看，乙仍占用四件，且能从中合法领取一件。
func TestCancelAtLateTimeDoesNotConfirmOthersExpiry(t *testing.T) {
	s := cxStore(t)

	// 用晚于两笔到期时刻的当前时刻取消甲：正常返回已取消记录，保留原数量
	// 五件、已用两件、未用三件及原到期时刻；取消不确认到期，Expired 保持否。
	canceled, err := s.Cancel(cxCommitA, cxLate)
	if err != nil {
		t.Fatalf("cancel %s: %v", cxCommitA, err)
	}
	if !canceled.Canceled || canceled.Expired ||
		canceled.Quantity != 5 || canceled.Used != 2 || canceled.Unused() != 3 ||
		canceled.RequestID != cxReqA || canceled.PartID != cxPart ||
		!canceled.Expiry.Equal(cxExpA) {
		t.Fatalf("cancel result: %+v, want qty=5 used=2 unused=3 canceled=true expired=false expiry=%v",
			canceled, cxExpA)
	}

	// 同一次取消之后，按编号取回乙：仍未取消、未确认到期，数量、归属和
	// 到期时刻不变。
	gotB, err := s.Commitment(cxCommitB)
	if err != nil {
		t.Fatalf("commitment %s: %v", cxCommitB, err)
	}
	if gotB.Canceled || gotB.Expired ||
		gotB.Quantity != 4 || gotB.Used != 0 ||
		gotB.RequestID != cxReqB || gotB.PartID != cxPart ||
		!gotB.Expiry.Equal(cxExpB) {
		t.Fatalf("%s disturbed by cancel of %s: %+v", cxCommitB, cxCommitA, gotB)
	}

	// 乙没有被其他操作确认到期：随后以早于乙到期时刻的当前时刻查看该备件，
	// 乙仍占用四件，账目为实物十件、有效占用四件、可承诺六件；甲显示
	// canceled（取消只释放未用三件，已领取的两件不回到实物库存）。
	st, err := s.PartStatus(cxPart, cxEarly)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	assertScopeAccount(t, st, 10, 4, 6)
	assertScopeDetail(t, mustDetailByID(t, st.Details, cxCommitA), cxReqA, 5, 2, 3, CommitmentCanceled, cxExpA)
	assertScopeDetail(t, mustDetailByID(t, st.Details, cxCommitB), cxReqB, 4, 0, 4, CommitmentActive, cxExpB)

	// 以这个较早时刻、用新的使用编号从乙合法领取一件应成功：账目变为
	// 实物九件、有效占用三件、可承诺六件，乙的已用数量变为一件。
	if _, err := s.Use("cx-uB1", cxCommitB, 1, cxEarly); err != nil {
		t.Fatalf("use 1 from %s at earlier time: %v", cxCommitB, err)
	}
	st2, err := s.PartStatus(cxPart, cxEarly)
	if err != nil {
		t.Fatalf("part status after use: %v", err)
	}
	assertScopeAccount(t, st2, 9, 3, 6)
	assertScopeDetail(t, mustDetailByID(t, st2.Details, cxCommitB), cxReqB, 4, 1, 3, CommitmentActive, cxExpB)

	// 甲的数量和取消结果保持原样。
	gotA, err := s.Commitment(cxCommitA)
	if err != nil {
		t.Fatalf("commitment %s: %v", cxCommitA, err)
	}
	if !gotA.Canceled || gotA.Expired || gotA.Quantity != 5 || gotA.Used != 2 ||
		!gotA.Expiry.Equal(cxExpA) {
		t.Fatalf("%s changed after %s use: %+v", cxCommitA, cxCommitB, gotA)
	}
}

// TestCancelFailuresDoNotConfirmExpiry 保障取消失败时作用范围相同：取消空
// 编号返回 ErrInvalidParam，取消不存在的非空编号返回 ErrNotFound；即使传入
// 晚于乙到期时刻的当前时刻，失败调用也不确认任何已知承诺到期、不扣减实物，
// 不存在的编号不会因此生成承诺。
func TestCancelFailuresDoNotConfirmExpiry(t *testing.T) {
	s := cxStore(t)

	if _, err := s.Cancel("", cxLate); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("cancel empty id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Cancel(cxMissing, cxLate); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown id: got %v, want ErrNotFound", err)
	}

	// 不存在的编号不会因失败取消而生成承诺。
	if _, err := s.Commitment(cxMissing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing commitment after failed cancel: got %v, want ErrNotFound", err)
	}

	// 已知承诺均未确认到期、未取消，实物未被扣减。
	for _, id := range []string{cxCommitA, cxCommitB} {
		c, err := s.Commitment(id)
		if err != nil {
			t.Fatalf("commitment %s: %v", id, err)
		}
		if c.Canceled || c.Expired {
			t.Fatalf("%s closed by failed cancels: %+v", id, c)
		}
	}
	if p, _ := s.Part(cxPart); p.Stock != 10 {
		t.Fatalf("part stock = %d, want 10 (failed cancels must not deduct)", p.Stock)
	}

	// 随后以早于两笔到期时刻的当前时刻查看：两笔仍占用全部未用数量，
	// 账目保持取消前的实物十件、有效占用七件、可承诺三件。
	st, err := s.PartStatus(cxPart, cxEarly)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	assertScopeAccount(t, st, 10, 7, 3)
	assertScopeDetail(t, mustDetailByID(t, st.Details, cxCommitA), cxReqA, 5, 2, 3, CommitmentActive, cxExpA)
	assertScopeDetail(t, mustDetailByID(t, st.Details, cxCommitB), cxReqB, 4, 0, 4, CommitmentActive, cxExpB)
}

// TestCancelDoesNotClearConfirmedExpiry 保障取消不会清除其他承诺已被正常
// 操作确认的到期结果：乙先由库存查询确认到期，之后即使用晚于乙到期时刻的
// 当前时刻取消甲，乙的到期结果保持；随后在早于乙到期时刻用新的使用编号
// 领取乙仍返回 ErrCommitmentClosed。
func TestCancelDoesNotClearConfirmedExpiry(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("cx-p1", t0, 60, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(cxPart, 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, id := range []string{cxReqA, cxReqB} {
		if err := s.SubmitRequest(id, "cx-p1", "FAULTY"); err != nil {
			t.Fatalf("submit %s: %v", id, err)
		}
	}
	// 本例中乙先到期：乙第二十天到期，甲第四十天到期。
	expB := t0.Add(20 * day)
	expA := t0.Add(40 * day)
	if _, err := s.Reserve(cxCommitA, cxReqA, cxPart, 5, expA, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", cxCommitA, err)
	}
	if _, err := s.Reserve(cxCommitB, cxReqB, cxPart, 4, expB, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", cxCommitB, err)
	}
	if _, err := s.Use("cx-uA-used2", cxCommitA, 2, nowOK); err != nil {
		t.Fatalf("use 2 from %s: %v", cxCommitA, err)
	}

	// 取消甲之前，乙先被正常操作（库存查询）确认到期：释放乙未用的四件，
	// 账目变为实物十件、有效占用三件（甲的未用）、可承诺七件。
	confirmAt := expB.Add(time.Hour)
	st, err := s.PartStatus(cxPart, confirmAt)
	if err != nil {
		t.Fatalf("part status confirming %s expiry: %v", cxCommitB, err)
	}
	assertScopeAccount(t, st, 10, 3, 7)
	assertScopeDetail(t, mustDetailByID(t, st.Details, cxCommitB), cxReqB, 4, 0, 4, CommitmentExpired, expB)

	// 用晚于乙到期时刻的当前时刻取消甲：正常取消，但不清除乙的到期结果。
	canceled, err := s.Cancel(cxCommitA, expB.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("cancel %s: %v", cxCommitA, err)
	}
	if !canceled.Canceled || canceled.Quantity != 5 || canceled.Used != 2 || canceled.Unused() != 3 {
		t.Fatalf("cancel result: %+v, want qty=5 used=2 unused=3 canceled=true", canceled)
	}
	gotB, err := s.Commitment(cxCommitB)
	if err != nil {
		t.Fatalf("commitment %s: %v", cxCommitB, err)
	}
	if !gotB.Expired || gotB.Canceled || gotB.Quantity != 4 || gotB.Used != 0 {
		t.Fatalf("%s expiry cleared by cancel of %s: %+v", cxCommitB, cxCommitA, gotB)
	}

	// 之后用新的使用编号、在早于乙到期时刻的当前时刻领取乙：已确认的到期
	// 不可逆，仍返回 ErrCommitmentClosed，且不扣减任何数量。
	if _, err := s.Use("cx-uB-late", cxCommitB, 1, cxEarly); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use %s after confirmed expiry: got %v, want ErrCommitmentClosed", cxCommitB, err)
	}
	st2, err := s.PartStatus(cxPart, cxEarly)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	// 甲已取消、乙已确认到期：有效占用为零，实物仍是十件。
	assertScopeAccount(t, st2, 10, 0, 10)
	assertScopeDetail(t, mustDetailByID(t, st2.Details, cxCommitA), cxReqA, 5, 2, 3, CommitmentCanceled, expA)
	assertScopeDetail(t, mustDetailByID(t, st2.Details, cxCommitB), cxReqB, 4, 0, 4, CommitmentExpired, expB)
}
