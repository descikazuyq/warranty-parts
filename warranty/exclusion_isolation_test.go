package warranty

import (
	"errors"
	"testing"
)

// hasReason 报告拒绝原因列表中是否包含指定原因。
func hasReason(reasons []RejectionReason, want RejectionReason) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// onlyCode 报告集合是否恰好只含指定代码。
func onlyCode(codes map[string]struct{}, want string) bool {
	if len(codes) != 1 {
		return false
	}
	_, ok := codes[want]
	return ok
}

// 登记成功后修改调用方持有的原清单（替换或补入代码），已登记产品的
// 除外规则仍以登记当时为准：原被除外代码仍不合格，替换/补入的代码不受影响。
func TestExclusionListSnapshotAtRegistration(t *testing.T) {
	s := NewStore()
	codes := []string{"FAULTX"}
	if err := s.RegisterProduct("p1", t0, 30, codes); err != nil {
		t.Fatalf("register product: %v", err)
	}

	// 调用方随后替换原清单中的代码，并补入新代码。
	codes[0] = "FAULTZ"
	codes = append(codes, "FAULTY")

	// 查询产品仍看到最初登记的除外规则。
	p, err := s.Product("p1")
	if err != nil {
		t.Fatalf("get product: %v", err)
	}
	if !onlyCode(p.ExcludedCodes, "FAULTX") {
		t.Fatalf("excluded codes changed by caller mutation: %v", p.ExcludedCodes)
	}

	// 原先被除外的故障在保修期内仍不合格，且明确列出故障除外原因。
	if err := s.SubmitRequest("r-excluded", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit excluded request: %v", err)
	}
	e, err := s.Evaluate("r-excluded", nowOK)
	if err != nil {
		t.Fatalf("evaluate excluded: %v", err)
	}
	if e.Eligible || !e.Excluded || !hasReason(e.Reasons, ReasonFaultExcluded) {
		t.Fatalf("originally excluded fault must stay ineligible: %+v", e)
	}

	// 后来替换或补入、原先没有被除外的代码在同样的保修期内应合格。
	for i, code := range []string{"FAULTZ", "FAULTY"} {
		id := "r-added-" + code
		if err := s.SubmitRequest(id, "p1", code); err != nil {
			t.Fatalf("submit request %d: %v", i, err)
		}
		e, err := s.Evaluate(id, nowOK)
		if err != nil {
			t.Fatalf("evaluate %s: %v", code, err)
		}
		if !e.Eligible || e.Excluded || len(e.Reasons) != 0 {
			t.Fatalf("later-added code %s must stay eligible: %+v", code, e)
		}
	}
}

// 修改取回的产品资料（删除原代码、加入新代码）不影响已登记规则；
// 重新取回的资料、此前取回的另一份资料以及请求资格都保持登记时的状态；
// 直接查询资格与按请求查看资格的结论相互一致。
func TestRetrievedProductMutationDoesNotChangeRules(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.SubmitRequest("r-excluded", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit excluded request: %v", err)
	}
	if err := s.SubmitRequest("r-new", "p1", "FAULTNEW"); err != nil {
		t.Fatalf("submit new-code request: %v", err)
	}

	// 先取回两份资料，再修改其中一份：删除原代码、加入新代码。
	mutated, err := s.Product("p1")
	if err != nil {
		t.Fatalf("get product: %v", err)
	}
	other, err := s.Product("p1")
	if err != nil {
		t.Fatalf("get product again: %v", err)
	}
	delete(mutated.ExcludedCodes, "FAULTX")
	mutated.ExcludedCodes["FAULTNEW"] = struct{}{}

	// 重新取回的资料仍反映登记时的规则。
	fresh, err := s.Product("p1")
	if err != nil {
		t.Fatalf("re-get product: %v", err)
	}
	if !onlyCode(fresh.ExcludedCodes, "FAULTX") {
		t.Fatalf("stored rules changed by mutating retrieved copy: %v", fresh.ExcludedCodes)
	}
	// 此前取回的另一份资料也不应跟着改变。
	if !onlyCode(other.ExcludedCodes, "FAULTX") {
		t.Fatalf("previously retrieved copy changed: %v", other.ExcludedCodes)
	}

	// 直接查询资格：原代码仍除外，新加入的代码仍合格。
	eExcluded, err := s.Evaluate("r-excluded", nowOK)
	if err != nil {
		t.Fatalf("evaluate excluded: %v", err)
	}
	if eExcluded.Eligible || !eExcluded.Excluded || !hasReason(eExcluded.Reasons, ReasonFaultExcluded) {
		t.Fatalf("excluded fault must stay ineligible: %+v", eExcluded)
	}
	eNew, err := s.Evaluate("r-new", nowOK)
	if err != nil {
		t.Fatalf("evaluate new code: %v", err)
	}
	if !eNew.Eligible || eNew.Excluded || len(eNew.Reasons) != 0 {
		t.Fatalf("caller-added code must stay eligible: %+v", eNew)
	}

	// 按请求查看资格与直接查询一致：合格标记、是否命中除外、拒绝原因
	// 不能出现资料显示已除外而请求却被放行的矛盾。
	for _, tc := range []struct {
		id string
		ev *Eligibility
	}{
		{"r-excluded", eExcluded},
		{"r-new", eNew},
	} {
		view, err := s.RequestView(tc.id, nowOK)
		if err != nil {
			t.Fatalf("request view %s: %v", tc.id, err)
		}
		ve := view.Eligibility
		if ve.Eligible != tc.ev.Eligible || ve.Excluded != tc.ev.Excluded ||
			len(ve.Reasons) != len(tc.ev.Reasons) {
			t.Fatalf("view/evaluate mismatch for %s: view %+v, evaluate %+v", tc.id, ve, tc.ev)
		}
		for i := range ve.Reasons {
			if ve.Reasons[i] != tc.ev.Reasons[i] {
				t.Fatalf("reason mismatch for %s: view %v, evaluate %v", tc.id, ve.Reasons, tc.ev.Reasons)
			}
		}
	}
}

// 备件预留结果同样以登记时的除外清单为准：调用方修改清单后，原先被除外
// 的请求仍返回不合格错误、不创建承诺、不占用备件；未被登记清单除外的
// 合格请求仍可正常预留。
func TestReserveIsolationForExcludedAndEligible(t *testing.T) {
	s := NewStore()
	codes := []string{"FAULTX"}
	if err := s.RegisterProduct("p1", t0, 30, codes); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r-excluded", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit excluded request: %v", err)
	}
	if err := s.SubmitRequest("r-added", "p1", "FAULTZ"); err != nil {
		t.Fatalf("submit added-code request: %v", err)
	}

	// 调用方修改自己持有的清单：替换原代码，并往取回的资料里补入代码。
	codes[0] = "FAULTZ"
	p, err := s.Product("p1")
	if err != nil {
		t.Fatalf("get product: %v", err)
	}
	p.ExcludedCodes["FAULTZ"] = struct{}{}

	// 原先被除外的请求：备件存在、数量足够、到期时刻合法，仍不合格。
	if _, err := s.Reserve("c-excluded", "r-excluded", "part1", 3, expiryOK, nowOK); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve excluded: got %v, want ErrIneligible", err)
	}
	// 不创建承诺、不占用备件。
	if _, err := s.Commitment("c-excluded"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("excluded reserve must not create commitment: got %v", err)
	}
	ps, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if ps.ActiveOccupied != 0 || ps.Committable != 10 || len(ps.Details) != 0 {
		t.Fatalf("excluded reserve must not occupy stock: %+v", ps)
	}

	// 未被登记清单除外的合格请求仍可正常预留。
	c, err := s.Reserve("c-added", "r-added", "part1", 3, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve eligible request: %v", err)
	}
	if c.Quantity != 3 || c.Unused() != 3 {
		t.Fatalf("unexpected commitment: %+v", c)
	}
	ps, err = s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if ps.ActiveOccupied != 3 || ps.Committable != 7 {
		t.Fatalf("eligible reserve must occupy stock: %+v", ps)
	}
}

// 产品以空除外清单登记时，调用方往取回的集合里加入代码，
// 不能让该产品开始拒绝这个故障。
func TestEmptyExclusionListStaysEmpty(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 5); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit request: %v", err)
	}

	// 调用方往取回的集合里加入代码。
	p, err := s.Product("p1")
	if err != nil {
		t.Fatalf("get product: %v", err)
	}
	p.ExcludedCodes["FAULTX"] = struct{}{}

	// 该产品不能因此开始拒绝这个故障。
	e, err := s.Evaluate("r1", nowOK)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !e.Eligible || e.Excluded || len(e.Reasons) != 0 {
		t.Fatalf("empty exclusion list must stay empty: %+v", e)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 2, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve must stay eligible: %v", err)
	}
}

// 故障代码沿用现有的精确匹配方式：不做大小写转换或代码修剪，
// 大小写不同或带空白的代码不命中已登记的除外代码。
func TestExclusionMatchingStaysExact(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	for i, code := range []string{"faultx", "Faultx", " FAULTX", "FAULTX "} {
		id := string(rune('a' + i))
		if err := s.SubmitRequest(id, "p1", code); err != nil {
			t.Fatalf("submit %q: %v", code, err)
		}
		e, err := s.Evaluate(id, nowOK)
		if err != nil {
			t.Fatalf("evaluate %q: %v", code, err)
		}
		if !e.Eligible || e.Excluded {
			t.Fatalf("code %q must not match excluded FAULTX: %+v", code, e)
		}
	}
}
