package warranty

import (
	"errors"
	"strings"
	"testing"
)

// 本文件回归保障除外故障代码清单的数据隔离：产品一经登记，保修规则以登记
// 当时的清单快照为准；调用方随后修改自己持有的登记入参或取回的产品资料，
// 都不能改变已登记产品的资格判断与备件预留结果。包不提供修改保修条款的
// 操作，这些测试只沿用登记、取回、查询与预留等既有公开行为。

// isolationStore 构造一个产品（除外 BROKEN_SEAL）、库存 10 的备件和若干请求。
func isolationStore(t *testing.T, excluded []string) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, excluded); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	return s
}

// mustSubmit 提交一个保修请求。
func mustSubmit(t *testing.T, s *Store, id, faultCode string) {
	t.Helper()
	if err := s.SubmitRequest(id, "p1", faultCode); err != nil {
		t.Fatalf("submit request %s: %v", id, err)
	}
}

// assertExcludedCodes 断言取回的产品资料中除外代码集合恰好等于 want。
func assertExcludedCodes(t *testing.T, p Product, want ...string) {
	t.Helper()
	if len(p.ExcludedCodes) != len(want) {
		t.Fatalf("excluded codes = %v, want exactly %v", p.ExcludedCodes, want)
	}
	for _, c := range want {
		if _, ok := p.ExcludedCodes[c]; !ok {
			t.Fatalf("excluded codes = %v, want to contain %q", p.ExcludedCodes, c)
		}
	}
}

// assertExcludedInWindow 断言请求在保修期内被故障除外原因拒绝，且直接查询
// 与按请求查看的结果一致。
func assertExcludedInWindow(t *testing.T, s *Store, requestID string) {
	t.Helper()
	e, err := s.Evaluate(requestID, nowOK)
	if err != nil {
		t.Fatalf("evaluate %s: %v", requestID, err)
	}
	if e.Eligible || !e.Excluded {
		t.Fatalf("%s: want ineligible and excluded, got %+v", requestID, e)
	}
	if len(e.Reasons) != 1 || e.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("%s: reasons = %v, want only [fault_code_excluded]", requestID, e.Reasons)
	}
	view, err := s.RequestView(requestID, nowOK)
	if err != nil {
		t.Fatalf("request view %s: %v", requestID, err)
	}
	ve := view.Eligibility
	if ve.Eligible != e.Eligible || ve.Excluded != e.Excluded || len(ve.Reasons) != len(e.Reasons) {
		t.Fatalf("%s: view %+v disagrees with evaluate %+v", requestID, ve, e)
	}
	for i := range e.Reasons {
		if ve.Reasons[i] != e.Reasons[i] {
			t.Fatalf("%s: view reasons %v disagree with %v", requestID, ve.Reasons, e.Reasons)
		}
	}
}

// assertEligibleInWindow 断言请求在保修期内合格、未命中除外，且直接查询
// 与按请求查看的结果一致。
func assertEligibleInWindow(t *testing.T, s *Store, requestID string) {
	t.Helper()
	e, err := s.Evaluate(requestID, nowOK)
	if err != nil {
		t.Fatalf("evaluate %s: %v", requestID, err)
	}
	if !e.Eligible || e.Excluded || len(e.Reasons) != 0 {
		t.Fatalf("%s: want eligible with no reasons, got %+v", requestID, e)
	}
	view, err := s.RequestView(requestID, nowOK)
	if err != nil {
		t.Fatalf("request view %s: %v", requestID, err)
	}
	ve := view.Eligibility
	if ve.Eligible != e.Eligible || ve.Excluded != e.Excluded || len(ve.Reasons) != len(e.Reasons) {
		t.Fatalf("%s: view %+v disagrees with evaluate %+v", requestID, ve, e)
	}
}

// TestRegistrationSnapshotsCallerList 登记成功后以登记当时的清单为准：
// 调用方替换或补入自己持有的原清单，不改变已登记产品的除外规则。
func TestRegistrationSnapshotsCallerList(t *testing.T) {
	t.Run("replace original code", func(t *testing.T) {
		codes := []string{"BROKEN_SEAL"}
		s := isolationStore(t, codes)
		mustSubmit(t, s, "rOrig", "BROKEN_SEAL")
		mustSubmit(t, s, "rReplaced", "NOISE")

		// 登记完成后把原清单中的除外代码替换成另一代码。
		codes[0] = "NOISE"
		p, err := s.Product("p1")
		if err != nil {
			t.Fatalf("product: %v", err)
		}
		assertExcludedCodes(t, p, "BROKEN_SEAL")
		// 原除外代码仍被拒绝；替换进来的代码原本不在清单中，应合格。
		assertExcludedInWindow(t, s, "rOrig")
		assertEligibleInWindow(t, s, "rReplaced")
	})

	t.Run("append code to caller list", func(t *testing.T) {
		codes := make([]string, 1, 4)
		codes[0] = "BROKEN_SEAL"
		s := isolationStore(t, codes)
		mustSubmit(t, s, "rOrig", "BROKEN_SEAL")
		mustSubmit(t, s, "rAdded", "LEAK")

		// 登记完成后向原清单补入代码（含就地扩容的情形）。
		codes = append(codes, "LEAK")
		codes = append(codes, "SHORT")
		p, err := s.Product("p1")
		if err != nil {
			t.Fatalf("product: %v", err)
		}
		assertExcludedCodes(t, p, "BROKEN_SEAL")
		assertExcludedInWindow(t, s, "rOrig")
		assertEligibleInWindow(t, s, "rAdded")
	})
}

// TestRetrievedProductMutationIsIsolated 取回产品资料后删除原有代码、加入
// 新代码：重新取回的资料、先前取得的另一份资料以及请求资格都继续反映
// 登记时的规则。
func TestRetrievedProductMutationIsIsolated(t *testing.T) {
	s := isolationStore(t, []string{"BROKEN_SEAL"})
	mustSubmit(t, s, "rOrig", "BROKEN_SEAL")
	mustSubmit(t, s, "rAddedLater", "LATER_CODE")

	first, err := s.Product("p1")
	if err != nil {
		t.Fatalf("first product fetch: %v", err)
	}
	retrieved, err := s.Product("p1")
	if err != nil {
		t.Fatalf("second product fetch: %v", err)
	}
	// 调用方从返回的集合中删除原有代码，再加入一个新代码。
	delete(retrieved.ExcludedCodes, "BROKEN_SEAL")
	retrieved.ExcludedCodes["LATER_CODE"] = struct{}{}

	// 重新取回仍反映登记时规则。
	again, err := s.Product("p1")
	if err != nil {
		t.Fatalf("re-fetch product: %v", err)
	}
	assertExcludedCodes(t, again, "BROKEN_SEAL")
	// 此前已经取得的另一份资料不跟着改变。
	assertExcludedCodes(t, first, "BROKEN_SEAL")
	if _, leaked := first.ExcludedCodes["LATER_CODE"]; leaked {
		t.Fatalf("earlier product copy gained added code: %+v", first.ExcludedCodes)
	}

	// 资格不被调用方对取回资料的修改影响：原有代码仍拒绝，新代码仍合格。
	assertExcludedInWindow(t, s, "rOrig")
	assertEligibleInWindow(t, s, "rAddedLater")
}

// TestEvaluateAndRequestViewStayConsistentAfterMutation 直接查询资格与按
// 请求查看资格时，合格标记、是否命中除外、拒绝原因必须相互一致，不能出现
// 资料显示已除外而请求被放行（或反之）的情况。
func TestEvaluateAndRequestViewStayConsistentAfterMutation(t *testing.T) {
	s := isolationStore(t, []string{"BROKEN_SEAL", "OVERHEAT"})
	mustSubmit(t, s, "r1", "BROKEN_SEAL")
	mustSubmit(t, s, "r2", "OVERHEAT")
	mustSubmit(t, s, "r3", "NOISE")

	p, _ := s.Product("p1")
	p.ExcludedCodes["NOISE"] = struct{}{}
	delete(p.ExcludedCodes, "BROKEN_SEAL")

	for _, id := range []string{"r1", "r2"} {
		assertExcludedInWindow(t, s, id)
	}
	assertEligibleInWindow(t, s, "r3")

	// 过保时刻：原除外代码仍同时列出过保与除外两项，两种查询口径一致。
	e, err := s.Evaluate("r1", t0.Add(31*day))
	if err != nil {
		t.Fatalf("evaluate after expiry: %v", err)
	}
	if e.Eligible || !e.Excluded || len(e.Reasons) != 2 {
		t.Fatalf("expired+excluded: got %+v", e)
	}
	got := map[RejectionReason]bool{}
	for _, r := range e.Reasons {
		got[r] = true
	}
	if !got[ReasonWarrantyExpired] || !got[ReasonFaultExcluded] {
		t.Fatalf("reasons = %v, want warranty_expired and fault_code_excluded", e.Reasons)
	}
	view, _ := s.RequestView("r1", t0.Add(31*day))
	if view.Eligibility.Eligible || !view.Eligibility.Excluded ||
		len(view.Eligibility.Reasons) != 2 {
		t.Fatalf("view after expiry disagrees: %+v", view.Eligibility)
	}
}

// TestReserveHonorsRegistrationSnapshot 除外清单隔离要落到备件预留结果上：
// 备件存在且数量足够、到期时刻合法时，原除外请求仍返回现有的不合格错误，
// 不创建承诺、不占用备件；未被登记清单除外的请求（含后来替换或补入的
// 代码）仍可正常预留。
func TestReserveHonorsRegistrationSnapshot(t *testing.T) {
	codes := []string{"BROKEN_SEAL"}
	s := isolationStore(t, codes)
	mustSubmit(t, s, "rExcl", "BROKEN_SEAL")
	mustSubmit(t, s, "rReplaced", "NOISE")
	mustSubmit(t, s, "rAdded", "LEAK")

	// 登记后替换原清单代码并补入新代码，再尝试预留。
	codes[0] = "NOISE"
	codes = append(codes, "LEAK")
	p, _ := s.Product("p1")
	p.ExcludedCodes["LEAK"] = struct{}{}
	delete(p.ExcludedCodes, "BROKEN_SEAL")

	// 原除外请求：仍返回 ErrIneligible，错误明确列出故障除外原因。
	_, err := s.Reserve("c-bad", "rExcl", "part1", 2, expiryOK, nowOK)
	if !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve excluded request: got %v, want ErrIneligible", err)
	}
	if !strings.Contains(err.Error(), string(ReasonFaultExcluded)) {
		t.Fatalf("error %q must list fault_code_excluded", err.Error())
	}
	// 不创建承诺。
	if _, err := s.Commitment("c-bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve created commitment: %v", err)
	}
	// 不占用备件。
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 0 || st.Committable != 10 {
		t.Fatalf("failed reserve occupied stock: %+v", st)
	}
	// 失败留痕，且历史中的资格快照仍显示除外。
	h, _ := s.RequestHistory("rExcl")
	if len(h) != 1 || h[0].Success || h[0].Error != HistoryErrorIneligible {
		t.Fatalf("ineligible history: %+v", h)
	}
	if h[0].Eligibility == nil || !h[0].Eligibility.Excluded ||
		len(h[0].Eligibility.Reasons) != 1 ||
		h[0].Eligibility.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("ineligible history snapshot: %+v", h[0].Eligibility)
	}

	// 替换/补入进来、原先未被除外的代码：正常预留，各占用 2 件。
	for _, tc := range []struct{ commit, request string }{
		{"c-replaced", "rReplaced"},
		{"c-added", "rAdded"},
	} {
		c, err := s.Reserve(tc.commit, tc.request, "part1", 2, expiryOK, nowOK)
		if err != nil {
			t.Fatalf("reserve %s: %v", tc.request, err)
		}
		if c.Quantity != 2 || c.Used != 0 {
			t.Fatalf("commitment for %s: %+v", tc.request, c)
		}
	}
	st2, _ := s.PartStatus("part1", nowOK)
	if st2.PhysicalRemaining != 10 || st2.ActiveOccupied != 4 || st2.Committable != 6 {
		t.Fatalf("eligible reserves stock math: %+v", st2)
	}
}

// TestEmptyExclusionListStaysEmpty 产品以空除外清单登记后，向取回的集合
// （或登记入参）加入代码，不能让该产品开始拒绝这个故障。
func TestEmptyExclusionListStaysEmpty(t *testing.T) {
	input := make([]string, 0, 4)
	s := isolationStore(t, input)
	mustSubmit(t, s, "r1", "RETROACTIVELY_ADDED")

	retrieved, _ := s.Product("p1")
	retrieved.ExcludedCodes["RETROACTIVELY_ADDED"] = struct{}{}
	input = append(input, "RETROACTIVELY_ADDED")

	again, _ := s.Product("p1")
	if len(again.ExcludedCodes) != 0 {
		t.Fatalf("empty exclusion list gained codes: %+v", again.ExcludedCodes)
	}
	assertEligibleInWindow(t, s, "r1")

	// 合格请求仍可正常预留并占用备件。
	c, err := s.Reserve("c1", "r1", "part1", 1, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve on empty-list product: %v", err)
	}
	if c.Quantity != 1 {
		t.Fatalf("commitment: %+v", c)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 1 || st.Committable != 9 {
		t.Fatalf("stock after reserve: %+v", st)
	}
}

// TestExclusionMatchingRemainsExactAndCaseSensitive 沿用故障代码现有的精确
// 匹配方式：不做大小写转换或代码修剪，大小写或首尾空白不同的代码是另一
// 个代码，仍应合格。
func TestExclusionMatchingRemainsExactAndCaseSensitive(t *testing.T) {
	s := isolationStore(t, []string{"BROKEN_SEAL"})
	mustSubmit(t, s, "rLower", "broken_seal")
	mustSubmit(t, s, "rSpaced", " BROKEN_SEAL ")

	assertEligibleInWindow(t, s, "rLower")
	assertEligibleInWindow(t, s, "rSpaced")
	// 精确匹配的原代码仍被拒绝，确认隔离测试没有放松匹配本身。
	mustSubmit(t, s, "rExact", "BROKEN_SEAL")
	assertExcludedInWindow(t, s, "rExact")

	// 未做转换/修剪的近似代码可正常预留。
	if _, err := s.Reserve("c-lower", "rLower", "part1", 1, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve lowercase code: %v", err)
	}
	if _, err := s.Reserve("c-exact", "rExact", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve exact excluded code: got %v, want ErrIneligible", err)
	}
}
