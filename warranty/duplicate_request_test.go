package warranty

import (
	"errors"
	"testing"
)

// 重复提交请求的保护：首次成功提交后，已存在的非空请求编号再次提交时一律
// 报 ErrDuplicateID，无论新提交的产品编号、故障代码是否合法（包括空故障
// 代码）；原请求的产品编号与故障代码完整保留，资格依据、关联承诺、预留
// 历史与库存账目都继续按第一次成功提交的资料执行。

// checkRequestData 校验取回的请求资料与首次成功提交完全一致。
func checkRequestData(t *testing.T, s *Store, id, productID, faultCode string) {
	t.Helper()
	r, err := s.Request(id)
	if err != nil {
		t.Fatalf("request %q: %v", id, err)
	}
	if r.ID != id || r.ProductID != productID || r.FaultCode != faultCode {
		t.Fatalf("request %q = %+v, want product %q fault %q", id, r, productID, faultCode)
	}
}

// requestDupSetup 登记两个在保产品：p1 保修三十天、除外 FAULTX；p2 保修
// 三百天、除外 FAULTZ；另有备件 part1 初始库存十件。
func requestDupSetup(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register p1: %v", err)
	}
	if err := s.RegisterProduct("p2", t0, 300, []string{"FAULTZ"}); err != nil {
		t.Fatalf("register p2: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part1: %v", err)
	}
	return s
}

// 已成功提交的编号再次提交：换成另一个在保产品和未被除外的故障、把故障
// 代码改成空串、连同产品编号一起置空，全部报 ErrDuplicateID 而不是
// ErrInvalidParam，原请求资料完整保留。
func TestDuplicateRequestInvalidContentStillDuplicate(t *testing.T) {
	s := requestDupSetup(t)
	if err := s.SubmitRequest("r1", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	cases := []struct {
		name             string
		productID, fault string
	}{
		{"other eligible product and fault", "p2", "NOISE"},
		{"empty fault code", "p2", ""},
		{"empty product and fault", "", ""},
		{"same product, empty fault", "p1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.SubmitRequest("r1", tc.productID, tc.fault)
			if !errors.Is(err, ErrDuplicateID) {
				t.Fatalf("duplicate request: got %v, want ErrDuplicateID", err)
			}
			if errors.Is(err, ErrInvalidParam) {
				t.Fatalf("duplicate id must win over param validation, got %v", err)
			}
			checkRequestData(t, s, "r1", "p1", "FAULTX")
		})
	}
}

// 拒绝后资格继续按原产品与原故障判断：原故障命中原产品除外清单时，换成
// 在保且未除外的新产品/故障重新提交被拒绝，该请求仍不合格；原请求本来
// 合格时，被拒绝的新内容（除外故障、未知产品、空故障）也不能把它改成
// 除外或产品不存在。保修时间仍按每次查询给定的时刻、依原产品期限判断。
func TestDuplicateRequestRejectionKeepsOriginalEligibility(t *testing.T) {
	s := requestDupSetup(t)
	// r1：原故障 FAULTX 命中 p1 的除外清单。
	if err := s.SubmitRequest("r1", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit r1: %v", err)
	}
	// r2：原资料 p2/NOISE，在保且未被除外。
	if err := s.SubmitRequest("r2", "p2", "NOISE"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}

	// r1 换成在保产品 p2 与未除外故障 NOISE 被拒绝，空故障再提交同样被拒绝。
	if err := s.SubmitRequest("r1", "p2", "NOISE"); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("r1 duplicate with eligible content: got %v", err)
	}
	if err := s.SubmitRequest("r1", "p2", ""); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("r1 duplicate with empty fault: got %v", err)
	}
	// r2 换成 p1 的除外故障、未知产品、空故障重复提交，全部被拒绝。
	for _, dup := range []struct{ p, f string }{
		{"p1", "FAULTX"},
		{"pMissing", "FAULTX"},
		{"p2", ""},
	} {
		if err := s.SubmitRequest("r2", dup.p, dup.f); !errors.Is(err, ErrDuplicateID) {
			t.Fatalf("r2 duplicate (%q/%q): got %v", dup.p, dup.f, err)
		}
	}

	at := t0.Add(10 * day)
	// r1 仍按 p1/FAULTX 判断：保修期内但命中除外，只有 fault_code_excluded。
	e, err := s.Evaluate("r1", at)
	if err != nil {
		t.Fatalf("evaluate r1: %v", err)
	}
	if e.ProductID != "p1" || e.FaultCode != "FAULTX" || e.WarrantyDays != 30 {
		t.Fatalf("r1 basis = %q/%q days %d, want p1/FAULTX/30", e.ProductID, e.FaultCode, e.WarrantyDays)
	}
	if e.Eligible || !e.Excluded || !containsReason(e.Reasons, ReasonFaultExcluded) {
		t.Fatalf("r1 must stay excluded under original data: %+v", e)
	}
	// 新预留仍按原资料判断为不合格，不生成承诺。
	if _, err := s.Reserve("cBad", "r1", "part1", 1, expiryOK, at); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve for r1: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("cBad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected reserve must not create a commitment, got %v", err)
	}

	// r2 仍按 p2/NOISE 判断：合格，不命中任何除外清单，产品明确存在。
	e, err = s.Evaluate("r2", at)
	if err != nil {
		t.Fatalf("evaluate r2: %v", err)
	}
	if e.ProductID != "p2" || e.FaultCode != "NOISE" || e.WarrantyDays != 300 {
		t.Fatalf("r2 basis = %q/%q days %d, want p2/NOISE/300", e.ProductID, e.FaultCode, e.WarrantyDays)
	}
	if !e.Eligible || e.Excluded || len(e.Reasons) != 0 {
		t.Fatalf("r2 must stay eligible under original data: %+v", e)
	}
	// 保修时间仍按当次时刻、依原产品 p2 的三百天期限判断：第四百天已过保。
	eLate, err := s.Evaluate("r2", t0.Add(400*day))
	if err != nil {
		t.Fatalf("evaluate r2 late: %v", err)
	}
	if eLate.Eligible || !containsReason(eLate.Reasons, ReasonWarrantyExpired) || eLate.Excluded {
		t.Fatalf("r2 at day 400 must be expired under original p2 terms: %+v", eLate)
	}
	// 保修期内新预留仍可成功，使用原请求资料。
	if _, err := s.Reserve("cOK", "r2", "part1", 2, expiryOK, at); err != nil {
		t.Fatalf("reserve for r2 after rejected duplicates: %v", err)
	}
}

// 已有承诺与预留历史属于原请求：重复提交不清空、不重挂、不改写记录，也
// 不新增预留处理历史；部分使用的有效承诺仍按原承诺呈现备件、原定数量、
// 已用数量与余量，实物剩余与可承诺数量不因拒绝而变化。
func TestDuplicateRequestRejectionKeepsCommitmentsAndHistory(t *testing.T) {
	s := requestDupSetup(t)
	if err := s.SubmitRequest("r2", "p2", "NOISE"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// 原请求已有一笔预留四件、成功使用一件的有效承诺。
	if _, err := s.Reserve("c1", "r2", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 1, nowOK); err != nil {
		t.Fatalf("use u1: %v", err)
	}
	histBefore, err := s.RequestHistory("r2")
	if err != nil {
		t.Fatalf("history before: %v", err)
	}
	if len(histBefore) != 1 || !histBefore[0].Success {
		t.Fatalf("history before duplicate = %+v, want one success record", histBefore)
	}

	// 用各种被拒绝的新内容重复提交，包括空故障代码。
	for _, dup := range []struct{ p, f string }{
		{"p1", "FAULTX"},
		{"p2", ""},
		{"pMissing", ""},
	} {
		if err := s.SubmitRequest("r2", dup.p, dup.f); !errors.Is(err, ErrDuplicateID) {
			t.Fatalf("duplicate (%q/%q): got %v", dup.p, dup.f, err)
		}
	}

	checkRequestData(t, s, "r2", "p2", "NOISE")

	// 按请求查看：资格仍来自 p2/NOISE，关联承诺仍是 c1 的 4 件原定、1 件已用。
	view, err := s.RequestView("r2", t0.Add(11*day))
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if view.Eligibility.ProductID != "p2" || view.Eligibility.FaultCode != "NOISE" ||
		!view.Eligibility.Eligible {
		t.Fatalf("view eligibility = %+v, want eligible p2/NOISE", view.Eligibility)
	}
	if len(view.Commitments) != 1 {
		t.Fatalf("commitments = %+v, want exactly c1", view.Commitments)
	}
	want := CommitmentDetail{
		CommitmentID:      "c1",
		RequestID:         "r2",
		PartID:            "part1",
		OriginalQuantity:  4,
		UsedQuantity:      1,
		RemainingQuantity: 3,
		Expiry:            expiryOK,
		Status:            CommitmentActive,
	}
	if view.Commitments[0] != want {
		t.Fatalf("c1 detail = %+v, want %+v", view.Commitments[0], want)
	}

	// 库存账目：使用扣减一次（实物九件），有效占用仍是三件，可承诺六件。
	st, err := s.PartStatus("part1", t0.Add(11*day))
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 9 || st.ActiveOccupied != 3 || st.Committable != 6 {
		t.Fatalf("stock = phys %d occupied %d committable %d, want 9/3/6",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 预留历史不被改写，重复提交不新增处理记录。
	histAfter, err := s.RequestHistory("r2")
	if err != nil {
		t.Fatalf("history after: %v", err)
	}
	if len(histAfter) != 1 {
		t.Fatalf("history after duplicate = %d records, want still 1: %+v", len(histAfter), histAfter)
	}
	if !histAfter[0].Success || histAfter[0].CommitID != "c1" {
		t.Fatalf("history record altered: %+v", histAfter[0])
	}
	// 被拒绝的新内容曾指向 p1，但承诺归属仍是原请求 r2，不被重挂。
	c1, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("commitment c1: %v", err)
	}
	if c1.RequestID != "r2" || c1.PartID != "part1" || c1.Quantity != 4 ||
		c1.Used != 1 || c1.Unused() != 3 || c1.Canceled || c1.Expired {
		t.Fatalf("stored commitment altered: %+v", c1)
	}

	// 重复提交后发起新的备件预留，仍按原请求资料成功并计入原请求历史。
	if _, err := s.Reserve("c2", "r2", "part1", 2, expiryOK, t0.Add(12*day)); err != nil {
		t.Fatalf("reserve c2 after duplicates: %v", err)
	}
	histEnd, err := s.RequestHistory("r2")
	if err != nil {
		t.Fatalf("history end: %v", err)
	}
	if len(histEnd) != 2 || !histEnd[1].Success || histEnd[1].CommitID != "c2" {
		t.Fatalf("new reserve history = %+v, want appended c2 success", histEnd)
	}
}

// 首次提交的边界保持不变：空请求编号或空故障代码返回 ErrInvalidParam，
// 失败后不留下请求；随后用同一非空编号和合法故障代码仍可成功提交，成功
// 之后再提交才报编号重复。产品尚未登记时不做产品存在校验，请求可先提交。
func TestFirstRequestFailureThenValidSubmission(t *testing.T) {
	s := requestDupSetup(t)

	if err := s.SubmitRequest("", "p1", "F"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v, want ErrInvalidParam", err)
	}
	if err := s.SubmitRequest("rNew", "p1", ""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty fault code: got %v, want ErrInvalidParam", err)
	} else if errors.Is(err, ErrDuplicateID) {
		t.Fatalf("failed first submission must not report duplicate")
	}
	// 失败后不留下请求。
	if _, err := s.Request("rNew"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed submission left a request: %v", err)
	}
	// 同一非空编号随后以合法故障代码成功提交。
	if err := s.SubmitRequest("rNew", "p1", "FAULTY"); err != nil {
		t.Fatalf("valid submission after failures: %v", err)
	}
	checkRequestData(t, s, "rNew", "p1", "FAULTY")
	// 成功之后再提交，即使新故障为空也报编号重复。
	if err := s.SubmitRequest("rNew", "p1", ""); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate after success: got %v, want ErrDuplicateID", err)
	}
	checkRequestData(t, s, "rNew", "p1", "FAULTY")

	// 产品尚未登记时请求仍可先提交；资格查询与预留再按现有规则报产品不存在。
	if err := s.SubmitRequest("rNoProduct", "pMissing", "F"); err != nil {
		t.Fatalf("submit before product registered: %v", err)
	}
	checkRequestData(t, s, "rNoProduct", "pMissing", "F")
	if _, err := s.Evaluate("rNoProduct", nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evaluate with missing product: got %v, want ErrNotFound", err)
	}
	if _, err := s.Reserve("cNoProduct", "rNoProduct", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve with missing product: got %v, want ErrNotFound", err)
	}
}
