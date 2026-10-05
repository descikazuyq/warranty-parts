package warranty

import (
	"errors"
	"testing"
)

// rejectionIsolationStore 构造场景仓库：产品保修三十天、BROKEN_SEAL 在除外
// 故障代码中，备件初始库存十件；rOK 未命中除外代码，rBad 命中 BROKEN_SEAL。
func rejectionIsolationStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("rOK", "p1", "CRACKED"); err != nil {
		t.Fatalf("submit rOK: %v", err)
	}
	if err := s.SubmitRequest("rBad", "p1", "BROKEN_SEAL"); err != nil {
		t.Fatalf("submit rBad: %v", err)
	}
	return s
}

// checkRejectedRecord 校验 rBad 那笔过保且命中除外的失败记录仍保留原事实。
func checkRejectedRecord(t *testing.T, rec HistoryRecord) {
	t.Helper()
	// 请求归属、次序号与提交内容。
	if rec.Seq != 1 {
		t.Fatalf("seq = %d, want 1", rec.Seq)
	}
	if rec.CommitID != "cBad" || rec.PartID != "part1" || rec.Quantity != 2 {
		t.Fatalf("submission fields changed: %+v", rec)
	}
	if !rec.Expiry.Equal(expiryOK) || !rec.Now.Equal(nowLate) {
		t.Fatalf("times changed: expiry=%v now=%v", rec.Expiry, rec.Now)
	}
	// 失败不能变成成功，错误类别保持 ineligible。
	if rec.Success || rec.Error != HistoryErrorIneligible {
		t.Fatalf("failure turned into success: success=%v error=%q", rec.Success, rec.Error)
	}
	// 资格依据：登记的购买时刻与保修期限，过保与故障除外两项原因都在。
	if rec.Eligibility == nil {
		t.Fatal("eligibility snapshot lost")
	}
	e := rec.Eligibility
	if e.Eligible || !e.Excluded || e.RequestID != "rBad" || e.ProductID != "p1" ||
		e.FaultCode != "BROKEN_SEAL" {
		t.Fatalf("eligibility snapshot changed: %+v", e)
	}
	if e.PurchaseTime != t0 || e.WarrantyDays != 30 || e.WarrantyExpiry != t0.Add(30*day) {
		t.Fatalf("eligibility basis changed: %+v", e)
	}
	if len(e.Reasons) != 2 {
		t.Fatalf("reasons = %v, want exactly two", e.Reasons)
	}
	got := map[RejectionReason]bool{}
	for _, r := range e.Reasons {
		got[r] = true
	}
	if !got[ReasonWarrantyExpired] || !got[ReasonFaultExcluded] {
		t.Fatalf("reasons = %v, want warranty_expired and fault_code_excluded", e.Reasons)
	}
	// 库存依据：实物十件、有效占用四件、可承诺六件。
	if rec.StockBasis == nil {
		t.Fatal("stock basis snapshot lost")
	}
	b := rec.StockBasis
	if b.PhysicalRemaining != 10 || b.ActiveOccupied != 4 || b.Committable != 6 {
		t.Fatalf("stock basis changed: %+v", b)
	}
}

// nowLate 是产品过保后的提交时刻（第 31 天）。
var nowLate = t0.Add(31 * day)

// setupRejectedReserve 执行场景中的两笔预留：rOK 在保修期内成功预留四件
// （承诺到期晚于随后查询历史的时刻），rBad 在过保后预留两件被拒绝。
// 返回失败发生后的第一次历史查询结果。
func setupRejectedReserve(t *testing.T, s *Store) []HistoryRecord {
	t.Helper()
	if _, err := s.Reserve("cOK", "rOK", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve rOK: %v", err)
	}
	if _, err := s.Reserve("cBad", "rBad", "part1", 2, expiryOK, nowLate); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve rBad: got %v, want ErrIneligible", err)
	}
	h, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1", len(h))
	}
	return h
}

func TestRejectedHistoryRecordFacts(t *testing.T) {
	s := rejectionIsolationStore(t)
	h := setupRejectedReserve(t, s)
	checkRejectedRecord(t, h[0])

	// 被拒绝的两件不能进入占用，也不能出现成功承诺。
	if _, err := s.Commitment("cBad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected commit should not exist: got %v", err)
	}
	st, err := s.PartStatus("part1", nowLate)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 4 || st.Committable != 6 {
		t.Fatalf("part status = %+v, want 10/4/6", st)
	}
	if len(st.Details) != 1 || st.Details[0].CommitmentID != "cOK" {
		t.Fatalf("only the successful commitment may appear: %+v", st.Details)
	}
}

func TestRejectedHistoryImmutableAgainstCallerEdits(t *testing.T) {
	s := rejectionIsolationStore(t)
	// 针对同一笔失败分别取回两份历史。
	first := setupRejectedReserve(t, s)
	second, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("second history: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second history len = %d, want 1", len(second))
	}

	// 调用方改动第一份：已有的拒绝原因改成其他原因，并改动合格标记、
	// 处理结果、提交数量和库存依据。
	rec := &first[0]
	rec.Eligibility.Reasons[0] = ReasonPurchaseInFuture
	rec.Eligibility.Reasons = rec.Eligibility.Reasons[:1]
	rec.Eligibility.Eligible = true
	rec.Eligibility.Excluded = false
	rec.Eligibility.PurchaseTime = nowLate
	rec.Eligibility.WarrantyDays = 3650
	rec.Success = true
	rec.Error = ""
	rec.Quantity = 99
	rec.CommitID = "cForged"
	rec.Seq = 7
	rec.StockBasis.PhysicalRemaining = 0
	rec.StockBasis.ActiveOccupied = 0
	rec.StockBasis.Committable = 999

	// 此前取回的第二份结果保留原事实。
	checkRejectedRecord(t, second[0])
	// 重新查询的历史也保留原事实。
	fresh, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("fresh history: %v", err)
	}
	if len(fresh) != 1 {
		t.Fatalf("fresh history len = %d, want 1", len(fresh))
	}
	checkRejectedRecord(t, fresh[0])

	// 调用方删除或补入自己那份列表中的记录，不改变仓库保存的提交次数。
	fresh = fresh[:0]
	fresh = append(fresh, HistoryRecord{Seq: 99, CommitID: "cGhost", Success: true})
	again, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("history after list edits: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("stored submission count changed: len = %d, want 1", len(again))
	}
	checkRejectedRecord(t, again[0])

	// 直接查询该请求的资格仍得到原来的拒绝原因。
	elig, err := s.Evaluate("rBad", nowLate)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if elig.Eligible || len(elig.Reasons) != 2 {
		t.Fatalf("eligibility changed: %+v", elig)
	}
	got := map[RejectionReason]bool{}
	for _, r := range elig.Reasons {
		got[r] = true
	}
	if !got[ReasonWarrantyExpired] || !got[ReasonFaultExcluded] {
		t.Fatalf("evaluate reasons = %v, want warranty_expired and fault_code_excluded", elig.Reasons)
	}

	// 备件查询仍只显示原有四件占用，不能因为编辑返回值就获得备件。
	st, err := s.PartStatus("part1", nowLate)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 4 || st.Committable != 6 {
		t.Fatalf("part status = %+v, want 10/4/6", st)
	}

	// 另一请求的历史不受对 rBad 返回值的编辑影响。
	hOK, err := s.RequestHistory("rOK")
	if err != nil {
		t.Fatalf("rOK history: %v", err)
	}
	if len(hOK) != 1 || !hOK[0].Success || hOK[0].CommitID != "cOK" || hOK[0].Quantity != 4 {
		t.Fatalf("rOK history changed: %+v", hOK)
	}
}

func TestEmptyHistoryCallerAppendDoesNotPersist(t *testing.T) {
	s := rejectionIsolationStore(t)

	// 已登记但没有预留记录的请求：取回的历史是空列表。
	h, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if h == nil || len(h) != 0 {
		t.Fatalf("expected empty non-nil history, got %v", h)
	}

	// 调用方在自己的列表中加入记录，再次查询仍为空。
	h = append(h, HistoryRecord{Seq: 1, CommitID: "cGhost", PartID: "part1", Quantity: 5, Success: true})
	again, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("history after append: %v", err)
	}
	if again == nil || len(again) != 0 {
		t.Fatalf("caller-appended record leaked into store: %v", again)
	}

	// 其他请求的历史不受影响。
	if _, err := s.Reserve("cOK", "rOK", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve rOK: %v", err)
	}
	hOK, err := s.RequestHistory("rOK")
	if err != nil {
		t.Fatalf("rOK history: %v", err)
	}
	if len(hOK) != 1 || !hOK[0].Success || hOK[0].CommitID != "cOK" {
		t.Fatalf("rOK history changed: %+v", hOK)
	}
	empty, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("rBad history: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("rBad history should stay empty, got %+v", empty)
	}
}
