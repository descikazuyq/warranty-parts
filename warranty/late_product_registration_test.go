package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障“保修请求先提交、产品资料后登记”：SubmitRequest 允许
// 关联尚未登记的产品，产品缺失留待资格查询与预留时报告（ErrNotFound）；
// 失败不产生承诺、不占用库存，并在该请求的预留历史中留下资格依据与库存
// 依据均为空的产品缺失记录。随后补登产品资料即作用于原请求——无需再次
// 提交或替换请求——资格按补登条款正常判断，沿用此前失败的承诺编号与
// 预留内容重试即可成功；早先的缺失记录不被补登资料回填、不被成功记录
// 覆盖，两条记录各有自己的递增序号。若补登条款把原请求的故障代码列为
// 除外，资格明确显示除外原因、预留返回 ErrIneligible，本次失败历史保存
// 已取得的资格与库存依据，而原请求的产品编号、故障代码及此前缺失记录
// 仍然保留。测试只沿用既有公开入口与现有错误类别，不改变任何规则。

// 本文件专用编号，避免与其他测试文件混用。
const (
	lpProductID   = "p-late"
	lpOtherProdID = "p-late-other"
	lpPartID      = "part-late"
	lpReqID       = "r-late"
	lpOtherReqID  = "r-late-other"
	lpCommitID    = "c-late"
	lpOtherCommit = "c-late-other"
	lpFaultCode   = "NOISE"
)

// lpClock 汇集主例的全部时刻。
type lpClock struct {
	purchase  time.Time // 补登产品的购买时刻
	deadline  time.Time // 保修截止时刻：购买时刻起第三十天
	firstAt   time.Time // 产品缺失期间资格查询与首次预留的当次当前时刻
	firstExp  time.Time // 首次预留的到期时刻，晚于 firstAt
	secondAt  time.Time // 补登后再次判断与重试预留的当次当前时刻
	secondExp time.Time // 重试预留的到期时刻，晚于 secondAt
}

// lpBaseScenario 构造用户给出的主例：备件 part-late 有 10 件实物；另一张
// 关联已登记产品的合格请求 r-late-other 已有效占用 4 件。新请求 r-late
// 关联尚未登记的产品 p-late，故障代码 NOISE 非空。此刻 p-late 尚未登记。
func lpBaseScenario(t *testing.T) (*Store, lpClock) {
	t.Helper()
	clk := lpClock{
		purchase:  t0,
		deadline:  t0.Add(30 * day),
		firstAt:   t0.Add(10 * day),
		firstExp:  t0.Add(45 * day),
		secondAt:  t0.Add(11 * day),
		secondExp: t0.Add(46 * day),
	}

	s := NewStore()
	// 另一张合格请求依赖的已登记产品（条款不除外 NOISE）。
	if err := s.RegisterProduct(lpOtherProdID, t0, 365, nil); err != nil {
		t.Fatalf("register other product: %v", err)
	}
	if err := s.RegisterPart(lpPartID, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	// 另一张合格请求，并有效占用 4 件。
	if err := s.SubmitRequest(lpOtherReqID, lpOtherProdID, "OTHER_FAULT"); err != nil {
		t.Fatalf("submit other request: %v", err)
	}
	if _, err := s.Reserve(lpOtherCommit, lpOtherReqID, lpPartID, 4, clk.firstExp, clk.firstAt); err != nil {
		t.Fatalf("reserve other commitment: %v", err)
	}
	// 新请求关联一个尚未登记的产品，故障代码非空：提交本身必须成功。
	if err := s.SubmitRequest(lpReqID, lpProductID, lpFaultCode); err != nil {
		t.Fatalf("submit request before product registration: %v", err)
	}
	return s, clk
}

// assertStock10x6 断言备件账目保持：实物 10 件、有效占用 4 件、可承诺 6 件。
func assertStock10x6(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	st, err := s.PartStatus(lpPartID, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 4 || st.Committable != 6 {
		t.Fatalf("stock = phys %d / occupied %d / committable %d, want 10/4/6",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
}

// assertMissingProductFailureRecord 断言产品缺失失败记录的完整内容：保存
// 提交编号、备件、数量、到期时刻与当次当前时刻；即使备件已经存在，资格
// 依据和库存依据也都为空。
func assertMissingProductFailureRecord(t *testing.T, rec HistoryRecord, seq int, clk lpClock) {
	t.Helper()
	if rec.Seq != seq {
		t.Fatalf("seq = %d, want %d", rec.Seq, seq)
	}
	if rec.Success {
		t.Fatalf("missing-product record marked success: %+v", rec)
	}
	if rec.Error != HistoryErrorProductNotFound {
		t.Fatalf("error category = %q, want %q", rec.Error, HistoryErrorProductNotFound)
	}
	if rec.CommitID != lpCommitID || rec.PartID != lpPartID || rec.Quantity != 3 {
		t.Fatalf("submission content = %q/%q/%d, want %q/%q/3",
			rec.CommitID, rec.PartID, rec.Quantity, lpCommitID, lpPartID)
	}
	if !rec.Expiry.Equal(clk.firstExp) || !rec.Now.Equal(clk.firstAt) {
		t.Fatalf("times = expiry %v now %v, want %v / %v",
			rec.Expiry, rec.Now, clk.firstExp, clk.firstAt)
	}
	// 即使备件已经存在，这条记录的资格依据和库存依据也都为空。
	if rec.Eligibility != nil {
		t.Fatalf("missing-product record must have nil eligibility, got %+v", rec.Eligibility)
	}
	if rec.StockBasis != nil {
		t.Fatalf("missing-product record must have nil stock basis even though part exists, got %+v", rec.StockBasis)
	}
}

// TestRequestBeforeProduct_MissingPeriod 产品缺失期间：资格查询（Evaluate
// 与 RequestView）和预留都返回 ErrNotFound；失败不产生承诺，实物仍 10 件、
// 有效占用 4 件、可承诺 6 件；预留历史留下产品缺失的失败记录，保存提交
// 编号、备件、数量、到期时刻和当次当前时刻，且资格依据、库存依据均为空。
func TestRequestBeforeProduct_MissingPeriod(t *testing.T) {
	s, clk := lpBaseScenario(t)

	// 提交时不校验产品：请求确实已保存，且保留原产品编号与故障代码。
	req, err := s.Request(lpReqID)
	if err != nil {
		t.Fatalf("request lookup: %v", err)
	}
	if req.ProductID != lpProductID || req.FaultCode != lpFaultCode {
		t.Fatalf("saved request = %+v, want product %q fault %q", req, lpProductID, lpFaultCode)
	}
	// 产品确实尚未登记。
	if _, err := s.Product(lpProductID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("product should be missing: got %v", err)
	}

	// 资格查询：产品缺失返回 ErrNotFound。
	if _, err := s.Evaluate(lpReqID, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evaluate while product missing: got %v, want ErrNotFound", err)
	}
	// 按请求查看同样沿用现有错误类别报告缺失。
	if _, err := s.RequestView(lpReqID, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("request view while product missing: got %v, want ErrNotFound", err)
	}

	// 预留 3 件，到期时刻晚于当次时刻：因产品缺失返回 ErrNotFound。
	if _, err := s.Reserve(lpCommitID, lpReqID, lpPartID, 3, clk.firstExp, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while product missing: got %v, want ErrNotFound", err)
	}

	// 失败不能产生承诺：编号未被占用，请求下也没有任何承诺明细。
	if _, err := s.Commitment(lpCommitID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve created a commitment: got %v", err)
	}
	if view, err := s.RequestView(lpReqID, clk.firstAt); err == nil {
		t.Fatalf("request view should still report missing product, got view with %d commitments",
			len(view.Commitments))
	}

	// 实物仍为 10、有效占用仍为 4、可承诺仍为 6（另一张请求的占用不变）。
	assertStock10x6(t, s, clk.firstAt)

	// 该请求的预留历史留下产品缺失的失败记录。
	h, err := s.RequestHistory(lpReqID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1", len(h))
	}
	assertMissingProductFailureRecord(t, h[0], 1, clk)

	// 另一张请求的 4 件占用与它自己的历史均保持原值。
	other, err := s.RequestHistory(lpOtherReqID)
	if err != nil {
		t.Fatalf("other history: %v", err)
	}
	if len(other) != 1 || !other[0].Success || other[0].CommitID != lpOtherCommit || other[0].Quantity != 4 {
		t.Fatalf("other request history altered: %+v", other)
	}
}

// TestRequestBeforeProduct_RegisterEligibleThenRetrySucceeds 补登合格条款：
// 购买时刻不晚于本次判断时刻、保修尚未到期且故障未被除外。无需再次提交
// 或替换请求，再查询原请求时资格按登记资料正常判断；沿用刚才失败的承诺
// 编号和预留内容、到期时刻仍晚于本次时刻时成功预留 3 件。实物仍 10 件、
// 有效占用变为 7、可承诺变为 3，另一张请求的 4 件保持原值。成功历史的
// 资格依据对应补登条款，库存依据是新增占用前的 10/4/6。此前的产品缺失
// 记录仍在前面且依据仍为空，不被回填或覆盖，两条记录序号各自递增。
func TestRequestBeforeProduct_RegisterEligibleThenRetrySucceeds(t *testing.T) {
	s, clk := lpBaseScenario(t)

	// 产品缺失期间先失败一次，留下 seq=1 的缺失记录。
	if _, err := s.Reserve(lpCommitID, lpReqID, lpPartID, 3, clk.firstExp, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while product missing: got %v, want ErrNotFound", err)
	}
	assertStock10x6(t, s, clk.firstAt)

	// 补登产品：购买时刻不晚于本次判断时刻、保修 30 天（在 secondAt 尚未到期）、
	// 除外清单不含 NOISE。
	if err := s.RegisterProduct(lpProductID, clk.purchase, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product after request: %v", err)
	}

	// 无需再次提交或替换请求：原请求的资格按补登资料正常判断。
	e, err := s.Evaluate(lpReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("evaluate after registration: %v", err)
	}
	if !e.Eligible || e.Excluded || len(e.Reasons) != 0 {
		t.Fatalf("expected eligible under late-registered terms, got %+v", e)
	}
	if e.ProductID != lpProductID || e.FaultCode != lpFaultCode {
		t.Fatalf("eligibility identity = %q/%q, want %q/%q", e.ProductID, e.FaultCode, lpProductID, lpFaultCode)
	}
	if !e.PurchaseTime.Equal(clk.purchase) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(clk.deadline) {
		t.Fatalf("eligibility basis = (%v, %d, %v), want late-registered terms (%v, 30, %v)",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, clk.purchase, clk.deadline)
	}
	// 按请求查看同样按补登条款判断，并仍无关联承诺。
	view, err := s.RequestView(lpReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view after registration: %v", err)
	}
	if !view.Eligibility.Eligible || len(view.Commitments) != 0 {
		t.Fatalf("view after registration: eligible=%v commitments=%+v", view.Eligibility.Eligible, view.Commitments)
	}

	// 沿用刚才失败的承诺编号和预留内容再次申请，到期时刻仍晚于本次时刻：成功。
	c, err := s.Reserve(lpCommitID, lpReqID, lpPartID, 3, clk.secondExp, clk.secondAt)
	if err != nil {
		t.Fatalf("retry reserve after registration: %v", err)
	}
	if c.ID != lpCommitID || c.RequestID != lpReqID || c.PartID != lpPartID ||
		c.Quantity != 3 || c.Used != 0 || c.Canceled || c.Expired {
		t.Fatalf("bad commitment: %+v", c)
	}
	if !c.Expiry.Equal(clk.secondExp) {
		t.Fatalf("commitment expiry = %v, want %v", c.Expiry, clk.secondExp)
	}

	// 实物仍 10 件，有效占用变为 7（4+3），可承诺变为 3。
	st, err := s.PartStatus(lpPartID, clk.secondAt)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 7 || st.Committable != 3 {
		t.Fatalf("stock after success = phys %d / occupied %d / committable %d, want 10/7/3",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	// 另一张请求的 4 件占用保持原值。
	other, err := s.Commitment(lpOtherCommit)
	if err != nil {
		t.Fatalf("other commitment: %v", err)
	}
	if other.Quantity != 4 || other.Used != 0 || other.Canceled || other.Expired {
		t.Fatalf("other commitment altered: %+v", other)
	}
	// 明细恰为两笔有效承诺，数量分别为 4 和 3。
	if len(st.Details) != 2 {
		t.Fatalf("part details = %+v, want 2 active commitments", st.Details)
	}
	byID := map[string]CommitmentDetail{}
	for _, d := range st.Details {
		byID[d.CommitmentID] = d
		if d.Status != CommitmentActive {
			t.Fatalf("commitment %q status = %q, want active", d.CommitmentID, d.Status)
		}
	}
	if byID[lpOtherCommit].OriginalQuantity != 4 || byID[lpCommitID].OriginalQuantity != 3 {
		t.Fatalf("detail quantities altered: other=%d retry=%d",
			byID[lpOtherCommit].OriginalQuantity, byID[lpCommitID].OriginalQuantity)
	}

	// 历史两条：此前的产品缺失记录仍在前面（seq=1），依据仍为空；
	// 成功记录在后（seq=2），不覆盖也不回填前一条。
	h, err := s.RequestHistory(lpReqID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	assertMissingProductFailureRecord(t, h[0], 1, clk)
	ok := h[1]
	if ok.Seq != 2 || !ok.Success || ok.Error != "" {
		t.Fatalf("second record should be success with seq 2: %+v", ok)
	}
	if ok.CommitID != lpCommitID || ok.PartID != lpPartID || ok.Quantity != 3 ||
		!ok.Expiry.Equal(clk.secondExp) || !ok.Now.Equal(clk.secondAt) {
		t.Fatalf("success submission content: %+v", ok)
	}
	// 成功历史的资格依据对应补登的产品条款。
	if ok.Eligibility == nil {
		t.Fatal("success record missing eligibility basis")
	}
	if !ok.Eligibility.Eligible || ok.Eligibility.Excluded || len(ok.Eligibility.Reasons) != 0 ||
		ok.Eligibility.ProductID != lpProductID || ok.Eligibility.FaultCode != lpFaultCode ||
		!ok.Eligibility.PurchaseTime.Equal(clk.purchase) || ok.Eligibility.WarrantyDays != 30 ||
		!ok.Eligibility.WarrantyExpiry.Equal(clk.deadline) {
		t.Fatalf("success eligibility basis should match late-registered terms: %+v", ok.Eligibility)
	}
	// 库存依据是新增占用前的 10 件实物、4 件占用和 6 件可承诺。
	if ok.StockBasis == nil {
		t.Fatal("success record missing stock basis")
	}
	if ok.StockBasis.PhysicalRemaining != 10 || ok.StockBasis.ActiveOccupied != 4 ||
		ok.StockBasis.Committable != 6 {
		t.Fatalf("success stock basis = %+v, want pre-addition 10/4/6", ok.StockBasis)
	}

	// 补登与成功预留都不改变原请求的产品编号与故障代码。
	req, err := s.Request(lpReqID)
	if err != nil {
		t.Fatalf("request lookup: %v", err)
	}
	if req.ProductID != lpProductID || req.FaultCode != lpFaultCode {
		t.Fatalf("request identity altered: %+v", req)
	}
}

// TestRequestBeforeProduct_RegisterExcludingTermsRejects 另一种补登条件：
// 登记的条款已将原请求的故障代码列为除外。资格明确显示除外原因，预留
// 返回 ErrIneligible；不新增占用，失败历史保存本次已取得的资格与库存
// 依据；原请求的产品编号、故障代码及此前缺失资料的记录均保留。
func TestRequestBeforeProduct_RegisterExcludingTermsRejects(t *testing.T) {
	s, clk := lpBaseScenario(t)

	// 产品缺失期间先失败一次，留下 seq=1 的缺失记录。
	if _, err := s.Reserve(lpCommitID, lpReqID, lpPartID, 3, clk.firstExp, clk.firstAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while product missing: got %v, want ErrNotFound", err)
	}

	// 补登条款把原请求的故障代码 NOISE 列为除外（保修期本身仍有效）。
	if err := s.RegisterProduct(lpProductID, clk.purchase, 30, []string{lpFaultCode}); err != nil {
		t.Fatalf("register excluding product: %v", err)
	}

	// 资格明确显示除外原因：不合格、命中除外、拒绝原因恰为故障除外一项。
	e, err := s.Evaluate(lpReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("evaluate after excluding registration: %v", err)
	}
	if e.Eligible || !e.Excluded {
		t.Fatalf("expected excluded, got %+v", e)
	}
	if len(e.Reasons) != 1 || e.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("reasons = %v, want exactly [fault_code_excluded]", e.Reasons)
	}
	if !e.PurchaseTime.Equal(clk.purchase) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(clk.deadline) {
		t.Fatalf("eligibility basis = (%v, %d, %v), want late-registered terms",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry)
	}
	view, err := s.RequestView(lpReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if view.Eligibility.Eligible || !view.Eligibility.Excluded ||
		len(view.Eligibility.Reasons) != 1 || view.Eligibility.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("view eligibility = %+v, want fault_code_excluded", view.Eligibility)
	}

	// 沿用同一编号与预留内容再次申请：返回 ErrIneligible。
	if _, err := s.Reserve(lpCommitID, lpReqID, lpPartID, 3, clk.secondExp, clk.secondAt); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve under excluding terms: got %v, want ErrIneligible", err)
	}

	// 不新增占用：实物 10、有效占用 4、可承诺 6，且不生成承诺。
	assertStock10x6(t, s, clk.secondAt)
	if _, err := s.Commitment(lpCommitID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ineligible retry created a commitment: got %v", err)
	}
	view, err = s.RequestView(lpReqID, clk.secondAt)
	if err != nil {
		t.Fatalf("request view after ineligible retry: %v", err)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("ineligible retry gained commitments: %+v", view.Commitments)
	}

	// 历史两条：此前缺失记录保留在前且依据为空；本次失败记录保存已取得的
	// 资格与库存依据（备件存在，库存依据非空）。
	h, err := s.RequestHistory(lpReqID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	assertMissingProductFailureRecord(t, h[0], 1, clk)
	excl := h[1]
	if excl.Seq != 2 || excl.Success || excl.Error != HistoryErrorIneligible {
		t.Fatalf("second record should be ineligible failure with seq 2: %+v", excl)
	}
	if excl.CommitID != lpCommitID || excl.PartID != lpPartID || excl.Quantity != 3 ||
		!excl.Expiry.Equal(clk.secondExp) || !excl.Now.Equal(clk.secondAt) {
		t.Fatalf("ineligible submission content: %+v", excl)
	}
	if excl.Eligibility == nil {
		t.Fatal("ineligible record should preserve the obtained eligibility")
	}
	if excl.Eligibility.Eligible || !excl.Eligibility.Excluded ||
		len(excl.Eligibility.Reasons) != 1 || excl.Eligibility.Reasons[0] != ReasonFaultExcluded ||
		excl.Eligibility.ProductID != lpProductID || excl.Eligibility.FaultCode != lpFaultCode ||
		!excl.Eligibility.PurchaseTime.Equal(clk.purchase) || excl.Eligibility.WarrantyDays != 30 ||
		!excl.Eligibility.WarrantyExpiry.Equal(clk.deadline) {
		t.Fatalf("ineligible record eligibility should match late-registered excluding terms: %+v", excl.Eligibility)
	}
	if excl.StockBasis == nil {
		t.Fatal("ineligible record should preserve the obtained stock basis")
	}
	if excl.StockBasis.PhysicalRemaining != 10 || excl.StockBasis.ActiveOccupied != 4 ||
		excl.StockBasis.Committable != 6 {
		t.Fatalf("ineligible record stock basis = %+v, want 10/4/6", excl.StockBasis)
	}

	// 原请求的产品编号与故障代码均保留。
	req, err := s.Request(lpReqID)
	if err != nil {
		t.Fatalf("request lookup: %v", err)
	}
	if req.ProductID != lpProductID || req.FaultCode != lpFaultCode {
		t.Fatalf("request identity altered: %+v", req)
	}
}
