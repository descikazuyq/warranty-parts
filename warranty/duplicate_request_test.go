package warranty

import (
	"errors"
	"testing"
)

// 重复提交请求的保护：请求首次成功提交后，用同一非空编号再次提交一律报
// ErrDuplicateID——无论新产品编号、故障代码是否与原请求相同，即使新故障
// 代码为空也优先报编号重复，errors.Is 不能把它识别为参数错误。被拒绝后
// 原请求的产品编号、故障代码完整保留，资格依据、关联承诺与预留历史都继续
// 属于第一次成功提交的内容。

// duplicateRequestStore 构造两个保修条款不同的产品和一个库存 10 的备件：
// p1 保修 30 天、除外 FAULTX；p2 保修 300 天、除外 FAULTZ。
func duplicateRequestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register p1: %v", err)
	}
	if err := s.RegisterProduct("p2", t0, 300, []string{"FAULTZ"}); err != nil {
		t.Fatalf("register p2: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	return s
}

// assertDuplicateRequest 断言再次提交报 ErrDuplicateID 且绝不被识别为参数错误。
func assertDuplicateRequest(t *testing.T, s *Store, id, productID, faultCode string) {
	t.Helper()
	err := s.SubmitRequest(id, productID, faultCode)
	if !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("resubmit %q with (%q,%q): got %v, want ErrDuplicateID", id, productID, faultCode, err)
	}
	if errors.Is(err, ErrInvalidParam) {
		t.Fatalf("duplicate id must win over param validation, got %v", err)
	}
}

// 原故障命中原产品除外清单时，把重复提交换成另一个在保产品与未被除外的
// 故障、或只把故障代码改成空串，都必须报编号重复；原请求资料与除外结论
// 完整保留，不能借新内容获得资格。
func TestDuplicateRequestRejectedContentNeverApplied(t *testing.T) {
	s := duplicateRequestStore(t)
	if err := s.SubmitRequest("rExcl", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 同一编号的各种再提交：换在保产品与未除外故障、原样重提、空故障代码。
	assertDuplicateRequest(t, s, "rExcl", "p2", "FAULTQ")
	assertDuplicateRequest(t, s, "rExcl", "p1", "FAULTX")
	assertDuplicateRequest(t, s, "rExcl", "p2", "")
	assertDuplicateRequest(t, s, "rExcl", "", "")

	// 原请求资料完整保留第一次成功提交的产品与故障。
	r, err := s.Request("rExcl")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if r.ProductID != "p1" || r.FaultCode != "FAULTX" {
		t.Fatalf("request changed to %+v, want {p1 FAULTX}", r)
	}

	// 资格依据继续来自原产品、按原故障判断：仍命中除外。
	e, err := s.Evaluate("rExcl", nowOK)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || !e.Excluded || e.ProductID != "p1" || e.FaultCode != "FAULTX" ||
		e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(t0.Add(30*day)) {
		t.Fatalf("eligibility = %+v, want original p1/FAULTX excluded", e)
	}
	view, err := s.RequestView("rExcl", nowOK)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	ve := view.Eligibility
	if ve.Eligible || !ve.Excluded || !containsReason(ve.Reasons, ReasonFaultExcluded) ||
		ve.ProductID != "p1" || ve.FaultCode != "FAULTX" {
		t.Fatalf("view eligibility = %+v, want original excluded basis", ve)
	}

	// 发起新的备件预留仍按原请求资料判断：原故障除外，不合格且不占用。
	if _, err := s.Reserve("c1", "rExcl", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve after rejected duplicate: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected request reserve must not create a commitment, got %v", err)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 0 || st.Committable != 10 {
		t.Fatalf("ineligible reserve occupied stock: %+v", st)
	}
}

// 原请求本来合格时，被拒绝的新内容（换产品、换除外故障、空故障代码）不能
// 把它改成除外或找不到产品；保修时间仍按查询或预留当次给定的时刻判断。
func TestDuplicateRequestEligibleStaysEligible(t *testing.T) {
	s := duplicateRequestStore(t)
	if err := s.SubmitRequest("rOK", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// 新内容若生效会变成另一产品的除外故障，或因空故障代码而参数非法。
	assertDuplicateRequest(t, s, "rOK", "p2", "FAULTZ")
	assertDuplicateRequest(t, s, "rOK", "p1", "FAULTX")
	assertDuplicateRequest(t, s, "rOK", "pUnregistered", "")

	r, err := s.Request("rOK")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if r.ProductID != "p1" || r.FaultCode != "FAULTY" {
		t.Fatalf("request changed to %+v, want {p1 FAULTY}", r)
	}

	// 保修期内按当次时刻仍合格，依据来自原产品 p1。
	e, err := s.Evaluate("rOK", nowOK)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !e.Eligible || e.ProductID != "p1" || e.FaultCode != "FAULTY" || e.WarrantyDays != 30 {
		t.Fatalf("eligibility = %+v, want eligible on original p1 terms", e)
	}

	// 预留先成功一笔并部分使用，再重复提交，验证承诺与库存不受影响。
	c, err := s.Reserve("c2", "rOK", "part1", 4, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if c.Quantity != 4 || c.Used != 0 {
		t.Fatalf("commitment: %+v", c)
	}
	if _, err := s.Use("u1", "c2", 1, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	assertDuplicateRequest(t, s, "rOK", "p2", "")
	assertDuplicateRequest(t, s, "rOK", "p1", "FAULTX")

	got, err := s.Commitment("c2")
	if err != nil {
		t.Fatalf("commitment: %v", err)
	}
	if got.PartID != "part1" || got.Quantity != 4 || got.Used != 1 || got.Unused() != 3 {
		t.Fatalf("commitment changed after rejected duplicate: %+v", got)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 9 || st.ActiveOccupied != 3 || st.Committable != 6 {
		t.Fatalf("stock after rejected duplicate = %+v, want phys 9 / occupied 3 / committable 6", st)
	}
	view, _ := s.RequestView("rOK", nowOK)
	if len(view.Commitments) != 1 {
		t.Fatalf("commitments view = %+v, want only c2", view.Commitments)
	}
	d := view.Commitments[0]
	if d.CommitmentID != "c2" || d.PartID != "part1" || d.OriginalQuantity != 4 ||
		d.UsedQuantity != 1 || d.RemainingQuantity != 3 {
		t.Fatalf("commitment detail changed: %+v", d)
	}

	// 重复提交不新增任何预留处理历史，也不清空、重挂已有记录。
	hist, err := s.RequestHistory("rOK")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 1 || !hist[0].Success || hist[0].CommitID != "c2" || hist[0].Seq != 1 {
		t.Fatalf("history changed after rejected duplicate: %+v", hist)
	}

	// 保修时间仍按预留当次给定的时刻判断：第 31 天按原 p1 的 30 天期限已过保，
	// 不能借被拒绝内容里 p2 的 300 天期限合格。
	at := t0.Add(31 * day)
	e2, err := s.Evaluate("rOK", at)
	if err != nil {
		t.Fatalf("evaluate at day 31: %v", err)
	}
	if e2.Eligible || !containsReason(e2.Reasons, ReasonWarrantyExpired) || e2.WarrantyDays != 30 {
		t.Fatalf("eligibility at day 31 = %+v, want warranty_expired on original terms", e2)
	}
	if _, err := s.Reserve("c3", "rOK", "part1", 1, t0.Add(400*day), at); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve after original warranty expired: got %v, want ErrIneligible", err)
	}
}

// 首次提交的边界保持不变：空请求编号或空故障代码返回 ErrInvalidParam，
// 失败后不留下请求；产品尚未登记时不做产品存在校验，非空故障代码的请求
// 仍可先提交，资格查询与预留再按现有规则报告产品不存在。
func TestFirstSubmitBoundariesUnchanged(t *testing.T) {
	s := duplicateRequestStore(t)

	// 空请求编号：参数错误，且不会因为某个“空编号记录”而变成重复。
	if err := s.SubmitRequest("", "p1", "F"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v, want ErrInvalidParam", err)
	}
	if err := s.SubmitRequest("", "p1", ""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty id and fault: got %v, want ErrInvalidParam", err)
	}

	// 空故障代码：参数错误，不留下请求。
	if err := s.SubmitRequest("rFresh", "p1", ""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty fault code: got %v, want ErrInvalidParam", err)
	}
	if errors.Is(s.SubmitRequest("rFresh", "p1", ""), ErrDuplicateID) {
		t.Fatal("failed first submit must not leave a request")
	}
	if _, err := s.Request("rFresh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed submit left a request: %v", err)
	}
	// 同一非空编号随后以合法故障代码仍可成功提交，成功后再提交才报重复。
	if err := s.SubmitRequest("rFresh", "p1", "FAULTY"); err != nil {
		t.Fatalf("valid submit after failed attempt: %v", err)
	}
	if err := s.SubmitRequest("rFresh", "p1", ""); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("resubmit existing id with empty fault: got %v, want ErrDuplicateID", err)
	}

	// 产品尚未登记：提交不校验产品存在性，仍可先成功提交。
	if err := s.SubmitRequest("rMissing", "pUnregistered", "F"); err != nil {
		t.Fatalf("submit before product registered: %v", err)
	}
	if _, err := s.Evaluate("rMissing", nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evaluate with missing product: got %v, want ErrNotFound", err)
	}
	if _, err := s.Reserve("c-missing", "rMissing", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve with missing product: got %v, want ErrNotFound", err)
	}
	// 产品缺失的失败留痕按现有规则记录，资格依据为空。
	hist, err := s.RequestHistory("rMissing")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 1 || hist[0].Success || hist[0].Error != HistoryErrorProductNotFound || hist[0].Eligibility != nil {
		t.Fatalf("missing-product history = %+v, want one product_not_found record with nil basis", hist)
	}
}
