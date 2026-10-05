package warranty

import (
	"testing"
	"time"
)

// 本文件回归保障按请求查询（RequestView）与直接资格查询（Evaluate）返回结果
// 的数据归属：每次查询得到的资格依据、拒绝原因和承诺明细都只反映该次查询时刻
// 的事实，并且是属于调用方的副本。调用方编辑自己手里的一份结果，只能改动这
// 一份数据，不能改变另一份已取回的结果、仓库记录、库存账目或后续查询；仓库之
// 后的真实使用、预留只体现在新查询中，不回写此前已经取回的快照（资格始终按各
// 次查询给定的时刻判断）。这些测试只沿用登记、预留、使用与查询等既有公开行为，
// 不改变任何资格规则、承诺状态和错误行为。

// rvSnapSetup 构造用户给出的主例：保修三十天、故障 NOISE 未被除外（除外清单
// 含另一代码 FAULTX）的产品；备件初始库存十件；保修期内预留四件、到期时刻
// （保修后第十五天）晚于保修截止时刻，并已成功使用一件。
func rvSnapSetup(t *testing.T) (s *Store, deadline, commitExpiry, justPast time.Time) {
	t.Helper()
	purchase := t0
	deadline = purchase.Add(30 * day)
	commitExpiry = purchase.Add(45 * day)
	// 产品刚过保、承诺尚未到期的查看时刻。
	justPast = deadline.Add(time.Minute)

	s = NewStore()
	if err := s.RegisterProduct("p1", purchase, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 4, commitExpiry, purchase.Add(10*day)); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 1, purchase.Add(12*day)); err != nil {
		t.Fatalf("use u1: %v", err)
	}
	return s, deadline, commitExpiry, justPast
}

// assertExpiredOnlyBasis 断言资格依据为登记内容、在过保时刻只有过保一项拒绝
// 原因：不合格、未命中除外，购买时刻、保修天数与保修截止时刻仍是登记依据。
func assertExpiredOnlyBasis(t *testing.T, e *Eligibility, purchase, deadline time.Time) {
	t.Helper()
	if e == nil {
		t.Fatal("eligibility is nil")
	}
	if e.RequestID != "r1" || e.ProductID != "p1" || e.FaultCode != "NOISE" {
		t.Fatalf("basis identity = %q/%q/%q, want r1/p1/NOISE", e.RequestID, e.ProductID, e.FaultCode)
	}
	if e.Eligible {
		t.Fatalf("eligible = true, want false: %+v", e)
	}
	if e.Excluded {
		t.Fatalf("excluded = true, want false: %+v", e)
	}
	if len(e.Reasons) != 1 || e.Reasons[0] != ReasonWarrantyExpired {
		t.Fatalf("reasons = %v, want only [warranty_expired]", e.Reasons)
	}
	if !e.PurchaseTime.Equal(purchase) {
		t.Fatalf("purchase time = %v, want registered %v", e.PurchaseTime, purchase)
	}
	if e.WarrantyDays != 30 {
		t.Fatalf("warranty days = %d, want registered 30", e.WarrantyDays)
	}
	if !e.WarrantyExpiry.Equal(deadline) {
		t.Fatalf("warranty expiry = %v, want %v", e.WarrantyExpiry, deadline)
	}
}

// assertC1Snapshot 断言承诺明细为 c1 的真实内容：归属 r1/part1、原数量四件，
// 已用/未用与状态按各次查询时刻给定。
func assertC1Snapshot(t *testing.T, d CommitmentDetail, used, remaining int, status CommitmentStatus, commitExpiry time.Time) {
	t.Helper()
	want := CommitmentDetail{
		CommitmentID:      "c1",
		RequestID:         "r1",
		PartID:            "part1",
		OriginalQuantity:  4,
		UsedQuantity:      used,
		RemainingQuantity: remaining,
		Expiry:            commitExpiry,
		Status:            status,
	}
	if d != want {
		t.Fatalf("c1 detail = %+v, want %+v", d, want)
	}
}

// assertJustPastView 断言“刚过保、承诺未到期”时刻的完整请求视图：资格只有
// 过保，承诺明细仍有效且为四件原数量、一件已用、三件未用。
func assertJustPastView(t *testing.T, v *RequestView, deadline, commitExpiry time.Time) {
	t.Helper()
	if v == nil {
		t.Fatal("request view is nil")
	}
	assertExpiredOnlyBasis(t, v.Eligibility, t0, deadline)
	if len(v.Commitments) != 1 {
		t.Fatalf("commitments = %d (%+v), want exactly 1", len(v.Commitments), v.Commitments)
	}
	assertC1Snapshot(t, v.Commitments[0], 1, 3, CommitmentActive, commitExpiry)
}

// TestRequestViewFactsAtJustPastWarranty 主例事实核对：产品刚过保、承诺尚未
// 到期时查看，资格不合格且拒绝原因只有过保，购买时刻、保修天数和保修截止时刻
// 仍是登记依据；承诺仍有效，原数量四、已用一、未用三，归属与到期时刻准确。
func TestRequestViewFactsAtJustPastWarranty(t *testing.T) {
	s, deadline, commitExpiry, justPast := rvSnapSetup(t)

	v, err := s.RequestView("r1", justPast)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	assertJustPastView(t, v, deadline, commitExpiry)

	// 同一时刻直接查询资格，口径一致：仍只显示过保。
	e, err := s.Evaluate("r1", justPast)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	assertExpiredOnlyBasis(t, e, t0, deadline)

	// 库存只反映真实使用：实物九件、有效占用三件、可承诺六件。
	st, err := s.PartStatus("part1", justPast)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 9 || st.ActiveOccupied != 3 || st.Committable != 6 {
		t.Fatalf("stock = phys %d / occupied %d / committable %d, want 9/3/6",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
}

// TestRequestViewMutationStaysInCallersCopy 同一时刻取得两份结果后，调用方
// 任意改动其中一份的购买依据、合格标记、拒绝原因与承诺明细（就地改写、删除、
// 补入），另一份结果、重新查询结果、直接资格查询与库存账目都保持真实内容，
// 不能把对返回数据的编辑当作真实使用、取消或产品条款变更。
func TestRequestViewMutationStaysInCallersCopy(t *testing.T) {
	s, deadline, commitExpiry, justPast := rvSnapSetup(t)

	first, err := s.RequestView("r1", justPast)
	if err != nil {
		t.Fatalf("first view: %v", err)
	}
	second, err := s.RequestView("r1", justPast)
	if err != nil {
		t.Fatalf("second view: %v", err)
	}

	// 改动第一份的资格依据：合格标记、购买时刻、保修天数、截止时刻、除外标记，
	// 并就地替换已有拒绝原因（直接写底层数组）再整体换成另一组原因。
	first.Eligibility.Eligible = true
	first.Eligibility.PurchaseTime = t0.Add(99 * day)
	first.Eligibility.WarrantyDays = 365
	first.Eligibility.WarrantyExpiry = t0.Add(365 * day)
	first.Eligibility.Excluded = true
	first.Eligibility.Reasons[0] = ReasonFaultExcluded
	first.Eligibility.Reasons = []RejectionReason{
		ReasonFaultExcluded, ReasonPurchaseInFuture,
	}

	// 改写第一份的承诺归属、数量与状态：就地写底层数组元素，再补入伪造明细，
	// 最后删除全部明细。
	first.Commitments[0] = CommitmentDetail{
		CommitmentID:      "FAKE",
		RequestID:         "r-hacker",
		PartID:            "part-hacker",
		OriginalQuantity:  40,
		UsedQuantity:      30,
		RemainingQuantity: 10,
		Expiry:            t0,
		Status:            CommitmentCanceled,
	}
	first.Commitments = append(first.Commitments, CommitmentDetail{CommitmentID: "FAKE-2"})
	first.Commitments = first.Commitments[:0]

	// 同时刻取回的另一份结果保持真实内容。
	assertJustPastView(t, second, deadline, commitExpiry)

	// 重新查询仍是事实：过保、承诺有效 4/1/3。
	again, err := s.RequestView("r1", justPast)
	if err != nil {
		t.Fatalf("re-query view: %v", err)
	}
	assertJustPastView(t, again, deadline, commitExpiry)

	// 直接查询资格仍只显示过保，登记依据不变。
	e, err := s.Evaluate("r1", justPast)
	if err != nil {
		t.Fatalf("evaluate after mutation: %v", err)
	}
	assertExpiredOnlyBasis(t, e, t0, deadline)

	// 库存账目不把编辑当真实使用或取消：9/3/6。
	st, err := s.PartStatus("part1", justPast)
	if err != nil {
		t.Fatalf("part status after mutation: %v", err)
	}
	if st.PhysicalRemaining != 9 || st.ActiveOccupied != 3 || st.Committable != 6 {
		t.Fatalf("stock after mutation = %d/%d/%d, want 9/3/6",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	d, ok := detailByID(st, "c1")
	if !ok {
		t.Fatalf("part details lost c1: %+v", st.Details)
	}
	assertC1Snapshot(t, d, 1, 3, CommitmentActive, commitExpiry)

	// 仓库记录本身不被改写。
	c, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("commitment c1: %v", err)
	}
	if c.RequestID != "r1" || c.PartID != "part1" || c.Quantity != 4 ||
		c.Used != 1 || c.Canceled || c.Expired || !c.Expiry.Equal(commitExpiry) {
		t.Fatalf("stored commitment altered: %+v", c)
	}
	p, err := s.Product("p1")
	if err != nil {
		t.Fatalf("product p1: %v", err)
	}
	if !p.PurchaseTime.Equal(t0) || p.WarrantyDays != 30 {
		t.Fatalf("stored product terms altered: %+v", p)
	}
	if _, excluded := p.ExcludedCodes["NOISE"]; excluded {
		t.Fatalf("NOISE became excluded through view mutation")
	}
}

// TestRequestViewSnapshotsDoNotTrackLaterRealOperations 已取回的结果不随之后
// 的真实使用变化：承诺到期前再成功使用一件后，新查询显示已用两件、未用两件，
// 库存变为实物八件、有效占用两件、可承诺六件；此前取回（未经调用方修改）的
// 结果仍保留已用一件、未用三件。保修截止前取得的合格结果也不会被后来的过保
// 查询改写。
func TestRequestViewSnapshotsDoNotTrackLaterRealOperations(t *testing.T) {
	s, deadline, commitExpiry, justPast := rvSnapSetup(t)

	// 保修截止前取得的合格结果：无拒绝原因，依据相同，承诺同样定格在 4/1/3。
	inWindow := t0.Add(20 * day)
	eligibleView, err := s.RequestView("r1", inWindow)
	if err != nil {
		t.Fatalf("view in warranty: %v", err)
	}
	if !eligibleView.Eligibility.Eligible || eligibleView.Eligibility.Excluded ||
		len(eligibleView.Eligibility.Reasons) != 0 {
		t.Fatalf("in-window eligibility = %+v, want eligible with no reasons",
			eligibleView.Eligibility)
	}
	if !eligibleView.Eligibility.PurchaseTime.Equal(t0) ||
		eligibleView.Eligibility.WarrantyDays != 30 ||
		!eligibleView.Eligibility.WarrantyExpiry.Equal(deadline) {
		t.Fatalf("in-window basis altered: %+v", eligibleView.Eligibility)
	}
	if len(eligibleView.Commitments) != 1 {
		t.Fatalf("in-window commitments = %+v, want one c1", eligibleView.Commitments)
	}
	assertC1Snapshot(t, eligibleView.Commitments[0], 1, 3, CommitmentActive, commitExpiry)

	// 刚过保、承诺未到期时取得的结果：资格只有过保，承诺仍 4/1/3。
	staleView, err := s.RequestView("r1", justPast)
	if err != nil {
		t.Fatalf("view just past warranty: %v", err)
	}
	assertJustPastView(t, staleView, deadline, commitExpiry)

	// 承诺到期前再真实使用一件。
	secondUseAt := t0.Add(40 * day)
	if _, err := s.Use("u2", "c1", 1, secondUseAt); err != nil {
		t.Fatalf("second real use: %v", err)
	}

	// 新查询反映真实变化：已用两件、未用两件，承诺在到期时刻前仍有效。
	fresh, err := s.RequestView("r1", secondUseAt)
	if err != nil {
		t.Fatalf("view after second use: %v", err)
	}
	assertExpiredOnlyBasis(t, fresh.Eligibility, t0, deadline)
	if len(fresh.Commitments) != 1 {
		t.Fatalf("fresh commitments = %+v, want one c1", fresh.Commitments)
	}
	assertC1Snapshot(t, fresh.Commitments[0], 2, 2, CommitmentActive, commitExpiry)

	// 库存：实物八件、有效占用两件、可承诺六件。
	st, err := s.PartStatus("part1", secondUseAt)
	if err != nil {
		t.Fatalf("part status after second use: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 2 || st.Committable != 6 {
		t.Fatalf("stock after second use = %d/%d/%d, want 8/2/6",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 此前未经修改的两份结果不被真实使用回写。
	if len(staleView.Commitments) != 1 {
		t.Fatalf("stale view commitments changed: %+v", staleView.Commitments)
	}
	assertC1Snapshot(t, staleView.Commitments[0], 1, 3, CommitmentActive, commitExpiry)
	assertExpiredOnlyBasis(t, staleView.Eligibility, t0, deadline)
	if len(eligibleView.Commitments) != 1 {
		t.Fatalf("eligible view commitments changed: %+v", eligibleView.Commitments)
	}
	assertC1Snapshot(t, eligibleView.Commitments[0], 1, 3, CommitmentActive, commitExpiry)
	if !eligibleView.Eligibility.Eligible || len(eligibleView.Eligibility.Reasons) != 0 {
		t.Fatalf("earlier eligible result rewritten by later expired query: %+v",
			eligibleView.Eligibility)
	}
}

// TestRequestViewEmptyCommitmentsStaysEmptyUntilLaterReserve 尚无承诺的已知
// 请求：原查询返回实际资格与空明细；之后正常预留成功，只在新查询中出现承诺，
// 不向已经取回的空结果补入记录。
func TestRequestViewEmptyCommitmentsStaysEmptyUntilLaterReserve(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("rEmpty", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request: %v", err)
	}

	inWindow := t0.Add(5 * day)
	before, err := s.RequestView("rEmpty", inWindow)
	if err != nil {
		t.Fatalf("view before reserve: %v", err)
	}
	if !before.Eligibility.Eligible || before.Eligibility.Excluded ||
		len(before.Eligibility.Reasons) != 0 {
		t.Fatalf("eligibility before reserve = %+v, want eligible with no reasons",
			before.Eligibility)
	}
	if len(before.Commitments) != 0 {
		t.Fatalf("commitments before reserve = %+v, want empty", before.Commitments)
	}

	// 之后正常预留成功。
	commitExpiry := t0.Add(50 * day)
	if _, err := s.Reserve("c-later", "rEmpty", "part1", 2, commitExpiry, t0.Add(6*day)); err != nil {
		t.Fatalf("later reserve: %v", err)
	}

	// 新查询出现承诺：合格、明细一条，原数量两件、未使用、有效。
	after, err := s.RequestView("rEmpty", t0.Add(7*day))
	if err != nil {
		t.Fatalf("view after reserve: %v", err)
	}
	if !after.Eligibility.Eligible || len(after.Eligibility.Reasons) != 0 {
		t.Fatalf("eligibility after reserve = %+v, want eligible with no reasons",
			after.Eligibility)
	}
	if len(after.Commitments) != 1 {
		t.Fatalf("commitments after reserve = %+v, want exactly one", after.Commitments)
	}
	got := after.Commitments[0]
	want := CommitmentDetail{
		CommitmentID:      "c-later",
		RequestID:         "rEmpty",
		PartID:            "part1",
		OriginalQuantity:  2,
		UsedQuantity:      0,
		RemainingQuantity: 2,
		Expiry:            commitExpiry,
		Status:            CommitmentActive,
	}
	if got != want {
		t.Fatalf("later commitment detail = %+v, want %+v", got, want)
	}

	// 先前取回的空结果不被补入记录，资格也保持当时的合格事实。
	if len(before.Commitments) != 0 {
		t.Fatalf("earlier empty view gained commitments: %+v", before.Commitments)
	}
	if !before.Eligibility.Eligible || len(before.Eligibility.Reasons) != 0 {
		t.Fatalf("earlier eligibility rewritten: %+v", before.Eligibility)
	}
}
