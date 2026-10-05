package warranty

import (
	"errors"
	"math"
	"testing"
	"time"
)

// 本文件回归保障合法大数量下的库存约束：同一种备件被多个合格保修请求预留，
// 数量接近当前运行环境 int 可表示的最大正整数（记为 M）时，可承诺数量仍按
// 真实账目核算，不能因累计数量回绕而放行超出可承诺数量的预留。初始库存 M、
// 预留 M−2、M−1、2、1 件都是登记与预留公开入口接受的合法非负/正整数，这项
// 保障不通过新增较小上限或调整公开数量类型来回避大数条件。所有请求都关联
// 已登记产品、操作在保修期内、故障不在除外清单、承诺到期时刻晚于操作时刻，
// 资格拒绝与到期释放不混入这项数量规则。测试只沿用现有登记、预留、查询与
// 历史入口的公开行为，不增加新的业务操作。

// 本文件专用编号，避免与其他测试文件混用。
const (
	mxProductID   = "p-mx"
	mxPartID      = "part-mx"
	mxReqA        = "r-mx-a"
	mxReqB        = "r-mx-b"
	mxCommitFirst = "c-mx-1" // 第一笔成功预留：M−2 件
	mxCommitOver  = "c-mx-2" // 超量申请：M−1 件，必须库存不足
	mxCommitExact = "c-mx-3" // 恰好用尽：2 件，成功
	mxCommitExtra = "c-mx-4" // 用尽后再申请：1 件，必须库存不足
)

// mxM 是当前运行环境 int 可表示的最大正整数。
const mxM = math.MaxInt

// mxClock 汇集本文件的全部时刻：操作在保修期内，承诺到期晚于操作时刻。
type mxClock struct {
	now    time.Time // 全部预留提交的当前时刻，保修期内
	expiry time.Time // 全部承诺的到期时刻，晚于 now
}

// mxScenario 构造大数量主例：产品保修三十天、除外清单不含 NOISE，备件初始
// 库存 M 件；两个请求都关联该产品且故障代码为 NOISE，全程合格。
func mxScenario(t *testing.T) (*Store, mxClock) {
	t.Helper()
	clk := mxClock{
		now:    t0.Add(10 * day),
		expiry: t0.Add(20 * day),
	}

	s := NewStore()
	if err := s.RegisterProduct(mxProductID, t0, 30, []string{"FLOOD"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(mxPartID, mxM); err != nil {
		t.Fatalf("register part with max-int stock: %v", err)
	}
	if err := s.SubmitRequest(mxReqA, mxProductID, "NOISE"); err != nil {
		t.Fatalf("submit request a: %v", err)
	}
	if err := s.SubmitRequest(mxReqB, mxProductID, "NOISE"); err != nil {
		t.Fatalf("submit request b: %v", err)
	}
	return s, clk
}

// assertMxAccount 断言按备件查询的账目恰为给定的实物剩余、有效占用与可承诺
// 数量——不能是负数、零库存或其他状态。
func assertMxAccount(t *testing.T, s *Store, now time.Time, phys, occupied, committable int) *PartStatus {
	t.Helper()
	st, err := s.PartStatus(mxPartID, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != phys || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("account = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, phys, occupied, committable)
	}
	return st
}

// assertMxBasis 断言历史记录的库存依据快照恰为拒绝或新增占用之前的真实账目。
func assertMxBasis(t *testing.T, rec HistoryRecord, phys, occupied, committable int) {
	t.Helper()
	if rec.StockBasis == nil {
		t.Fatalf("record %+v: expected stock basis snapshot", rec)
	}
	b := rec.StockBasis
	if b.PhysicalRemaining != phys || b.ActiveOccupied != occupied || b.Committable != committable {
		t.Fatalf("record %q stock basis = phys %d / occupied %d / committable %d, want %d/%d/%d",
			rec.CommitID, b.PhysicalRemaining, b.ActiveOccupied, b.Committable, phys, occupied, committable)
	}
}

// assertMxFirstCommitment 断言第一笔承诺保持原定数量 M−2、归属 r-mx-a、
// 已用数量为零、到期时刻不变，未被后续失败或成功的提交改变。
func assertMxFirstCommitment(t *testing.T, s *Store, clk mxClock) {
	t.Helper()
	c, err := s.Commitment(mxCommitFirst)
	if err != nil {
		t.Fatalf("first commitment: %v", err)
	}
	if c.RequestID != mxReqA || c.PartID != mxPartID || c.Quantity != mxM-2 ||
		c.Used != 0 || c.Unused() != mxM-2 || c.Canceled || c.Expired ||
		!c.Expiry.Equal(clk.expiry) {
		t.Fatalf("first commitment altered: %+v", c)
	}
}

// TestReserveNearMaxIntCommittableLimit 主例：初始库存 M，先成功预留 M−2 件，
// 账目为实物 M、有效占用 M−2、可承诺 2；再由另一合格请求申请 M−1 件——数量
// 单独可由 int 表示，但超过只剩的 2 件可承诺数量，必须返回
// ErrInsufficientStock 而不是参数非法，也不能因累计数量回绕而成功。失败不
// 生成承诺、不改变原承诺，失败请求的历史保留拒绝前的真实账目。随后同一请求
// 申请恰好 2 件成功，两笔有效承诺未用数量合计 M、可承诺为零；再申请 1 件仍
// 库存不足，不留第三笔承诺。
func TestReserveNearMaxIntCommittableLimit(t *testing.T) {
	s, clk := mxScenario(t)

	// 第一步：成功预留 M−2 件。返回的承诺已用数量为零、未用数量为 M−2。
	first, err := s.Reserve(mxCommitFirst, mxReqA, mxPartID, mxM-2, clk.expiry, clk.now)
	if err != nil {
		t.Fatalf("reserve M-2: %v", err)
	}
	if first.Quantity != mxM-2 || first.Used != 0 || first.Unused() != mxM-2 ||
		first.RequestID != mxReqA || first.PartID != mxPartID {
		t.Fatalf("first reserve result = %+v, want quantity M-2 unused", first)
	}

	// 库存查询：实物剩余 M、有效占用 M−2、可承诺 2；明细保留原定数量与
	// 未用数量，已用数量为零。
	st := assertMxAccount(t, s, clk.now, mxM, mxM-2, 2)
	if len(st.Details) != 1 {
		t.Fatalf("details = %+v, want only the first commitment", st.Details)
	}
	d := st.Details[0]
	if d.CommitmentID != mxCommitFirst || d.RequestID != mxReqA ||
		d.OriginalQuantity != mxM-2 || d.UsedQuantity != 0 || d.RemainingQuantity != mxM-2 ||
		d.Status != CommitmentActive || !d.Expiry.Equal(clk.expiry) {
		t.Fatalf("first detail = %+v, want original/remaining M-2, used 0, active", d)
	}

	// 第二步：另一合格请求用新承诺编号申请 M−1 件。M−1 单独可由 int 表示，
	// 但超过只剩的 2 件可承诺数量：必须返回 ErrInsufficientStock，不能返回
	// 参数非法，也不能因累计数量回绕而成功。
	if _, err := s.Reserve(mxCommitOver, mxReqB, mxPartID, mxM-1, clk.expiry, clk.now); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve M-1 over committable: got %v, want ErrInsufficientStock", err)
	} else if errors.Is(err, ErrInvalidParam) {
		t.Fatalf("reserve M-1: got %v, must not be ErrInvalidParam for a representable positive quantity", err)
	}

	// 失败不生成承诺。
	if _, err := s.Commitment(mxCommitOver); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected commitment %q exists: err=%v", mxCommitOver, err)
	}
	// 失败不改变原承诺的数量、归属与到期时刻。
	assertMxFirstCommitment(t, s, clk)
	// 查询仍只有先前的成功占用：实物 M、占用 M−2、可承诺 2，明细只有第一笔。
	st = assertMxAccount(t, s, clk.now, mxM, mxM-2, 2)
	if len(st.Details) != 1 || st.Details[0].CommitmentID != mxCommitFirst {
		t.Fatalf("details after rejection = %+v, want only the first commitment", st.Details)
	}

	// 失败请求的历史：本次库存不足记录保留拒绝前的真实账目（实物 M、占用
	// M−2、可承诺 2），不是负数、零库存或失败之后的其他状态；资格为合格。
	histB, err := s.RequestHistory(mxReqB)
	if err != nil {
		t.Fatalf("request b history: %v", err)
	}
	if len(histB) != 1 {
		t.Fatalf("request b history length = %d, want 1", len(histB))
	}
	rec := histB[0]
	if rec.Success || rec.Error != HistoryErrorInsufficientStock ||
		rec.CommitID != mxCommitOver || rec.PartID != mxPartID || rec.Quantity != mxM-1 ||
		!rec.Expiry.Equal(clk.expiry) || !rec.Now.Equal(clk.now) {
		t.Fatalf("rejection record = %+v, want insufficient_stock for M-1", rec)
	}
	if rec.Eligibility == nil || !rec.Eligibility.Eligible || len(rec.Eligibility.Reasons) != 0 {
		t.Fatalf("rejection record eligibility = %+v, want eligible", rec.Eligibility)
	}
	assertMxBasis(t, rec, mxM, mxM-2, 2)

	// 第三步：同一请求改用新承诺编号申请恰好 2 件，成功。
	exact, err := s.Reserve(mxCommitExact, mxReqB, mxPartID, 2, clk.expiry, clk.now)
	if err != nil {
		t.Fatalf("reserve exact 2: %v", err)
	}
	if exact.Quantity != 2 || exact.Used != 0 || exact.Unused() != 2 || exact.RequestID != mxReqB {
		t.Fatalf("exact reserve result = %+v, want quantity 2 unused for request b", exact)
	}

	// 两笔有效承诺的未用数量合计为 M，实物库存仍为 M，可承诺数量为零。
	st = assertMxAccount(t, s, clk.now, mxM, mxM, 0)
	if len(st.Details) != 2 {
		t.Fatalf("details = %+v, want two active commitments", st.Details)
	}
	unusedSum := 0
	for _, d := range st.Details {
		if d.Status != CommitmentActive || d.UsedQuantity != 0 {
			t.Fatalf("detail = %+v, want active with used 0", d)
		}
		unusedSum += d.RemainingQuantity
	}
	if unusedSum != mxM {
		t.Fatalf("unused sum = %d, want M = %d", unusedSum, mxM)
	}

	// 第四步：可承诺数量已为零，再用新编号申请 1 件仍返回库存不足，
	// 不能留下第三笔承诺。
	if _, err := s.Reserve(mxCommitExtra, mxReqB, mxPartID, 1, clk.expiry, clk.now); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve 1 after exhaustion: got %v, want ErrInsufficientStock", err)
	}
	if _, err := s.Commitment(mxCommitExtra); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected commitment %q exists: err=%v", mxCommitExtra, err)
	}
	st = assertMxAccount(t, s, clk.now, mxM, mxM, 0)
	if len(st.Details) != 2 {
		t.Fatalf("details after final rejection = %+v, want still two commitments", st.Details)
	}
	assertMxFirstCommitment(t, s, clk)

	// 成功记录的库存依据保留新增占用之前的数值：第一笔是实物 M、占用 0、
	// 可承诺 M；恰好 2 件那笔是实物 M、占用 M−2、可承诺 2。失败记录同样
	// 保留当次拒绝前的真实账目。
	histA, err := s.RequestHistory(mxReqA)
	if err != nil {
		t.Fatalf("request a history: %v", err)
	}
	if len(histA) != 1 || !histA[0].Success || histA[0].Error != "" ||
		histA[0].CommitID != mxCommitFirst || histA[0].Quantity != mxM-2 {
		t.Fatalf("request a history = %+v, want single success of M-2", histA)
	}
	assertMxBasis(t, histA[0], mxM, 0, mxM)

	histB, err = s.RequestHistory(mxReqB)
	if err != nil {
		t.Fatalf("request b history: %v", err)
	}
	if len(histB) != 3 {
		t.Fatalf("request b history length = %d, want 3 (fail, success, fail)", len(histB))
	}
	for i, rec := range histB {
		if rec.Seq != i+1 {
			t.Fatalf("record %d seq = %d, want %d", i, rec.Seq, i+1)
		}
	}
	if histB[1].Success != true || histB[1].Error != "" ||
		histB[1].CommitID != mxCommitExact || histB[1].Quantity != 2 {
		t.Fatalf("second record = %+v, want success of exactly 2", histB[1])
	}
	assertMxBasis(t, histB[1], mxM, mxM-2, 2)
	if histB[2].Success || histB[2].Error != HistoryErrorInsufficientStock ||
		histB[2].CommitID != mxCommitExtra || histB[2].Quantity != 1 {
		t.Fatalf("third record = %+v, want insufficient_stock for 1", histB[2])
	}
	assertMxBasis(t, histB[2], mxM, mxM, 0)

	// 按请求和按备件查看的成功承诺归属与数量一致。
	viewA, err := s.RequestView(mxReqA, clk.now)
	if err != nil {
		t.Fatalf("request a view: %v", err)
	}
	if len(viewA.Commitments) != 1 || viewA.Commitments[0].CommitmentID != mxCommitFirst ||
		viewA.Commitments[0].OriginalQuantity != mxM-2 || viewA.Commitments[0].RemainingQuantity != mxM-2 {
		t.Fatalf("request a commitments = %+v, want the M-2 commitment", viewA.Commitments)
	}
	viewB, err := s.RequestView(mxReqB, clk.now)
	if err != nil {
		t.Fatalf("request b view: %v", err)
	}
	if len(viewB.Commitments) != 1 || viewB.Commitments[0].CommitmentID != mxCommitExact ||
		viewB.Commitments[0].OriginalQuantity != 2 || viewB.Commitments[0].RemainingQuantity != 2 {
		t.Fatalf("request b commitments = %+v, want the 2-unit commitment", viewB.Commitments)
	}
	byID := make(map[string]CommitmentDetail, len(st.Details))
	for _, d := range st.Details {
		byID[d.CommitmentID] = d
	}
	for _, view := range []*RequestView{viewA, viewB} {
		for _, c := range view.Commitments {
			d, ok := byID[c.CommitmentID]
			if !ok || d.RequestID != c.RequestID || d.OriginalQuantity != c.OriginalQuantity ||
				d.RemainingQuantity != c.RemainingQuantity || d.UsedQuantity != c.UsedQuantity {
				t.Fatalf("request view %+v inconsistent with part detail %+v", c, d)
			}
		}
	}
}
