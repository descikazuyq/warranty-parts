package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障预留失败历史（RequestHistory）的拒绝事实归属：每次历史查询
// 返回的记录列表、资格依据快照（含已经有内容的拒绝原因列表）与库存依据快照
// 都是属于调用方的副本。调用方拿到失败记录后可以为了展示任意整理自己那份
// 内容——改写拒绝原因、合格标记、处理结果、提交数量、库存依据，删除或补入
// 列表记录——但这些编辑只能影响这一份数据，不能覆盖或丢失仓库保存的拒绝
// 事实：另一份此前取回的结果、重新查询的历史、按请求的资格与承诺查询、按
// 备件的库存账目都继续反映真实处理结果，失败不能变成成功，被拒绝的数量不
// 能进入占用，仓库保存的提交次数也不随调用方列表的增删而增减。这些测试只
// 沿用登记、预留与查询等既有公开行为与现有的预留错误类别、依据快照和按请求
// 查询约定，不改变任何资格规则、库存规则和错误行为。

// 本文件专用编号，避免与其他测试文件混用。
const (
	hrProductID = "p-hrej"
	hrPartID    = "part-hrej"
	hrOKReq     = "r-ok"
	hrBadReq    = "r-bad"
	hrEmptyReq  = "r-empty"
	hrOKCommit  = "c-ok"
	hrBadCommit = "c-bad"
)

// hrClock 汇集主例的全部时刻。
type hrClock struct {
	deadline  time.Time // 保修截止时刻：购买时刻起第三十天
	okAt      time.Time // 保修期内第一次预留的当前时刻
	okExpiry  time.Time // 第一次预留的承诺到期时刻，晚于随后查询与第二次预留的时刻
	badAt     time.Time // 产品已过保后第二次预留的当前时刻
	badExpiry time.Time // 第二次预留给定的到期时刻，本身有效（晚于 badAt）
}

// hrScenario 构造用户给出的主例：产品保修三十天，BROKEN_SEAL 在除外故障
// 代码中，备件初始库存十件。先在保修期内为未命中除外代码的 r-ok（故障
// NOISE）预留四件，承诺到期晚于随后的查询与第二次预留时刻；再在产品已过
// 保后，为命中 BROKEN_SEAL 的 r-bad 预留两件，到期参数本身有效，该次预留
// 返回 ErrIneligible。另有一个已登记但没有任何预留记录的 r-empty。
func hrScenario(t *testing.T) (*Store, hrClock) {
	t.Helper()
	clk := hrClock{
		deadline:  t0.Add(30 * day),
		okAt:      t0.Add(10 * day),
		okExpiry:  t0.Add(45 * day),
		badAt:     t0.Add(31 * day),
		badExpiry: t0.Add(40 * day),
	}

	s := NewStore()
	if err := s.RegisterProduct(hrProductID, t0, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(hrPartID, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(hrOKReq, hrProductID, "NOISE"); err != nil {
		t.Fatalf("submit ok request: %v", err)
	}
	if err := s.SubmitRequest(hrBadReq, hrProductID, "BROKEN_SEAL"); err != nil {
		t.Fatalf("submit bad request: %v", err)
	}
	if err := s.SubmitRequest(hrEmptyReq, hrProductID, "NOISE"); err != nil {
		t.Fatalf("submit empty request: %v", err)
	}

	// 保修期内为未命中除外代码的请求预留四件，成功占用。
	if _, err := s.Reserve(hrOKCommit, hrOKReq, hrPartID, 4, clk.okExpiry, clk.okAt); err != nil {
		t.Fatalf("reserve within warranty: %v", err)
	}
	// 产品已过保且命中除外代码：返回 ErrIneligible，到期参数本身有效。
	if _, err := s.Reserve(hrBadCommit, hrBadReq, hrPartID, 2, clk.badExpiry, clk.badAt); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve expired+excluded: got %v, want ErrIneligible", err)
	}
	return s, clk
}

// assertRejectionRecord 断言一条历史记录完整保留主例中的拒绝事实：归属
// r-bad 的次序号一、提交内容为 c-bad/part-hrej/两件与给定时刻，处理结果
// 是类别为 ineligible 的失败；资格依据仍是登记的购买时刻、保修三十天与
// 保修截止时刻，不合格且命中除外，拒绝原因恰好为过保与故障除外两项且
// 次序不变；库存依据为处理前实物十件、有效占用四件、可承诺六件。
func assertRejectionRecord(t *testing.T, rec HistoryRecord, clk hrClock) {
	t.Helper()
	if rec.Seq != 1 {
		t.Fatalf("seq = %d, want 1", rec.Seq)
	}
	if rec.Success {
		t.Fatalf("failure record became success: %+v", rec)
	}
	if rec.Error != HistoryErrorIneligible {
		t.Fatalf("error category = %q, want %q", rec.Error, HistoryErrorIneligible)
	}
	if rec.CommitID != hrBadCommit || rec.PartID != hrPartID || rec.Quantity != 2 {
		t.Fatalf("submission content = %q/%q/%d, want %q/%q/2",
			rec.CommitID, rec.PartID, rec.Quantity, hrBadCommit, hrPartID)
	}
	if !rec.Expiry.Equal(clk.badExpiry) || !rec.Now.Equal(clk.badAt) {
		t.Fatalf("times = expiry %v now %v, want %v / %v", rec.Expiry, rec.Now, clk.badExpiry, clk.badAt)
	}

	e := rec.Eligibility
	if e == nil {
		t.Fatal("expected eligibility snapshot on rejected record")
	}
	if e.RequestID != hrBadReq || e.ProductID != hrProductID || e.FaultCode != "BROKEN_SEAL" {
		t.Fatalf("eligibility identity = %q/%q/%q, want %q/%q/BROKEN_SEAL",
			e.RequestID, e.ProductID, e.FaultCode, hrBadReq, hrProductID)
	}
	if e.Eligible {
		t.Fatalf("eligible = true, want false: %+v", e)
	}
	if !e.Excluded {
		t.Fatalf("excluded = false, want true: %+v", e)
	}
	if len(e.Reasons) != 2 || e.Reasons[0] != ReasonWarrantyExpired || e.Reasons[1] != ReasonFaultExcluded {
		t.Fatalf("reasons = %v, want exactly [warranty_expired fault_code_excluded]", e.Reasons)
	}
	if !e.PurchaseTime.Equal(t0) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(clk.deadline) {
		t.Fatalf("registered eligibility basis altered: purchase=%v days=%d expiry=%v",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry)
	}

	b := rec.StockBasis
	if b == nil {
		t.Fatal("expected stock basis snapshot on rejected record")
	}
	if b.PhysicalRemaining != 10 || b.ActiveOccupied != 4 || b.Committable != 6 {
		t.Fatalf("stock basis = phys %d / occupied %d / committable %d, want 10/4/6",
			b.PhysicalRemaining, b.ActiveOccupied, b.Committable)
	}
}

// assertRejectionState 断言仓库真实状态保留拒绝事实：被拒绝的两件没有成为
// 承诺，也没有进入占用——备件账目始终是实物十件、有效占用四件、可承诺六件，
// 明细只有 r-ok 那笔四件的有效承诺；直接查询 r-bad 的资格仍得到过保与故障
// 除外两项原因，按请求查看没有任何关联承诺。
func assertRejectionState(t *testing.T, s *Store, clk hrClock) {
	t.Helper()
	if _, err := s.Commitment(hrBadCommit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected commitment %q exists: err=%v", hrBadCommit, err)
	}

	st, err := s.PartStatus(hrPartID, clk.badAt)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 4 || st.Committable != 6 {
		t.Fatalf("stock after rejection = phys %d / occupied %d / committable %d, want 10/4/6",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if len(st.Details) != 1 {
		t.Fatalf("part details = %+v, want only the four-unit commitment", st.Details)
	}
	d := st.Details[0]
	if d.CommitmentID != hrOKCommit || d.RequestID != hrOKReq || d.OriginalQuantity != 4 ||
		d.UsedQuantity != 0 || d.RemainingQuantity != 4 || d.Status != CommitmentActive ||
		!d.Expiry.Equal(clk.okExpiry) {
		t.Fatalf("only commitment detail altered: %+v", d)
	}

	e, err := s.Evaluate(hrBadReq, clk.badAt)
	if err != nil {
		t.Fatalf("evaluate rejected request: %v", err)
	}
	if e.Eligible || !e.Excluded || len(e.Reasons) != 2 ||
		e.Reasons[0] != ReasonWarrantyExpired || e.Reasons[1] != ReasonFaultExcluded {
		t.Fatalf("direct eligibility = %+v, want ineligible with both rejection reasons", e)
	}

	view, err := s.RequestView(hrBadReq, clk.badAt)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if view.Eligibility == nil || view.Eligibility.Eligible ||
		len(view.Eligibility.Reasons) != 2 ||
		view.Eligibility.Reasons[0] != ReasonWarrantyExpired ||
		view.Eligibility.Reasons[1] != ReasonFaultExcluded {
		t.Fatalf("view eligibility = %+v, want both rejection reasons", view.Eligibility)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("rejected request gained commitments: %+v", view.Commitments)
	}
}

// TestRejectedHistoryFactSurvivesCallerEdits 主例：过保且命中 BROKEN_SEAL
// 的两件预留返回 ErrIneligible。对失败记录分别取回两份历史，调用方把其中
// 一份已有的拒绝原因改成其他原因，并改动合格标记、处理结果、提交数量和
// 库存依据，再在自己的列表里补入和删除记录；另一份此前取回的结果以及
// 重新查询的历史都必须保留原事实——请求归属、提交内容、次序号和两项拒绝
// 原因不被覆盖或丢失，失败不变成成功，提交次数不增不减，资格与备件查询
// 也不能因为编辑返回值而放行或发放备件。
func TestRejectedHistoryFactSurvivesCallerEdits(t *testing.T) {
	s, clk := hrScenario(t)

	// 拒绝事实在任何编辑之前先核对一次。
	assertRejectionState(t, s, clk)

	first, err := s.RequestHistory(hrBadReq)
	if err != nil {
		t.Fatalf("first history: %v", err)
	}
	second, err := s.RequestHistory(hrBadReq)
	if err != nil {
		t.Fatalf("second history: %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("history lengths = %d / %d, want 1 / 1", len(first), len(second))
	}
	assertRejectionRecord(t, first[0], clk)
	assertRejectionRecord(t, second[0], clk)

	// 调用方为了展示任意整理第一份结果：
	rec := &first[0]
	rec.Seq = 99
	rec.CommitID = "c-fake"
	rec.PartID = "part-fake"
	rec.Quantity = 99
	rec.Expiry = t0
	rec.Now = t0
	// 处理结果：失败改写成成功。
	rec.Success = true
	rec.Error = ""
	// 资格依据：就地改写已有拒绝原因底层数组，再翻合格标记、除外标记和归属，
	// 最后把拒绝原因整体替换成另一项原因。
	rec.Eligibility.Reasons[0] = ReasonPurchaseInFuture
	rec.Eligibility.Eligible = true
	rec.Eligibility.Excluded = false
	rec.Eligibility.RequestID = "r-fake"
	rec.Eligibility.ProductID = "p-fake"
	rec.Eligibility.FaultCode = "NOISE"
	rec.Eligibility.PurchaseTime = t0.Add(99 * day)
	rec.Eligibility.WarrantyDays = 365
	rec.Eligibility.WarrantyExpiry = t0.Add(365 * day)
	rec.Eligibility.Reasons = []RejectionReason{ReasonPurchaseInFuture}
	// 库存依据：先就地改字段，再整体换成另一份依据。
	rec.StockBasis.PhysicalRemaining = 100
	rec.StockBasis.ActiveOccupied = 90
	rec.StockBasis.Committable = 10
	rec.StockBasis = &StockBasis{PhysicalRemaining: 1, ActiveOccupied: 1, Committable: 0}
	// 在调用方自己的列表中补入一条伪造的成功记录。
	first = append(first, HistoryRecord{Seq: 2, CommitID: "c-added", Success: true})

	// 另一份此前取回的结果保留全部原事实，包括拒绝原因底层数组。
	if len(second) != 1 {
		t.Fatalf("earlier copy length = %d, want 1", len(second))
	}
	assertRejectionRecord(t, second[0], clk)

	// 重新查询：补入不增加仓库保存的提交次数，原记录仍归属 r-bad 且序号为一。
	again, err := s.RequestHistory(hrBadReq)
	if err != nil {
		t.Fatalf("re-query after append: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("re-query length = %d, want 1 (caller append must not be saved)", len(again))
	}
	assertRejectionRecord(t, again[0], clk)

	// 调用方从自己那份列表删除记录（连同此前补入的伪造记录一起清空）。
	first = first[:0]

	// 删除不减少仓库保存的提交次数。
	again2, err := s.RequestHistory(hrBadReq)
	if err != nil {
		t.Fatalf("re-query after delete: %v", err)
	}
	if len(again2) != 1 {
		t.Fatalf("re-query length = %d, want 1 (caller delete must not remove saved record)", len(again2))
	}
	assertRejectionRecord(t, again2[0], clk)

	// 编辑返回值不能放行资格、不能发放备件，也不能造出成功承诺。
	assertRejectionState(t, s, clk)

	// 其他请求的历史不受影响：r-ok 仍只有第一次预留的成功记录，其库存
	// 依据是那次提交处理前的实物十件、占用零、可承诺十件。
	okHistory, err := s.RequestHistory(hrOKReq)
	if err != nil {
		t.Fatalf("ok request history: %v", err)
	}
	if len(okHistory) != 1 {
		t.Fatalf("ok history length = %d, want 1", len(okHistory))
	}
	okRec := okHistory[0]
	if okRec.Seq != 1 || !okRec.Success || okRec.Error != "" ||
		okRec.CommitID != hrOKCommit || okRec.PartID != hrPartID || okRec.Quantity != 4 ||
		!okRec.Expiry.Equal(clk.okExpiry) || !okRec.Now.Equal(clk.okAt) {
		t.Fatalf("ok success record altered: %+v", okRec)
	}
	if okRec.Eligibility == nil || !okRec.Eligibility.Eligible || okRec.Eligibility.Excluded ||
		len(okRec.Eligibility.Reasons) != 0 ||
		!okRec.Eligibility.PurchaseTime.Equal(t0) || okRec.Eligibility.WarrantyDays != 30 ||
		!okRec.Eligibility.WarrantyExpiry.Equal(clk.deadline) {
		t.Fatalf("ok eligibility snapshot altered: %+v", okRec.Eligibility)
	}
	if okRec.StockBasis == nil || okRec.StockBasis.PhysicalRemaining != 10 ||
		okRec.StockBasis.ActiveOccupied != 0 || okRec.StockBasis.Committable != 10 {
		t.Fatalf("ok stock basis altered: %+v", okRec.StockBasis)
	}
}

// TestEmptyHistoryStaysEmptyAfterCallerAppends 已登记但没有预留记录的请求：
// 取回的历史是空列表；调用方在自己的列表中加入伪造记录后，再次查询仍为
// 空列表，仓库中其他请求的成功与失败历史也不受影响。
func TestEmptyHistoryStaysEmptyAfterCallerAppends(t *testing.T) {
	s, clk := hrScenario(t)

	empty, err := s.RequestHistory(hrEmptyReq)
	if err != nil {
		t.Fatalf("empty history: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("registered request without reserves: got %v, want empty non-nil list", empty)
	}

	// 调用方在自己的空列表中补入伪造的成功与失败记录。
	empty = append(empty,
		HistoryRecord{Seq: 1, CommitID: "c-fake-1", PartID: hrPartID, Quantity: 3, Success: true},
		HistoryRecord{Seq: 2, CommitID: "c-fake-2", PartID: hrPartID, Quantity: 5, Success: false, Error: HistoryErrorIneligible},
	)

	// 再次查询仍为空列表（非 nil），伪造记录没有进入仓库，提交次数为零。
	again, err := s.RequestHistory(hrEmptyReq)
	if err != nil {
		t.Fatalf("re-query empty history: %v", err)
	}
	if again == nil || len(again) != 0 {
		t.Fatalf("empty history gained records: %+v", again)
	}

	// 其他请求的历史不受影响：r-bad 仍是那一条拒绝事实，r-ok 仍是那一条成功。
	badHistory, err := s.RequestHistory(hrBadReq)
	if err != nil {
		t.Fatalf("bad history: %v", err)
	}
	if len(badHistory) != 1 {
		t.Fatalf("bad history length = %d, want 1", len(badHistory))
	}
	assertRejectionRecord(t, badHistory[0], clk)

	okHistory, err := s.RequestHistory(hrOKReq)
	if err != nil {
		t.Fatalf("ok history: %v", err)
	}
	if len(okHistory) != 1 || !okHistory[0].Success || okHistory[0].CommitID != hrOKCommit {
		t.Fatalf("ok history altered: %+v", okHistory)
	}

	// 备件账目也不被伪造记录占用：仍是 10/4/6。
	st, err := s.PartStatus(hrPartID, clk.badAt)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 4 || st.Committable != 6 {
		t.Fatalf("stock after fabricating empty-history records = %d/%d/%d, want 10/4/6",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
}
