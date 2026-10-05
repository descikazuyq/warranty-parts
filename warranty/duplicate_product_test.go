package warranty

import (
	"errors"
	"math"
	"testing"
	"time"
)

// 重复登记的保护：已登记的非空编号再次提交时一律报 ErrDuplicateID，
// 无论新条款是否合法；原产品的购买时刻、保修天数和除外集合完整保留，
// 资格判断、预留与历史依据都继续按第一次成功登记的条款执行。

// checkProductTerms 校验取回的产品资料与首次登记的条款完全一致。
func checkProductTerms(t *testing.T, s *Store, id string, purchase time.Time, days int, codes ...string) {
	t.Helper()
	p, err := s.Product(id)
	if err != nil {
		t.Fatalf("product %q: %v", id, err)
	}
	if !p.PurchaseTime.Equal(purchase) {
		t.Fatalf("product %q purchase time = %v, want %v", id, p.PurchaseTime, purchase)
	}
	if p.WarrantyDays != days {
		t.Fatalf("product %q warranty days = %d, want %d", id, p.WarrantyDays, days)
	}
	if len(p.ExcludedCodes) != len(codes) {
		t.Fatalf("product %q excluded codes = %v, want exactly %v", id, p.ExcludedCodes, codes)
	}
	for _, c := range codes {
		if _, ok := p.ExcludedCodes[c]; !ok {
			t.Fatalf("product %q excluded codes = %v, missing %q", id, p.ExcludedCodes, c)
		}
	}
}

// 新条款与旧条款明显不同（购买时刻、保修天数、除外清单全部改变）时，
// 重复登记仍报 ErrDuplicateID，原记录三项条款完整保留，不是部分保留。
func TestDuplicateProductDifferentTermsKeepsOriginal(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX", "FAULTY"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	// 三项条款全部不同的合法新提交。
	err := s.RegisterProduct("p1", t0.Add(5*day), 300, []string{"FAULTZ"})
	if !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate with different terms: got %v, want ErrDuplicateID", err)
	}
	// 原条款完整保留：购买时刻、天数、除外集合都不被新提交改动。
	checkProductTerms(t, s, "p1", t0, 30, "FAULTX", "FAULTY")
}

// 重复编号同时带有非法条款时，编号重复优先于参数校验：报 ErrDuplicateID
// 而不是 ErrInvalidParam，原记录不被清空或部分替换。
func TestDuplicateProductInvalidTermsStillDuplicate(t *testing.T) {
	cases := []struct {
		name     string
		purchase time.Time
		days     int
		codes    []string
	}{
		{"zero days", t0, 0, nil},
		{"negative days", t0, -5, nil},
		{"unrepresentable expiry", t0, math.MaxInt, nil},
		{"empty excluded code", t0, 10, []string{""}},
		{"empty code among valid", t0, 10, []string{"FAULTZ", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
				t.Fatalf("register product: %v", err)
			}
			err := s.RegisterProduct("p1", tc.purchase, tc.days, tc.codes)
			if !errors.Is(err, ErrDuplicateID) {
				t.Fatalf("duplicate with invalid terms: got %v, want ErrDuplicateID", err)
			}
			if errors.Is(err, ErrInvalidParam) {
				t.Fatalf("duplicate id must win over param validation, got %v", err)
			}
			checkProductTerms(t, s, "p1", t0, 30, "FAULTX")
		})
	}
}

// 重复登记被拒绝后，资格继续按原购买时刻与原期限判断：按原条款仍在保的
// 请求不因新期限较短而被拒绝；按原条款已过保的请求不借较长的新期限重新
// 合格。原清单中的除外故障仍被拒绝，仅出现在新清单中的故障不构成除外。
// 直接查询资格与按请求查看资格依据都显示原保修条款。
func TestDuplicateRejectionKeepsOriginalEligibility(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	for _, r := range []struct{ id, fault string }{
		{"rPlain", "FAULTY"},
		{"rExcluded", "FAULTX"},
		{"rNewOnly", "FAULTZ"},
	} {
		if err := s.SubmitRequest(r.id, "p1", r.fault); err != nil {
			t.Fatalf("submit %s: %v", r.id, err)
		}
	}
	// 较短的新期限（5 天）加一份不同的除外清单：登记被拒绝。
	if err := s.RegisterProduct("p1", t0, 5, []string{"FAULTZ"}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate with shorter term: got %v, want ErrDuplicateID", err)
	}

	// 第 10 天：按原 30 天期限仍在保，按新 5 天期限已过保。
	at := t0.Add(10 * day)
	e, err := s.Evaluate("rPlain", at)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !e.Eligible {
		t.Fatalf("still in warranty under original terms, got reasons %v", e.Reasons)
	}
	if e.WarrantyDays != 30 || !e.PurchaseTime.Equal(t0) || !e.WarrantyExpiry.Equal(t0.Add(30*day)) {
		t.Fatalf("eligibility basis = (%v, %d, %v), want original (%v, 30, %v)",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, t0, t0.Add(30*day))
	}
	// 原清单中的除外故障仍被拒绝。
	e, err = s.Evaluate("rExcluded", at)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || !containsReason(e.Reasons, ReasonFaultExcluded) {
		t.Fatalf("original exclusion lost: %+v", e)
	}
	// 仅出现在新清单中的故障不能成为除外。
	e, err = s.Evaluate("rNewOnly", at)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !e.Eligible {
		t.Fatalf("fault only in rejected new list must not exclude, got reasons %v", e.Reasons)
	}

	// 较长的新期限（300 天）：登记同样被拒绝。
	if err := s.RegisterProduct("p1", t0, 300, nil); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate with longer term: got %v, want ErrDuplicateID", err)
	}
	// 第 40 天：按原 30 天期限已过保，不能借新期限重新合格。
	later := t0.Add(40 * day)
	e, err = s.Evaluate("rPlain", later)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || !containsReason(e.Reasons, ReasonWarrantyExpired) {
		t.Fatalf("expired under original terms must stay ineligible, got %+v", e)
	}
	if e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(t0.Add(30*day)) {
		t.Fatalf("eligibility basis changed: %+v", e)
	}

	// 按请求查看资格依据：同样显示原保修条款与对应拒绝原因。
	view, err := s.RequestView("rPlain", later)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	ve := view.Eligibility
	if ve.Eligible || !containsReason(ve.Reasons, ReasonWarrantyExpired) {
		t.Fatalf("view eligibility = %+v, want expired under original terms", ve)
	}
	if ve.WarrantyDays != 30 || !ve.PurchaseTime.Equal(t0) || !ve.WarrantyExpiry.Equal(t0.Add(30*day)) {
		t.Fatalf("view basis = (%v, %d, %v), want original terms",
			ve.PurchaseTime, ve.WarrantyDays, ve.WarrantyExpiry)
	}
	view, err = s.RequestView("rExcluded", at)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if view.Eligibility.Eligible || !containsReason(view.Eligibility.Reasons, ReasonFaultExcluded) {
		t.Fatalf("view for excluded fault = %+v", view.Eligibility)
	}
}

// 备件预留同样只认原条款：按原条款在保且未命中除外的请求，在库存足够、
// 承诺到期时刻晚于当前时刻时仍可预留；按原条款不合格的请求仍返回
// ErrIneligible，不生成承诺、不增加库存占用。重复登记失败不改动已有承诺
// 与已有预留历史中的资格依据。
func TestDuplicateRejectionKeepsReserveBehavior(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("rOK", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// 重复登记失败前已有一笔成功预留。
	if _, err := s.Reserve("c1", "rOK", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	// 重复登记失败：新期限更短、除外清单不同。
	if err := s.RegisterProduct("p1", t0, 5, []string{"FAULTY"}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate: got %v, want ErrDuplicateID", err)
	}

	// 按原条款在保的请求（即使故障代码出现在新清单中）仍可预留。
	if _, err := s.Reserve("c2", "rOK", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve after rejected duplicate: %v", err)
	}
	// 按原条款命中除外的请求仍不合格：不生成承诺、不增加占用。
	if _, err := s.Reserve("c3", "rBad", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve for excluded fault: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("c3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected reserve must not create a commitment, got %v", err)
	}
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.ActiveOccupied != 8 || st.Committable != 2 || st.PhysicalRemaining != 10 {
		t.Fatalf("stock = phys %d occupied %d committable %d, want 10/8/2",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 已有承诺不被重复登记失败改动。
	c1, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("commitment c1: %v", err)
	}
	if c1.Quantity != 4 || c1.Used != 0 || c1.Canceled || c1.Expired {
		t.Fatalf("existing commitment changed: %+v", c1)
	}

	// 已有预留历史中的资格依据仍显示原条款。
	hist, err := s.RequestHistory("rOK")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 2 || !hist[0].Success || !hist[1].Success {
		t.Fatalf("unexpected history: %+v", hist)
	}
	basis := hist[0].Eligibility
	if basis == nil {
		t.Fatal("first success record missing eligibility basis")
	}
	if basis.WarrantyDays != 30 || !basis.PurchaseTime.Equal(t0) || !basis.WarrantyExpiry.Equal(t0.Add(30*day)) {
		t.Fatalf("history basis = (%v, %d, %v), want original terms",
			basis.PurchaseTime, basis.WarrantyDays, basis.WarrantyExpiry)
	}
	// 不合格请求的失败记录也保留原条款依据与拒绝原因。
	histBad, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(histBad) != 1 || histBad[0].Success || histBad[0].Error != HistoryErrorIneligible {
		t.Fatalf("unexpected history for excluded request: %+v", histBad)
	}
	basis = histBad[0].Eligibility
	if basis == nil || basis.WarrantyDays != 30 || !containsReason(basis.Reasons, ReasonFaultExcluded) {
		t.Fatalf("rejected record basis = %+v, want original terms with fault_code_excluded", basis)
	}
}

// 区分重复编号和首次登记失败：尚未登记的非空编号提交非法条款时仍返回
// ErrInvalidParam，不留下产品记录；随后用同一编号提交合法条款正常登记。
func TestFirstRegistrationFailureThenValidRegistration(t *testing.T) {
	s := NewStore()
	invalid := []struct {
		name  string
		days  int
		codes []string
	}{
		{"zero days", 0, nil},
		{"negative days", -1, nil},
		{"unrepresentable expiry", math.MaxInt, nil},
		{"empty excluded code", 10, []string{""}},
	}
	for _, tc := range invalid {
		err := s.RegisterProduct("pNew", t0, tc.days, tc.codes)
		if !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("%s: got %v, want ErrInvalidParam", tc.name, err)
		}
		if errors.Is(err, ErrDuplicateID) {
			t.Fatalf("%s: unregistered id must not report duplicate, got %v", tc.name, err)
		}
		if _, err := s.Product("pNew"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: failed first registration left a record, got %v", tc.name, err)
		}
	}
	// 同一编号随后以合法条款正常登记。
	if err := s.RegisterProduct("pNew", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register after failed attempts: %v", err)
	}
	checkProductTerms(t, s, "pNew", t0, 30, "FAULTX")
	// 登记成功后再提交才报重复。
	if err := s.RegisterProduct("pNew", t0, 30, []string{"FAULTX"}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate after successful registration: got %v, want ErrDuplicateID", err)
	}
}
