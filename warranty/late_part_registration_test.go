package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障“产品与请求已就绪、备件资料稍后登记”：产品资料完整
// （各次操作时购买时刻已到、保修期未结束、故障未被除外），预留到期时刻
// 也都晚于当次当前时刻，但申请的备件编号尚未登记时，首次预留返回
// ErrNotFound，不自动创建备件或承诺，失败也不占用承诺编号。原请求仍可
// 查询，资格仍然合格，关联承诺列表为空。失败在原请求下保留一条
// part_not_found 历史，记录提交的承诺编号、备件编号、数量、到期时刻和
// 当次当前时刻；即使产品资料完整，这条提前失败的记录资格依据与库存依据
// 均为空——不填成合格资格，也不把尚未登记的备件解释成零库存。随后为原
// 备件编号登记库存，登记本身不替之前的申请预留数量；原请求无需重新提交，
// 沿用原承诺编号、原备件、原数量和原到期时刻重试即可成功，历史依次保留
// 旧失败和本次成功，旧记录不被覆盖或补写。另保留补登库存不足的边界：
// 同样先被拒绝，随后登记的库存少于申请数量时，沿用原提交内容预留返回
// ErrInsufficientStock，不创建承诺、不部分占用，历史追加 insufficient_stock
// 并保存合格资格与当次真实库存依据。测试只沿用既有公开入口与现有错误
// 类别，不改变任何规则。

// 本文件专用编号，避免与其他测试文件混用。
const (
	lprProductID = "p-latepart"
	lprPartID    = "part-latepart"
	lprReqID     = "r-latepart"
	lprCommitID  = "c-latepart"
	lprFaultCode = "NOISE"
)

// lprClock 汇集主例的全部时刻。
type lprClock struct {
	purchase time.Time // 产品的购买时刻
	deadline time.Time // 保修截止时刻：购买时刻起第三十天
	firstAt  time.Time // 备件缺失期间首次预留的当次当前时刻
	expiry   time.Time // 各次预留共用的到期时刻，晚于各次当前时刻
	secondAt time.Time // 补登备件后重试预留的当次当前时刻
}

// lprBaseScenario 构造用户给出的主例：产品 p-latepart 已登记（购买时刻
// t0、保修 30 天、除外清单不含 NOISE），请求 r-latepart 已提交并关联该
// 产品，故障代码 NOISE 非空且未被除外。此刻备件 part-latepart 尚未登记。
func lprBaseScenario(t *testing.T) (*Store, lprClock) {
	t.Helper()
	clk := lprClock{
		purchase: t0,
		deadline: t0.Add(30 * day),
		firstAt:  t0.Add(10 * day),
		expiry:   t0.Add(45 * day),
		secondAt: t0.Add(11 * day),
	}

	s := NewStore()
	// 产品资料完整：购买时刻不晚于各次操作时刻、保修期在各次时刻均未结束、
	// 除外清单不含 NOISE。
	if err := s.RegisterProduct(lprProductID, clk.purchase, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.SubmitRequest(lprReqID, lprProductID, lprFaultCode); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	return s, clk
}

// assertEligible 断言原请求在指定时刻资格合格：无拒绝原因、未命中除外，
// 资格依据对应已登记的产品条款。
func assertEligible(t *testing.T, e *Eligibility, clk lprClock) {
	t.Helper()
	if e == nil {
		t.Fatal("eligibility must not be nil")
	}
	if !e.Eligible || e.Excluded || len(e.Reasons) != 0 {
		t.Fatalf("expected eligible, got %+v", e)
	}
	if e.RequestID != lprReqID || e.ProductID != lprProductID || e.FaultCode != lprFaultCode {
		t.Fatalf("eligibility identity = %q/%q/%q, want %q/%q/%q",
			e.RequestID, e.ProductID, e.FaultCode, lprReqID, lprProductID, lprFaultCode)
	}
	if !e.PurchaseTime.Equal(clk.purchase) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(clk.deadline) {
		t.Fatalf("eligibility basis = (%v, %d, %v), want registered terms (%v, 30, %v)",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, clk.purchase, clk.deadline)
	}
}

// assertStock 断言备件账目为给定的实物剩余、有效占用与可承诺数量。
func assertLprStock(t *testing.T, s *Store, now time.Time, phys, occupied, committable int) {
	t.Helper()
	st, err := s.PartStatus(lprPartID, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != phys || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, phys, occupied, committable)
	}
}

// assertMissingPartFailureRecord 断言备件缺失失败记录的完整内容：保存提交
// 的承诺编号、备件编号、三件数量、到期时刻与当次当前时刻；即使产品资料
// 完整、资格实际合格，这条提前失败的记录资格依据与库存依据也都为空——
// 不填成合格资格，也不把尚未登记的备件解释成零库存。
func assertMissingPartFailureRecord(t *testing.T, rec HistoryRecord, seq int, clk lprClock) {
	t.Helper()
	if rec.Seq != seq {
		t.Fatalf("seq = %d, want %d", rec.Seq, seq)
	}
	if rec.Success {
		t.Fatalf("missing-part record marked success: %+v", rec)
	}
	if rec.Error != HistoryErrorPartNotFound {
		t.Fatalf("error category = %q, want %q", rec.Error, HistoryErrorPartNotFound)
	}
	if rec.CommitID != lprCommitID || rec.PartID != lprPartID || rec.Quantity != 3 {
		t.Fatalf("submission content = %q/%q/%d, want %q/%q/3",
			rec.CommitID, rec.PartID, rec.Quantity, lprCommitID, lprPartID)
	}
	if !rec.Expiry.Equal(clk.expiry) || !rec.Now.Equal(clk.firstAt) {
		t.Fatalf("times = expiry %v now %v, want %v / %v",
			rec.Expiry, rec.Now, clk.expiry, clk.firstAt)
	}
	// 即使产品资料完整，这条提前失败的记录也不能填成合格资格。
	if rec.Eligibility != nil {
		t.Fatalf("missing-part record must have nil eligibility, got %+v", rec.Eligibility)
	}
	// 尚未登记的备件不能解释成零库存：库存依据明确为空。
	if rec.StockBasis != nil {
		t.Fatalf("missing-part record must have nil stock basis, got %+v", rec.StockBasis)
	}
}

// TestReserveBeforePart_MissingPeriod 备件缺失期间：产品资料完整、资格
// 合格、到期时刻晚于当次时刻，首次预留三件仍返回 ErrNotFound，不自动
// 创建备件或承诺，失败也不占用承诺编号。原请求仍可查询，资格仍然合格，
// 关联承诺列表为空；历史留下一条 part_not_found 记录，资格依据与库存
// 依据均为空。
func TestReserveBeforePart_MissingPeriod(t *testing.T) {
	s, clk := lprBaseScenario(t)

	// 备件确实尚未登记。
	if _, err := s.Part(lprPartID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("part should be missing: got %v", err)
	}

	// 产品资料完整：资格合格，依据对应已登记条款。
	e, err := s.Evaluate(lprReqID, clk.firstAt)
	if err != nil {
		t.Fatalf("evaluate while part missing: %v", err)
	}
	assertEligible(t, e, clk)

	// 预留 3 件，到期时刻晚于当次时刻：因备件缺失返回 ErrNotFound。
	if _, err := s.Reserve(lprCommitID, lprReqID, lprPartID, 3, clk.expiry, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while part missing: got %v, want ErrNotFound", err)
	}

	// 失败不能自动创建备件或承诺：备件仍不存在，承诺编号未被占用。
	if _, err := s.Part(lprPartID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve registered the part: got %v", err)
	}
	if _, err := s.Commitment(lprCommitID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve occupied the commit id: got %v", err)
	}

	// 原请求仍可查询：资格仍然合格，关联承诺列表为空。
	req, err := s.Request(lprReqID)
	if err != nil {
		t.Fatalf("request lookup: %v", err)
	}
	if req.ProductID != lprProductID || req.FaultCode != lprFaultCode {
		t.Fatalf("saved request = %+v, want product %q fault %q", req, lprProductID, lprFaultCode)
	}
	view, err := s.RequestView(lprReqID, clk.firstAt)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	assertEligible(t, view.Eligibility, clk)
	if len(view.Commitments) != 0 {
		t.Fatalf("failed reserve gained commitments: %+v", view.Commitments)
	}

	// 历史恰为一条 part_not_found 失败记录，资格依据与库存依据均为空。
	h, err := s.RequestHistory(lprReqID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1", len(h))
	}
	assertMissingPartFailureRecord(t, h[0], 1, clk)
}

// TestReserveBeforePart_RegisterStockThenRetrySucceeds 补登五件库存：登记
// 本身不替之前的申请预留数量，此时实物五件、有效占用零件、可承诺五件。
// 原请求无需重新提交，沿用原承诺编号、原备件、三件数量和原到期时刻预留
// 成功；承诺归属原请求，账目变为 5/3/2。历史依次保留旧失败和本次成功，
// 成功记录的库存依据是新增占用前的 5/0/5，旧失败的内容与空依据保持原样；
// 请求的承诺列表只出现这笔成功承诺。
func TestReserveBeforePart_RegisterStockThenRetrySucceeds(t *testing.T) {
	s, clk := lprBaseScenario(t)

	// 备件缺失期间先失败一次，留下 seq=1 的 part_not_found 记录。
	if _, err := s.Reserve(lprCommitID, lprReqID, lprPartID, 3, clk.expiry, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while part missing: got %v, want ErrNotFound", err)
	}

	// 为原备件编号登记五件库存。
	if err := s.RegisterPart(lprPartID, 5); err != nil {
		t.Fatalf("register part: %v", err)
	}

	// 登记本身不替之前的申请预留数量：实物 5、有效占用 0、可承诺 5，
	// 原请求仍无关联承诺。
	assertLprStock(t, s, clk.secondAt, 5, 0, 5)
	view, err := s.RequestView(lprReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view after registration: %v", err)
	}
	assertEligible(t, view.Eligibility, clk)
	if len(view.Commitments) != 0 {
		t.Fatalf("registration reserved for the earlier request: %+v", view.Commitments)
	}

	// 原请求无需重新提交：以原承诺编号、原备件、三件数量和原到期时刻
	// 预留，到期时刻仍晚于本次时刻，应成功。
	c, err := s.Reserve(lprCommitID, lprReqID, lprPartID, 3, clk.expiry, clk.secondAt)
	if err != nil {
		t.Fatalf("retry reserve after part registration: %v", err)
	}
	if c.ID != lprCommitID || c.RequestID != lprReqID || c.PartID != lprPartID ||
		c.Quantity != 3 || c.Used != 0 || c.Canceled || c.Expired {
		t.Fatalf("bad commitment: %+v", c)
	}
	if !c.Expiry.Equal(clk.expiry) {
		t.Fatalf("commitment expiry = %v, want %v", c.Expiry, clk.expiry)
	}

	// 账目变为实物 5、有效占用 3、可承诺 2。
	assertLprStock(t, s, clk.secondAt, 5, 3, 2)

	// 请求的承诺列表此时只出现这笔成功承诺，不多出先前失败申请的记录。
	view, err = s.RequestView(lprReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view after success: %v", err)
	}
	if len(view.Commitments) != 1 {
		t.Fatalf("request commitments = %+v, want exactly the successful commitment", view.Commitments)
	}
	d := view.Commitments[0]
	if d.CommitmentID != lprCommitID || d.RequestID != lprReqID || d.PartID != lprPartID ||
		d.OriginalQuantity != 3 || d.UsedQuantity != 0 || d.RemainingQuantity != 3 ||
		d.Status != CommitmentActive {
		t.Fatalf("commitment detail = %+v, want active 3-piece commitment %q", d, lprCommitID)
	}

	// 历史两条：旧失败在前（seq=1），内容与空依据保持原样，不被成功
	// 覆盖或补写；本次成功在后（seq=2）。
	h, err := s.RequestHistory(lprReqID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	assertMissingPartFailureRecord(t, h[0], 1, clk)
	ok := h[1]
	if ok.Seq != 2 || !ok.Success || ok.Error != "" {
		t.Fatalf("second record should be success with seq 2: %+v", ok)
	}
	if ok.CommitID != lprCommitID || ok.PartID != lprPartID || ok.Quantity != 3 ||
		!ok.Expiry.Equal(clk.expiry) || !ok.Now.Equal(clk.secondAt) {
		t.Fatalf("success submission content: %+v", ok)
	}
	// 成功记录保存当次的合格资格依据。
	assertEligible(t, ok.Eligibility, clk)
	// 库存依据是新增占用前的实物 5、占用 0、可承诺 5。
	if ok.StockBasis == nil {
		t.Fatal("success record missing stock basis")
	}
	if ok.StockBasis.PhysicalRemaining != 5 || ok.StockBasis.ActiveOccupied != 0 ||
		ok.StockBasis.Committable != 5 {
		t.Fatalf("success stock basis = %+v, want pre-addition 5/0/5", ok.StockBasis)
	}
}

// TestReserveBeforePart_RegisterInsufficientStockRejects 补登库存不足的
// 边界：备件缺失期间同样先申请三件并被拒绝，随后只登记两件库存，再沿用
// 原提交内容预留返回 ErrInsufficientStock，不创建承诺、不只占用两件作为
// 部分成功。账目保持 2/0/2，历史追加 insufficient_stock，保存合格资格与
// 当次真实库存依据，之前的 part_not_found 记录仍原样保留。
func TestReserveBeforePart_RegisterInsufficientStockRejects(t *testing.T) {
	s, clk := lprBaseScenario(t)

	// 备件缺失期间先申请三件并被拒绝，留下 seq=1 的 part_not_found 记录。
	if _, err := s.Reserve(lprCommitID, lprReqID, lprPartID, 3, clk.expiry, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while part missing: got %v, want ErrNotFound", err)
	}

	// 随后只登记两件库存。
	if err := s.RegisterPart(lprPartID, 2); err != nil {
		t.Fatalf("register part: %v", err)
	}
	assertLprStock(t, s, clk.secondAt, 2, 0, 2)

	// 沿用原提交内容（原承诺编号、原备件、三件、原到期时刻）预留：
	// 可承诺只有两件，返回 ErrInsufficientStock。
	if _, err := s.Reserve(lprCommitID, lprReqID, lprPartID, 3, clk.expiry, clk.secondAt); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("retry reserve with insufficient stock: got %v, want ErrInsufficientStock", err)
	}

	// 不创建承诺、不只占用两件作为部分成功：承诺编号仍未被占用，
	// 账目保持实物 2、有效占用 0、可承诺 2。
	if _, err := s.Commitment(lprCommitID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("insufficient retry created a commitment: got %v", err)
	}
	assertLprStock(t, s, clk.secondAt, 2, 0, 2)
	view, err := s.RequestView(lprReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("insufficient retry gained commitments: %+v", view.Commitments)
	}

	// 历史两条：之前的 part_not_found 记录仍原样保留在前（seq=1）；
	// 本次 insufficient_stock 记录在后（seq=2），保存合格资格与当次
	// 真实库存依据 2/0/2。
	h, err := s.RequestHistory(lprReqID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	assertMissingPartFailureRecord(t, h[0], 1, clk)
	ins := h[1]
	if ins.Seq != 2 || ins.Success || ins.Error != HistoryErrorInsufficientStock {
		t.Fatalf("second record should be insufficient_stock failure with seq 2: %+v", ins)
	}
	if ins.CommitID != lprCommitID || ins.PartID != lprPartID || ins.Quantity != 3 ||
		!ins.Expiry.Equal(clk.expiry) || !ins.Now.Equal(clk.secondAt) {
		t.Fatalf("insufficient submission content: %+v", ins)
	}
	// 产品资料完整：本次失败记录保存当次的合格资格依据。
	assertEligible(t, ins.Eligibility, clk)
	// 库存依据是当次真实账目：实物 2、占用 0、可承诺 2。
	if ins.StockBasis == nil {
		t.Fatal("insufficient record missing stock basis")
	}
	if ins.StockBasis.PhysicalRemaining != 2 || ins.StockBasis.ActiveOccupied != 0 ||
		ins.StockBasis.Committable != 2 {
		t.Fatalf("insufficient stock basis = %+v, want 2/0/2", ins.StockBasis)
	}
}
