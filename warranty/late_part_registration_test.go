package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障“保修请求与产品资料已就绪、备件资料后登记”：产品与请求
// 都已保存且资格合格，但申请的备件编号尚未登记时，首次预留返回
// ErrNotFound——不自动创建备件或承诺，失败也不占用承诺编号；原请求仍可
// 查询、资格仍然合格、关联承诺列表为空。该次失败在原请求下留下一条
// part_not_found 历史，保存提交的承诺编号、备件编号、数量、到期时刻与
// 当次当前时刻，且资格依据与库存依据均为空：即使产品资料完整，这条提前
// 失败的记录也不能填成合格资格，尚未登记的备件不能解释成零库存。随后
// 补登备件库存（登记本身不替之前的申请预留数量），无需重新提交请求，
// 沿用原承诺编号、原备件、原数量与原到期时刻即可成功预留；历史依次保留
// 旧失败与本次成功，旧记录内容与空依据保持原样，不被成功覆盖或补写。
// 另覆盖补登库存不足的边界：备件缺失期间的失败保留后，只登记不足数量
// 的库存再沿用原提交内容预留，返回 ErrInsufficientStock，不创建承诺也
// 不做部分占用，历史追加 insufficient_stock 并保存当次真实依据。测试只
// 沿用既有公开入口与现有错误类别，不改变任何规则。

// 本文件专用编号，避免与其他测试文件混用。
const (
	pltProductID = "p-latepart"
	pltPartID    = "part-latepart"
	pltReqID     = "r-latepart"
	pltCommitID  = "c-latepart"
	pltFaultCode = "NOISE"
)

// pltClock 汇集主例的全部时刻。
type pltClock struct {
	purchase time.Time // 产品的购买时刻
	deadline time.Time // 保修截止时刻：购买时刻起第三十天
	firstAt  time.Time // 备件缺失期间首次预留的当次当前时刻
	secondAt time.Time // 补登库存后重试预留的当次当前时刻
	expiry   time.Time // 两次预留共用的到期时刻，晚于两次当前时刻
}

// pltBaseScenario 构造主例：产品 p-latepart 已登记（购买时刻已到、保修
// 三十天、除外清单不含 NOISE），请求 r-latepart 已保存且故障代码非空；
// 备件 part-latepart 此刻尚未登记。
func pltBaseScenario(t *testing.T) (*Store, pltClock) {
	t.Helper()
	clk := pltClock{
		purchase: t0,
		deadline: t0.Add(30 * day),
		firstAt:  t0.Add(10 * day),
		secondAt: t0.Add(11 * day),
		expiry:   t0.Add(45 * day),
	}

	s := NewStore()
	if err := s.RegisterProduct(pltProductID, clk.purchase, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.SubmitRequest(pltReqID, pltProductID, pltFaultCode); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	// 备件故意不登记，由各个用例在需要的时刻补登。
	return s, clk
}

// assertStock 断言备件账目为给定的实物剩余、有效占用与可承诺数量。
func pltAssertStock(t *testing.T, s *Store, now time.Time, phys, occupied, committable int) {
	t.Helper()
	st, err := s.PartStatus(pltPartID, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != phys || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, phys, occupied, committable)
	}
}

// assertEligible 断言原请求在当次时刻资格合格：购买时刻已到、保修未结束、
// 故障未命中除外，且没有拒绝原因。
func assertEligible(t *testing.T, e *Eligibility, clk pltClock) {
	t.Helper()
	if e == nil {
		t.Fatal("eligibility basis missing")
	}
	if !e.Eligible || e.Excluded || len(e.Reasons) != 0 {
		t.Fatalf("expected eligible without reasons, got %+v", e)
	}
	if e.RequestID != pltReqID || e.ProductID != pltProductID || e.FaultCode != pltFaultCode {
		t.Fatalf("eligibility identity = %q/%q/%q, want %q/%q/%q",
			e.RequestID, e.ProductID, e.FaultCode, pltReqID, pltProductID, pltFaultCode)
	}
	if !e.PurchaseTime.Equal(clk.purchase) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(clk.deadline) {
		t.Fatalf("eligibility basis = (%v, %d, %v), want registered terms (%v, 30, %v)",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, clk.purchase, clk.deadline)
	}
}

// assertMissingPartFailureRecord 断言备件缺失失败记录的完整内容：保存提交
// 的承诺编号、备件、数量、到期时刻与当次当前时刻；即使产品资料完整、资格
// 本来合格，这条提前失败的记录的资格依据与库存依据也都为空——尚未登记的
// 备件不能解释成零库存，也不能回填成合格资格。
func assertMissingPartFailureRecord(t *testing.T, rec HistoryRecord, seq int, clk pltClock) {
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
	if rec.CommitID != pltCommitID || rec.PartID != pltPartID || rec.Quantity != 3 {
		t.Fatalf("submission content = %q/%q/%d, want %q/%q/3",
			rec.CommitID, rec.PartID, rec.Quantity, pltCommitID, pltPartID)
	}
	if !rec.Expiry.Equal(clk.expiry) || !rec.Now.Equal(clk.firstAt) {
		t.Fatalf("times = expiry %v now %v, want %v / %v",
			rec.Expiry, rec.Now, clk.expiry, clk.firstAt)
	}
	// 产品资料完整、资格本来合格，也不能把这条提前失败的记录填成合格资格。
	if rec.Eligibility != nil {
		t.Fatalf("missing-part record must have nil eligibility, got %+v", rec.Eligibility)
	}
	// 尚未登记的备件不能解释成零库存。
	if rec.StockBasis != nil {
		t.Fatalf("missing-part record must have nil stock basis, got %+v", rec.StockBasis)
	}
}

// TestReserveBeforePart_MissingPeriod 备件缺失期间：原请求仍可查询，资格
// 仍然合格，关联承诺列表为空；首次预留三件返回 ErrNotFound，不自动创建
// 备件或承诺，失败不占用承诺编号；历史留下一条 part_not_found 记录，保存
// 提交内容与当次时刻，资格依据与库存依据均为空。
func TestReserveBeforePart_MissingPeriod(t *testing.T) {
	s, clk := pltBaseScenario(t)

	// 请求已保存：取回原产品编号与故障代码。
	req, err := s.Request(pltReqID)
	if err != nil {
		t.Fatalf("request lookup: %v", err)
	}
	if req.ProductID != pltProductID || req.FaultCode != pltFaultCode {
		t.Fatalf("saved request = %+v, want product %q fault %q", req, pltProductID, pltFaultCode)
	}
	// 备件确实尚未登记。
	if _, err := s.Part(pltPartID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("part should be missing: got %v", err)
	}

	// 资格仍然合格：购买时刻已到、保修期未结束、故障未被除外。
	e, err := s.Evaluate(pltReqID, clk.firstAt)
	if err != nil {
		t.Fatalf("evaluate while part missing: %v", err)
	}
	assertEligible(t, e, clk)
	// 按请求查看同样合格，且关联承诺列表为空。
	view, err := s.RequestView(pltReqID, clk.firstAt)
	if err != nil {
		t.Fatalf("request view while part missing: %v", err)
	}
	assertEligible(t, view.Eligibility, clk)
	if len(view.Commitments) != 0 {
		t.Fatalf("no commitments expected before any success, got %+v", view.Commitments)
	}

	// 首次预留三件（数量为正、到期时刻晚于当次时刻，失败原因确实只是备件
	// 未登记）：返回 ErrNotFound，不能自动创建备件或承诺。
	if _, err := s.Reserve(pltCommitID, pltReqID, pltPartID, 3, clk.expiry, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while part missing: got %v, want ErrNotFound", err)
	}
	if _, err := s.Part(pltPartID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve must not auto-register the part: got %v", err)
	}
	// 失败不占用承诺编号。
	if _, err := s.Commitment(pltCommitID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve created a commitment: got %v", err)
	}
	// 失败后原请求仍合格，关联承诺列表仍为空。
	view, err = s.RequestView(pltReqID, clk.firstAt)
	if err != nil {
		t.Fatalf("request view after failed reserve: %v", err)
	}
	assertEligible(t, view.Eligibility, clk)
	if len(view.Commitments) != 0 {
		t.Fatalf("failed reserve gained commitments: %+v", view.Commitments)
	}

	// 历史留下一条 part_not_found 失败记录，依据均为空。
	h, err := s.RequestHistory(pltReqID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1", len(h))
	}
	assertMissingPartFailureRecord(t, h[0], 1, clk)
}

// TestReserveBeforePart_RegisterStockThenRetrySucceeds 随后为原备件编号登记
// 五件库存：登记本身不替之前的申请预留数量，此时实物五件、有效占用零件、
// 可承诺五件。原请求无需重新提交，再以原承诺编号、原备件、三件数量和原
// 到期时刻预留应成功；承诺归属原请求，账目变为 5/3/2。历史依次保留旧
// 失败与本次成功：成功记录的库存依据是新增占用前的 5/0/5，旧失败记录的
// 内容与空依据保持原样，不被成功覆盖或补写；请求的承诺列表只出现这笔
// 成功承诺。
func TestReserveBeforePart_RegisterStockThenRetrySucceeds(t *testing.T) {
	s, clk := pltBaseScenario(t)

	// 备件缺失期间先失败一次，留下 seq=1 的 part_not_found 记录。
	if _, err := s.Reserve(pltCommitID, pltReqID, pltPartID, 3, clk.expiry, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while part missing: got %v, want ErrNotFound", err)
	}

	// 补登五件库存：登记本身不替之前的申请预留数量。
	if err := s.RegisterPart(pltPartID, 5); err != nil {
		t.Fatalf("register part after failed reserve: %v", err)
	}
	pltAssertStock(t, s, clk.secondAt, 5, 0, 5)
	// 原请求无需重新提交：资格仍合格，关联承诺列表仍为空。
	view, err := s.RequestView(pltReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view after part registration: %v", err)
	}
	assertEligible(t, view.Eligibility, clk)
	if len(view.Commitments) != 0 {
		t.Fatalf("part registration must not reserve for the earlier request, got %+v", view.Commitments)
	}

	// 沿用原承诺编号、原备件、三件数量和原到期时刻再次预留：成功。
	c, err := s.Reserve(pltCommitID, pltReqID, pltPartID, 3, clk.expiry, clk.secondAt)
	if err != nil {
		t.Fatalf("retry reserve after part registration: %v", err)
	}
	if c.ID != pltCommitID || c.RequestID != pltReqID || c.PartID != pltPartID ||
		c.Quantity != 3 || c.Used != 0 || c.Canceled || c.Expired {
		t.Fatalf("bad commitment: %+v", c)
	}
	if !c.Expiry.Equal(clk.expiry) {
		t.Fatalf("commitment expiry = %v, want %v", c.Expiry, clk.expiry)
	}

	// 账目变为：实物五件、有效占用三件、可承诺两件。
	pltAssertStock(t, s, clk.secondAt, 5, 3, 2)

	// 请求的承诺列表只出现这笔成功承诺，不多出先前失败申请的记录。
	view, err = s.RequestView(pltReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view after success: %v", err)
	}
	if len(view.Commitments) != 1 {
		t.Fatalf("commitments = %+v, want exactly the one successful commitment", view.Commitments)
	}
	d := view.Commitments[0]
	if d.CommitmentID != pltCommitID || d.RequestID != pltReqID || d.PartID != pltPartID ||
		d.OriginalQuantity != 3 || d.UsedQuantity != 0 || d.RemainingQuantity != 3 ||
		d.Status != CommitmentActive || !d.Expiry.Equal(clk.expiry) {
		t.Fatalf("commitment detail = %+v, want the successful 3-piece active commitment", d)
	}

	// 历史两条：旧失败记录在前且内容与空依据保持原样；成功记录在后，
	// 不覆盖也不补写前一条。
	h, err := s.RequestHistory(pltReqID)
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
	if ok.CommitID != pltCommitID || ok.PartID != pltPartID || ok.Quantity != 3 ||
		!ok.Expiry.Equal(clk.expiry) || !ok.Now.Equal(clk.secondAt) {
		t.Fatalf("success submission content: %+v", ok)
	}
	// 成功记录保存当次合格的资格依据。
	assertEligible(t, ok.Eligibility, clk)
	// 库存依据是新增占用前的五件实物、零件占用、五件可承诺。
	if ok.StockBasis == nil {
		t.Fatal("success record missing stock basis")
	}
	if ok.StockBasis.PhysicalRemaining != 5 || ok.StockBasis.ActiveOccupied != 0 ||
		ok.StockBasis.Committable != 5 {
		t.Fatalf("success stock basis = %+v, want pre-addition 5/0/5", ok.StockBasis)
	}
}

// TestReserveBeforePart_LateStockInsufficient 补登库存不足的边界：备件缺失
// 期间同样先申请三件并被拒绝（留下 part_not_found 记录），随后只登记两件
// 库存，再沿用原提交内容预留，返回 ErrInsufficientStock——不能创建承诺，
// 也不能只占用两件作为部分成功。账目保持 2/0/2，历史追加 insufficient_stock
// 并保存合格资格与当次真实库存依据，之前的 part_not_found 记录原样保留。
func TestReserveBeforePart_LateStockInsufficient(t *testing.T) {
	s, clk := pltBaseScenario(t)

	// 备件缺失期间先申请三件并被拒绝，留下 seq=1 的 part_not_found 记录。
	if _, err := s.Reserve(pltCommitID, pltReqID, pltPartID, 3, clk.expiry, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while part missing: got %v, want ErrNotFound", err)
	}

	// 只登记两件库存，不足申请的三件。
	if err := s.RegisterPart(pltPartID, 2); err != nil {
		t.Fatalf("register part with insufficient stock: %v", err)
	}
	pltAssertStock(t, s, clk.secondAt, 2, 0, 2)

	// 沿用原承诺编号、原备件、三件数量和原到期时刻再次预留：库存不足。
	if _, err := s.Reserve(pltCommitID, pltReqID, pltPartID, 3, clk.expiry, clk.secondAt); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve with insufficient late-registered stock: got %v, want ErrInsufficientStock", err)
	}

	// 不能创建承诺，失败仍不占用编号。
	if _, err := s.Commitment(pltCommitID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("insufficient-stock reserve created a commitment: got %v", err)
	}
	// 也不能只占用两件作为部分成功：账目保持两件、零件、两件。
	pltAssertStock(t, s, clk.secondAt, 2, 0, 2)
	// 请求下仍无任何承诺。
	view, err := s.RequestView(pltReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view after insufficient-stock reserve: %v", err)
	}
	assertEligible(t, view.Eligibility, clk)
	if len(view.Commitments) != 0 {
		t.Fatalf("insufficient-stock reserve gained commitments: %+v", view.Commitments)
	}

	// 历史两条：之前的 part_not_found 记录原样保留在前；本次追加
	// insufficient_stock，保存合格资格与当次真实库存依据 2/0/2。
	h, err := s.RequestHistory(pltReqID)
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
	if ins.CommitID != pltCommitID || ins.PartID != pltPartID || ins.Quantity != 3 ||
		!ins.Expiry.Equal(clk.expiry) || !ins.Now.Equal(clk.secondAt) {
		t.Fatalf("insufficient-stock submission content: %+v", ins)
	}
	// 资格依据为当次合格资格。
	assertEligible(t, ins.Eligibility, clk)
	// 库存依据为当次真实账目：两件实物、零件占用、两件可承诺。
	if ins.StockBasis == nil {
		t.Fatal("insufficient-stock record missing stock basis")
	}
	if ins.StockBasis.PhysicalRemaining != 2 || ins.StockBasis.ActiveOccupied != 0 ||
		ins.StockBasis.Committable != 2 {
		t.Fatalf("insufficient-stock basis = %+v, want 2/0/2", ins.StockBasis)
	}
}
