package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障“保修请求先提交、产品资料后登记”的现有行为：提交请求时不
// 要求产品已经登记，产品是否合格留待资格查询与预留时读取产品资料；产品补登
// 后无需再次提交或替换请求，原请求即按补登条款判断资格，此前因产品缺失产生
// 的失败记录按当时事实保留——提交编号、备件、数量、到期时刻与当次当前时刻
// 仍在，资格依据与库存依据明确为空，不会被补登资料回填，也不会被后来的成功
// 或再次失败的记录覆盖，各条记录保有自己严格递增的序号。这些测试只沿用登记、
// 提交、资格查询、预留与历史等既有公开入口和现有错误类别，不改变任何规则。

// 本文件专用编号，避免与其他测试文件混用。
const (
	lpPartID   = "part-late"
	lpOtherReq = "r-late-other"
	lpNewReq   = "r-late-new"
	lpCommit   = "c-late-new"
	lpOtherOK  = "c-late-other"
)

// lpClock 汇集补登主例的全部时刻。
type lpClock struct {
	missingAt   time.Time // 产品缺失期间首次资格查询与预留的当前时刻
	missingExp  time.Time // 首次预留的到期时刻，晚于 missingAt
	judgeAt     time.Time // 补登后本次判断与第二次预留的当前时刻
	judgeExp    time.Time // 第二次预留的到期时刻，晚于 judgeAt
	excludedAt  time.Time // 除外条款补登后本次判断与预留的当前时刻
	excludedExp time.Time // 除外场景预留的到期时刻，晚于 excludedAt
	purchase    time.Time // 补登产品的购买时刻，不晚于任何判断时刻
}

func lpTimes() lpClock {
	return lpClock{
		purchase:    t0,
		missingAt:   t0.Add(10 * day),
		missingExp:  t0.Add(40 * day),
		judgeAt:     t0.Add(12 * day),
		judgeExp:    t0.Add(50 * day),
		excludedAt:  t0.Add(14 * day),
		excludedExp: t0.Add(51 * day),
	}
}

// lpBase 构造产品缺失阶段的共同起点：备件十件实物，另一张合格请求已有效占用
// 四件；新请求 r-late-new 关联尚未登记的产品 p-late-new，故障代码 NOISE 非空。
func lpBase(t *testing.T) (*Store, lpClock) {
	t.Helper()
	clk := lpTimes()

	s := NewStore()
	if err := s.RegisterProduct("p-late-other", clk.purchase, 60, nil); err != nil {
		t.Fatalf("register other product: %v", err)
	}
	if err := s.RegisterPart(lpPartID, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(lpOtherReq, "p-late-other", "NOISE"); err != nil {
		t.Fatalf("submit other request: %v", err)
	}
	if err := s.SubmitRequest(lpNewReq, "p-late-new", "NOISE"); err != nil {
		t.Fatalf("submit request for unregistered product: %v", err)
	}
	// 另一张合格请求有效占用四件，到期晚于本文件的全部判断时刻。
	if _, err := s.Reserve(lpOtherOK, lpOtherReq, lpPartID, 4, clk.judgeExp.Add(10*day), clk.missingAt); err != nil {
		t.Fatalf("reserve other four units: %v", err)
	}
	return s, clk
}

// assertAccount1046 断言备件账目为实物十件、有效占用四件、可承诺六件。
func assertAccount1046(t *testing.T, s *Store, now time.Time) {
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

// assertMissingRecord 断言产品缺失期间留下的失败记录：序号为一、失败类别为
// product_not_found，提交编号/备件/数量/到期时刻/当次当前时刻完整保留，
// 资格依据与库存依据都明确为空——即使备件已经登记也不用零库存或其他内容替代。
func assertMissingRecord(t *testing.T, rec HistoryRecord, clk lpClock) {
	t.Helper()
	if rec.Seq != 1 {
		t.Fatalf("missing record seq = %d, want 1", rec.Seq)
	}
	if rec.Success || rec.Error != HistoryErrorProductNotFound {
		t.Fatalf("missing record marker = success=%v error=%q, want failure product_not_found",
			rec.Success, rec.Error)
	}
	if rec.CommitID != lpCommit || rec.PartID != lpPartID || rec.Quantity != 3 {
		t.Fatalf("missing record submission = %q/%q/%d, want %q/%q/3",
			rec.CommitID, rec.PartID, rec.Quantity, lpCommit, lpPartID)
	}
	if !rec.Expiry.Equal(clk.missingExp) || !rec.Now.Equal(clk.missingAt) {
		t.Fatalf("missing record times = expiry %v now %v, want %v / %v",
			rec.Expiry, rec.Now, clk.missingExp, clk.missingAt)
	}
	if rec.Eligibility != nil {
		t.Fatalf("missing record eligibility must be nil, got %+v", rec.Eligibility)
	}
	if rec.StockBasis != nil {
		t.Fatalf("missing record stock basis must be nil even though part exists, got %+v", rec.StockBasis)
	}
}

// TestLateProductRegistration_MissingThenEligible 主例：新请求关联尚未登记的
// 产品。产品缺失期间资格查询与预留都返回 ErrNotFound；失败不产生承诺，备件
// 始终是实物十件、有效占用四件（另一张请求）、可承诺六件；该请求的预留历史
// 留下一条产品缺失失败记录，保存提交编号、备件、数量、到期时刻和当次当前
// 时刻，备件虽已存在但资格依据与库存依据都为空。随后补登合格产品（购买时刻
// 不晚于本次判断时刻、保修未到期、故障未被除外），无需再次提交请求：原请求
// 的资格按补登资料正常判断；沿用失败过的承诺编号与原预留内容、到期时刻仍晚
// 于本次时刻时成功预留三件，实物仍为十件、有效占用变为七件、可承诺变为三件，
// 另一张请求的四件占用保持不变。成功记录的资格依据对应补登条款，库存依据是
// 新增占用前的 10/4/6；此前的产品缺失记录仍排在前面且依据仍为空，不被回填
// 或覆盖，两条记录各有自己的递增序号。
func TestLateProductRegistration_MissingThenEligible(t *testing.T) {
	s, clk := lpBase(t)

	// ---- 产品缺失阶段 ----

	// 资格查询：产品尚未登记，明确返回 ErrNotFound。
	if _, err := s.Evaluate(lpNewReq, clk.missingAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evaluate while product missing: got %v, want ErrNotFound", err)
	}
	// 按请求查看同样在资格一步失败，不返回任何视图。
	if _, err := s.RequestView(lpNewReq, clk.missingAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("request view while product missing: got %v, want ErrNotFound", err)
	}

	// 申请预留三件，到期时刻晚于当次时刻：产品缺失，返回 ErrNotFound。
	if _, err := s.Reserve(lpCommit, lpNewReq, lpPartID, 3, clk.missingExp, clk.missingAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while product missing: got %v, want ErrNotFound", err)
	}

	// 失败不能产生承诺。
	if _, err := s.Commitment(lpCommit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve created commitment %q: %v", lpCommit, err)
	}
	// 实物十件、另一张请求占用四件、可承诺六件，失败不占用数量。
	assertAccount1046(t, s, clk.missingAt)

	// 失败记录按当时事实留痕：依据明确为空。
	h, err := s.RequestHistory(lpNewReq)
	if err != nil {
		t.Fatalf("history while missing: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1", len(h))
	}
	assertMissingRecord(t, h[0], clk)

	// 另一张请求的四件占用及其历史不受影响。
	other, err := s.RequestHistory(lpOtherReq)
	if err != nil {
		t.Fatalf("other request history: %v", err)
	}
	if len(other) != 1 || !other[0].Success || other[0].CommitID != lpOtherOK || other[0].Quantity != 4 {
		t.Fatalf("other request history altered: %+v", other)
	}

	// ---- 补登合格产品 ----

	// 购买时刻不晚于本次判断时刻，保修六十天（在 judgeAt 尚未到期），NOISE 未被除外。
	if err := s.RegisterProduct("p-late-new", clk.purchase, 60, nil); err != nil {
		t.Fatalf("register product late: %v", err)
	}

	// 无需再次提交或替换请求：原请求按补登资料正常判断为合格。
	e, err := s.Evaluate(lpNewReq, clk.judgeAt)
	if err != nil {
		t.Fatalf("evaluate after registration: %v", err)
	}
	wantDeadline := clk.purchase.Add(60 * day)
	if !e.Eligible || len(e.Reasons) != 0 || e.Excluded {
		t.Fatalf("eligibility after registration = %+v, want eligible with no reasons", e)
	}
	if e.RequestID != lpNewReq || e.ProductID != "p-late-new" || e.FaultCode != "NOISE" {
		t.Fatalf("eligibility identity = %q/%q/%q, want %q/p-late-new/NOISE",
			e.RequestID, e.ProductID, e.FaultCode, lpNewReq)
	}
	if !e.PurchaseTime.Equal(clk.purchase) || e.WarrantyDays != 60 || !e.WarrantyExpiry.Equal(wantDeadline) {
		t.Fatalf("eligibility basis = purchase %v days %d expiry %v, want %v / 60 / %v",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, clk.purchase, wantDeadline)
	}
	// 按请求查看的资格与直接查询一致，且补登前后没有任何关联承诺。
	view, err := s.RequestView(lpNewReq, clk.judgeAt)
	if err != nil {
		t.Fatalf("request view after registration: %v", err)
	}
	if view.Eligibility == nil || !view.Eligibility.Eligible || len(view.Eligibility.Reasons) != 0 {
		t.Fatalf("view eligibility after registration = %+v, want eligible", view.Eligibility)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("request gained commitments before reserve: %+v", view.Commitments)
	}

	// 沿用失败过的承诺编号与原预留内容再次申请，到期时刻仍晚于本次时刻：成功。
	c, err := s.Reserve(lpCommit, lpNewReq, lpPartID, 3, clk.judgeExp, clk.judgeAt)
	if err != nil {
		t.Fatalf("reserve after registration: %v", err)
	}
	if c.ID != lpCommit || c.RequestID != lpNewReq || c.PartID != lpPartID ||
		c.Quantity != 3 || c.Used != 0 || c.Canceled || c.Expired || !c.Expiry.Equal(clk.judgeExp) {
		t.Fatalf("successful commitment = %+v", c)
	}

	// 实物仍为十件（预留不扣减实物），有效占用变为七件，可承诺变为三件。
	st, err := s.PartStatus(lpPartID, clk.judgeAt)
	if err != nil {
		t.Fatalf("part status after success: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 7 || st.Committable != 3 {
		t.Fatalf("stock after success = phys %d / occupied %d / committable %d, want 10/7/3",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	// 另一张请求的四件占用保持原值，新承诺三件，二者并存于明细。
	details := map[string]CommitmentDetail{}
	for _, d := range st.Details {
		details[d.CommitmentID] = d
	}
	d4 := details[lpOtherOK]
	if d4.RequestID != lpOtherReq || d4.OriginalQuantity != 4 || d4.RemainingQuantity != 4 ||
		d4.UsedQuantity != 0 || d4.Status != CommitmentActive {
		t.Fatalf("other commitment altered: %+v", d4)
	}
	d3 := details[lpCommit]
	if d3.RequestID != lpNewReq || d3.OriginalQuantity != 3 || d3.RemainingQuantity != 3 ||
		d3.UsedQuantity != 0 || d3.Status != CommitmentActive {
		t.Fatalf("new commitment detail altered: %+v", d3)
	}

	// 历史：缺失记录在前（序号一、依据为空），成功记录在后（序号二）。
	h2, err := s.RequestHistory(lpNewReq)
	if err != nil {
		t.Fatalf("history after success: %v", err)
	}
	if len(h2) != 2 {
		t.Fatalf("history len = %d, want 2", len(h2))
	}
	assertMissingRecord(t, h2[0], clk)
	succ := h2[1]
	if succ.Seq != 2 || !succ.Success || succ.Error != "" {
		t.Fatalf("success record marker = seq %d success=%v error=%q, want seq 2 success",
			succ.Seq, succ.Success, succ.Error)
	}
	if succ.CommitID != lpCommit || succ.PartID != lpPartID || succ.Quantity != 3 ||
		!succ.Expiry.Equal(clk.judgeExp) || !succ.Now.Equal(clk.judgeAt) {
		t.Fatalf("success record submission = %+v", succ)
	}
	// 成功记录的资格依据对应补登条款。
	se := succ.Eligibility
	if se == nil {
		t.Fatal("success record missing eligibility basis")
	}
	if !se.Eligible || se.Excluded || len(se.Reasons) != 0 ||
		se.RequestID != lpNewReq || se.ProductID != "p-late-new" || se.FaultCode != "NOISE" ||
		!se.PurchaseTime.Equal(clk.purchase) || se.WarrantyDays != 60 || !se.WarrantyExpiry.Equal(wantDeadline) {
		t.Fatalf("success eligibility basis = %+v, want eligible basis from registered terms", se)
	}
	// 成功记录的库存依据是新增占用前的实物十件、占用四件、可承诺六件。
	if succ.StockBasis == nil ||
		succ.StockBasis.PhysicalRemaining != 10 ||
		succ.StockBasis.ActiveOccupied != 4 ||
		succ.StockBasis.Committable != 6 {
		t.Fatalf("success stock basis = %+v, want 10/4/6 before the new occupation", succ.StockBasis)
	}

	// 序号严格递增且两条记录各自独立。
	if h2[0].Seq >= h2[1].Seq {
		t.Fatalf("seqs not strictly increasing: %d then %d", h2[0].Seq, h2[1].Seq)
	}

	// 再次查询历史：缺失记录仍不被补登资料回填，也不被成功记录覆盖。
	h3, err := s.RequestHistory(lpNewReq)
	if err != nil {
		t.Fatalf("re-query history: %v", err)
	}
	if len(h3) != 2 {
		t.Fatalf("re-query history len = %d, want 2", len(h3))
	}
	assertMissingRecord(t, h3[0], clk)
}

// TestLateProductRegistration_ExcludedTermsReject 覆盖另一种补登条件：补登的
// 条款已把原请求的故障代码列为除外（在本次判断时刻保修本身尚未到期，因此
// 拒绝原因明确只有故障除外一项）。资格查询明确显示除外原因；沿用此前缺失
// 失败用过的承诺编号再次预留返回 ErrIneligible，不新增占用；新的失败记录
// 保存本次已经取得的资格依据（除外）与库存依据（10/4/6）。原请求的产品
// 编号、故障代码以及此前产品缺失的记录都保留：缺失记录仍排在最前、依据仍
// 为空，不被补登条款回填，新失败记录排在其后并拥有自己的递增序号。
func TestLateProductRegistration_ExcludedTermsReject(t *testing.T) {
	s, clk := lpBase(t)

	// 产品缺失期间先留下一条产品缺失失败记录。
	if _, err := s.Evaluate(lpNewReq, clk.missingAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evaluate while product missing: got %v, want ErrNotFound", err)
	}
	if _, err := s.Reserve(lpCommit, lpNewReq, lpPartID, 3, clk.missingExp, clk.missingAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve while product missing: got %v, want ErrNotFound", err)
	}
	assertAccount1046(t, s, clk.missingAt)

	// 补登条款：六十天保修在 excludedAt 尚未到期，但 NOISE 被列为除外故障。
	if err := s.RegisterProduct("p-late-new", clk.purchase, 60, []string{"NOISE"}); err != nil {
		t.Fatalf("register product late with exclusion: %v", err)
	}

	// 资格明确显示除外原因：只有 fault_code_excluded 一项（保修尚未到期、购买不晚于当前时刻）。
	e, err := s.Evaluate(lpNewReq, clk.excludedAt)
	if err != nil {
		t.Fatalf("evaluate with excluded terms: %v", err)
	}
	wantDeadline := clk.purchase.Add(60 * day)
	if e.Eligible {
		t.Fatalf("eligible = true, want false: %+v", e)
	}
	if !e.Excluded || len(e.Reasons) != 1 || e.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("reasons = %v, want exactly [fault_code_excluded]", e.Reasons)
	}
	if e.RequestID != lpNewReq || e.ProductID != "p-late-new" || e.FaultCode != "NOISE" ||
		!e.PurchaseTime.Equal(clk.purchase) || e.WarrantyDays != 60 || !e.WarrantyExpiry.Equal(wantDeadline) {
		t.Fatalf("excluded eligibility basis altered: %+v", e)
	}
	// 按请求查看同样明确显示除外原因。
	view, err := s.RequestView(lpNewReq, clk.excludedAt)
	if err != nil {
		t.Fatalf("request view with excluded terms: %v", err)
	}
	if view.Eligibility == nil || view.Eligibility.Eligible || !view.Eligibility.Excluded ||
		len(view.Eligibility.Reasons) != 1 || view.Eligibility.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("view eligibility = %+v, want excluded only", view.Eligibility)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("excluded request must have no commitments, got %+v", view.Commitments)
	}

	// 沿用缺失失败用过的承诺编号与预留内容再次申请：返回 ErrIneligible。
	if _, err := s.Reserve(lpCommit, lpNewReq, lpPartID, 3, clk.excludedExp, clk.excludedAt); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve with excluded terms: got %v, want ErrIneligible", err)
	}

	// 不新增占用：仍无新承诺，账目保持实物十件、占用四件、可承诺六件。
	if _, err := s.Commitment(lpCommit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ineligible reserve created commitment %q: %v", lpCommit, err)
	}
	assertAccount1046(t, s, clk.excludedAt)

	// 历史：此前缺失记录保留在最前（序号一、依据为空），其后是本次除外失败（序号二）。
	h, err := s.RequestHistory(lpNewReq)
	if err != nil {
		t.Fatalf("history after excluded failure: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	assertMissingRecord(t, h[0], clk)

	rec := h[1]
	if rec.Seq != 2 || rec.Success || rec.Error != HistoryErrorIneligible {
		t.Fatalf("excluded record marker = seq %d success=%v error=%q, want seq 2 ineligible failure",
			rec.Seq, rec.Success, rec.Error)
	}
	if rec.CommitID != lpCommit || rec.PartID != lpPartID || rec.Quantity != 3 ||
		!rec.Expiry.Equal(clk.excludedExp) || !rec.Now.Equal(clk.excludedAt) {
		t.Fatalf("excluded record submission = %+v", rec)
	}
	// 本次失败保存已取得的资格依据：除外、拒绝原因恰为故障除外，条款为补登内容。
	re := rec.Eligibility
	if re == nil {
		t.Fatal("excluded failure must keep the eligibility basis obtained this time")
	}
	if re.Eligible || !re.Excluded || len(re.Reasons) != 1 || re.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("excluded record eligibility = %+v, want fault_code_excluded only", re)
	}
	if re.RequestID != lpNewReq || re.ProductID != "p-late-new" || re.FaultCode != "NOISE" ||
		!re.PurchaseTime.Equal(clk.purchase) || re.WarrantyDays != 60 || !re.WarrantyExpiry.Equal(wantDeadline) {
		t.Fatalf("excluded record basis altered: %+v", re)
	}
	// 本次失败保存库存依据：新增占用前的 10/4/6。
	if rec.StockBasis == nil ||
		rec.StockBasis.PhysicalRemaining != 10 ||
		rec.StockBasis.ActiveOccupied != 4 ||
		rec.StockBasis.Committable != 6 {
		t.Fatalf("excluded record stock basis = %+v, want 10/4/6", rec.StockBasis)
	}

	// 原请求的产品编号与故障代码仍保留为提交时的内容。
	rq, err := s.Request(lpNewReq)
	if err != nil {
		t.Fatalf("request lookup: %v", err)
	}
	if rq.ID != lpNewReq || rq.ProductID != "p-late-new" || rq.FaultCode != "NOISE" {
		t.Fatalf("request identity altered: %+v", rq)
	}

	// 另一张请求的四件占用始终保持原值。
	other, err := s.RequestHistory(lpOtherReq)
	if err != nil {
		t.Fatalf("other history: %v", err)
	}
	if len(other) != 1 || !other[0].Success || other[0].Quantity != 4 {
		t.Fatalf("other request history altered: %+v", other)
	}
}
