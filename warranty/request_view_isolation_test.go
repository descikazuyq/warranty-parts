package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障按请求查看资格与承诺明细（RequestView）的返回结果相互独立：
// 每次查询都返回一份反映当次时刻真实记录的全新结果，资格依据、拒绝原因和
// 承诺明细都定格在该次查询的事实上。调用方为了展示而修改自己手里的合格
// 标记、购买依据、拒绝原因或承诺归属、数量、状态，以及删除、补入明细，
// 只能影响这份结果，不能变成仓库里的真实使用、取消或产品条款变更：另一份
// 已取回的结果、重新查询、直接资格查询和库存账目都继续反映真实记录；先前
// 未经修改的结果也不随之后的真实使用改写，资格始终按各次查询给定的时刻
// 判断。这些操作只沿用登记、预留、使用、查询等既有公开行为。

// 本文件专用的编号与固定时刻，避免与其他测试文件混用：
// 产品保修三十天，故障 NOISE 不在除外清单；备件初始库存十件；承诺在保修期
// 内预留四件、已使用一件，承诺自身到期时刻（第四十天）晚于保修截止时刻
// （第三十天）。
const (
	rvIsoProduct = "p-rviso"
	rvIsoPart    = "part-rviso"
	rvIsoRequest = "r-rviso"
	rvIsoCommit  = "c-rviso"
	rvIsoFault   = "NOISE"
)

var (
	rvIsoWarrantyEnd   = t0.Add(30 * day)
	rvIsoCommitExpiry  = t0.Add(40 * day)
	rvIsoAfterWarranty = rvIsoWarrantyEnd.Add(time.Minute)
)

// rvIsoStore 构造一个保修三十天、故障未被除外的产品，初始库存十件的备件，
// 以及一个合格请求；不预留任何承诺。
func rvIsoStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct(rvIsoProduct, t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(rvIsoPart, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(rvIsoRequest, rvIsoProduct, rvIsoFault); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	return s
}

// rvIsoSetup 在保修期内预留四件并使用一件：库存为实物九件、有效占用三件、
// 可承诺六件。
func rvIsoSetup(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Reserve(rvIsoCommit, rvIsoRequest, rvIsoPart, 4, rvIsoCommitExpiry, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u-rviso-1", rvIsoCommit, 1, nowOK); err != nil {
		t.Fatalf("use one: %v", err)
	}
}

// assertRVEligibilityFact 断言资格依据完整反映登记事实：购买时刻、保修天数、
// 保修截止时刻与未命中除外保持登记内容；合格标记与拒绝原因按期望值给定。
func assertRVEligibilityFact(t *testing.T, e *Eligibility, eligible bool, wantReasons ...RejectionReason) {
	t.Helper()
	if e == nil {
		t.Fatal("eligibility is nil")
	}
	if e.RequestID != rvIsoRequest || e.ProductID != rvIsoProduct || e.FaultCode != rvIsoFault {
		t.Fatalf("eligibility identity = req=%s product=%s fault=%s, want %s/%s/%s",
			e.RequestID, e.ProductID, e.FaultCode, rvIsoRequest, rvIsoProduct, rvIsoFault)
	}
	if e.Eligible != eligible {
		t.Fatalf("eligible = %v, want %v (full %+v)", e.Eligible, eligible, e)
	}
	if !reasonsEqual(e.Reasons, wantReasons) {
		t.Fatalf("reasons = %v, want exactly %v", e.Reasons, wantReasons)
	}
	if !e.PurchaseTime.Equal(t0) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(rvIsoWarrantyEnd) {
		t.Fatalf("warranty basis = purchase %v / %d days / expiry %v, want %v / 30 / %v",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, t0, rvIsoWarrantyEnd)
	}
	if e.Excluded {
		t.Fatalf("fault %q must not be excluded, got Excluded=true", rvIsoFault)
	}
}

// assertRVCommitmentFact 断言一条承诺明细反映该笔承诺的真实事实：归属准确，
// 原定四件，已用与未用按期望值，状态与到期时刻按当次查询给定。
func assertRVCommitmentFact(t *testing.T, d CommitmentDetail, used, remaining int, status CommitmentStatus) {
	t.Helper()
	if d.CommitmentID != rvIsoCommit || d.RequestID != rvIsoRequest || d.PartID != rvIsoPart {
		t.Fatalf("detail identity = commit=%s req=%s part=%s, want %s/%s/%s",
			d.CommitmentID, d.RequestID, d.PartID, rvIsoCommit, rvIsoRequest, rvIsoPart)
	}
	if d.OriginalQuantity != 4 || d.UsedQuantity != used || d.RemainingQuantity != remaining ||
		d.Status != status || !d.Expiry.Equal(rvIsoCommitExpiry) {
		t.Fatalf("detail = orig=%d used=%d remaining=%d status=%s expiry=%v; want orig=4 used=%d remaining=%d status=%s expiry=%v",
			d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status, d.Expiry,
			used, remaining, status, rvIsoCommitExpiry)
	}
}

// assertRVSingleCommitment 断言请求视图里恰好只有本文件那一笔承诺，且它
// 反映给定的真实事实。
func assertRVSingleCommitment(t *testing.T, v *RequestView, used, remaining int, status CommitmentStatus) CommitmentDetail {
	t.Helper()
	if len(v.Commitments) != 1 {
		t.Fatalf("commitments = %d (%+v), want exactly 1", len(v.Commitments), v.Commitments)
	}
	d := v.Commitments[0]
	assertRVCommitmentFact(t, d, used, remaining, status)
	return d
}

// assertRVStockFact 断言备件账目为期望的实物/有效占用/可承诺三件套。
func assertRVStockFact(t *testing.T, s *Store, now time.Time, physical, occupied, committable int) {
	t.Helper()
	st, err := s.PartStatus(rvIsoPart, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock phys=%d occupied=%d committable=%d, want phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// TestRequestViewResultsAreIndependentAcrossQueries 对应主例：保修三十天、
// 故障未被除外的产品在保修期内预留四件并已使用一件，备件初始库存十件，
// 承诺到期晚于保修截止。产品刚过保、承诺尚未到期时取得两份结果：资格不
// 合格且拒绝原因只有过保，购买时刻、保修天数、截止时刻仍是登记依据，承诺
// 明细有效且为四件、已用一件、未用三件，归属与到期时刻准确。调用方随后
// 任意篡改其中一份（合格标记、购买依据、拒绝原因，承诺归属/数量/状态，
// 删除并补入明细），另一份、重新查询、直接资格查询与库存都保持真实内容，
// 仓库记录也没有被当作真实使用、取消或条款变更。
func TestRequestViewResultsAreIndependentAcrossQueries(t *testing.T) {
	s := rvIsoStore(t)
	rvIsoSetup(t, s)

	// 产品刚过保、承诺尚未到期：两份结果在同一时刻取得，内容相同但必须
	// 各自独立。
	first, err := s.RequestView(rvIsoRequest, rvIsoAfterWarranty)
	if err != nil {
		t.Fatalf("first request view: %v", err)
	}
	second, err := s.RequestView(rvIsoRequest, rvIsoAfterWarranty)
	if err != nil {
		t.Fatalf("second request view: %v", err)
	}
	for _, v := range []*RequestView{first, second} {
		assertRVEligibilityFact(t, v.Eligibility, false, ReasonWarrantyExpired)
		assertRVSingleCommitment(t, v, 1, 3, CommitmentActive)
	}

	// 调用方篡改第二份：改合格标记与购买依据、替换已有拒绝原因、改写承诺
	// 的归属/数量/状态与到期时刻，再删除真实明细并补入一条虚构明细。
	e := second.Eligibility
	e.Eligible = true
	e.Excluded = true
	e.Reasons = []RejectionReason{ReasonFaultExcluded}
	e.PurchaseTime = t0.Add(-99 * day)
	e.WarrantyDays = 7
	e.WarrantyExpiry = t0.Add(99 * day)
	e.RequestID = "rewritten-request"
	e.ProductID = "rewritten-product"
	e.FaultCode = "REWRITTEN"
	second.Commitments[0] = CommitmentDetail{
		CommitmentID:      rvIsoCommit,
		RequestID:         "rewritten-request",
		PartID:            "rewritten-part",
		OriginalQuantity:  50,
		UsedQuantity:      49,
		RemainingQuantity: 1,
		Expiry:            t0.Add(99 * day),
		Status:            CommitmentCanceled,
	}
	second.Commitments = second.Commitments[:0]
	second.Commitments = append(second.Commitments, CommitmentDetail{
		CommitmentID:      "c-fabricated",
		RequestID:         "rewritten-request",
		PartID:            "rewritten-part",
		OriginalQuantity:  8,
		UsedQuantity:      0,
		RemainingQuantity: 8,
		Expiry:            t0.Add(99 * day),
		Status:            CommitmentActive,
	})

	// 先前取得的第一份结果保留查询当时的真实内容。
	assertRVEligibilityFact(t, first.Eligibility, false, ReasonWarrantyExpired)
	assertRVSingleCommitment(t, first, 1, 3, CommitmentActive)

	// 重新查询仍是真实内容：过保且只有过保一项原因，承诺有效、四件用一件。
	refreshed, err := s.RequestView(rvIsoRequest, rvIsoAfterWarranty)
	if err != nil {
		t.Fatalf("refreshed request view: %v", err)
	}
	assertRVEligibilityFact(t, refreshed.Eligibility, false, ReasonWarrantyExpired)
	assertRVSingleCommitment(t, refreshed, 1, 3, CommitmentActive)

	// 直接查询资格也仍显示过保，登记依据不变。
	direct, err := s.Evaluate(rvIsoRequest, rvIsoAfterWarranty)
	if err != nil {
		t.Fatalf("direct evaluate: %v", err)
	}
	assertRVEligibilityFact(t, direct, false, ReasonWarrantyExpired)

	// 库存仍为实物九件、有效占用三件、可承诺六件：对返回数据的编辑不是
	// 真实使用或取消。
	assertRVStockFact(t, s, rvIsoAfterWarranty, 9, 3, 6)
	st, _ := s.PartStatus(rvIsoPart, rvIsoAfterWarranty)
	d, ok := detailByID(st, rvIsoCommit)
	if !ok {
		t.Fatalf("real commitment missing from part details: %+v", st.Details)
	}
	if d.RequestID != rvIsoRequest || d.Status != CommitmentActive ||
		d.OriginalQuantity != 4 || d.UsedQuantity != 1 || d.RemainingQuantity != 3 {
		t.Fatalf("part detail disturbed by caller edit: %+v", d)
	}
	if _, ok := detailByID(st, "c-fabricated"); ok {
		t.Fatalf("fabricated detail leaked into stock query")
	}

	// 仓库里的承诺没有被编辑使用、取消或改写归属。
	c, err := s.Commitment(rvIsoCommit)
	if err != nil {
		t.Fatalf("stored commitment: %v", err)
	}
	if c.RequestID != rvIsoRequest || c.PartID != rvIsoPart || c.Quantity != 4 || c.Used != 1 ||
		c.Unused() != 3 || c.Canceled || c.Expired || !c.Expiry.Equal(rvIsoCommitExpiry) {
		t.Fatalf("stored commitment disturbed by caller edit: %+v", c)
	}

	// 产品条款没有被编辑改写：过保时刻的新预留仍按现有规则不合格，不创建
	// 承诺、不增加占用。
	if _, err := s.Reserve("c-rviso-ineligible", rvIsoRequest, rvIsoPart, 1, rvIsoCommitExpiry, rvIsoAfterWarranty); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve after warranty: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("c-rviso-ineligible"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ineligible reserve created a commitment: %v", err)
	}
	assertRVStockFact(t, s, rvIsoAfterWarranty, 9, 3, 6)

	// 真实承诺仍处于有效状态：承诺到期前的新使用可以成功，证明编辑没有把
	// 它取消或用掉。
	if _, err := s.Use("u-rviso-probe", rvIsoCommit, 1, rvIsoAfterWarranty); err != nil {
		t.Fatalf("real commitment closed by a caller-side edit: %v", err)
	}
}

// TestRequestViewSnapshotStaysFixedAcrossLaterUse 保护已经取得的结果不随
// 之后的真实操作变化：保修截止前取得的合格结果与刚过保时取得的结果（已用
// 一件、未用三件）都保留各自查询当时的内容；承诺到期前再成功使用一件后，
// 新查询显示已用两件、未用两件，库存变为实物八件、有效占用两件、可承诺
// 六件。资格始终按各次查询给定的时刻判断，保修截止前的合格结果不被后来
// 的过保查询改写。
func TestRequestViewSnapshotStaysFixedAcrossLaterUse(t *testing.T) {
	s := rvIsoStore(t)
	rvIsoSetup(t, s)

	// 保修截止前的查询：合格、无拒绝原因，承诺已用一件、未用三件。
	inWindow, err := s.RequestView(rvIsoRequest, nowOK)
	if err != nil {
		t.Fatalf("in-window request view: %v", err)
	}
	assertRVEligibilityFact(t, inWindow.Eligibility, true)
	assertRVSingleCommitment(t, inWindow, 1, 3, CommitmentActive)

	// 产品刚过保、承诺尚未到期时的查询：资格只有过保，承诺仍有效。
	saved, err := s.RequestView(rvIsoRequest, rvIsoAfterWarranty)
	if err != nil {
		t.Fatalf("after-warranty request view: %v", err)
	}
	assertRVEligibilityFact(t, saved.Eligibility, false, ReasonWarrantyExpired)
	assertRVSingleCommitment(t, saved, 1, 3, CommitmentActive)

	// 承诺到期前再成功使用一件。
	later := rvIsoWarrantyEnd.Add(2 * day)
	if !later.Before(rvIsoCommitExpiry) {
		t.Fatalf("test setup: %v must be before commitment expiry %v", later, rvIsoCommitExpiry)
	}
	if _, err := s.Use("u-rviso-2", rvIsoCommit, 1, later); err != nil {
		t.Fatalf("second real use: %v", err)
	}

	// 新查询反映真实使用：已用两件、未用两件，承诺仍有效；资格仍只有过保。
	fresh, err := s.RequestView(rvIsoRequest, later)
	if err != nil {
		t.Fatalf("fresh request view after second use: %v", err)
	}
	assertRVEligibilityFact(t, fresh.Eligibility, false, ReasonWarrantyExpired)
	assertRVSingleCommitment(t, fresh, 2, 2, CommitmentActive)

	// 库存变为实物八件、有效占用两件、可承诺六件。
	assertRVStockFact(t, s, later, 8, 2, 6)

	// 先前未经调用方修改的两份结果保留查询当时内容：刚过保那份仍是已用
	// 一件、未用三件，不被真实使用改写。
	assertRVEligibilityFact(t, saved.Eligibility, false, ReasonWarrantyExpired)
	assertRVSingleCommitment(t, saved, 1, 3, CommitmentActive)
	// 保修截止前那份仍合格：资格按各次查询时刻判断，不被后来的过保查询
	// 和真实使用改写，明细同样定格在一件已用。
	assertRVEligibilityFact(t, inWindow.Eligibility, true)
	assertRVSingleCommitment(t, inWindow, 1, 3, CommitmentActive)

	// 重新直接查询：保修期内仍合格，过保时刻仍过保，各自独立判断。
	eNow, err := s.Evaluate(rvIsoRequest, nowOK)
	if err != nil {
		t.Fatalf("evaluate in window: %v", err)
	}
	assertRVEligibilityFact(t, eNow, true)
	eLater, err := s.Evaluate(rvIsoRequest, later)
	if err != nil {
		t.Fatalf("evaluate after warranty: %v", err)
	}
	assertRVEligibilityFact(t, eLater, false, ReasonWarrantyExpired)
}

// TestRequestViewEmptyCommitmentsSnapshotStaysEmpty 同一规则适用于尚无承诺
// 的已知请求：原查询返回实际资格与空明细；之后正常预留成功，承诺只在新
// 查询中出现，不向已经取回的空结果补入记录；调用方向自己手里的空结果补入
// 明细同样不会进入仓库或新查询。
func TestRequestViewEmptyCommitmentsSnapshotStaysEmpty(t *testing.T) {
	s := rvIsoStore(t)
	const emptyReq = "r-rvempty"
	if err := s.SubmitRequest(emptyReq, rvIsoProduct, rvIsoFault); err != nil {
		t.Fatalf("submit empty request: %v", err)
	}

	empty, err := s.RequestView(emptyReq, nowOK)
	if err != nil {
		t.Fatalf("empty request view: %v", err)
	}
	ee := empty.Eligibility
	if ee.RequestID != emptyReq || ee.ProductID != rvIsoProduct || ee.FaultCode != rvIsoFault ||
		!ee.Eligible || !reasonsEqual(ee.Reasons, nil) || ee.Excluded ||
		!ee.PurchaseTime.Equal(t0) || ee.WarrantyDays != 30 || !ee.WarrantyExpiry.Equal(rvIsoWarrantyEnd) {
		t.Fatalf("empty view eligibility = %+v, want eligible with the registered basis", ee)
	}
	if len(empty.Commitments) != 0 {
		t.Fatalf("commitments = %+v, want empty", empty.Commitments)
	}

	// 调用方向自己手里的空结果补入一条虚构明细。
	empty.Commitments = append(empty.Commitments, CommitmentDetail{
		CommitmentID:      "c-fabricated",
		RequestID:         emptyReq,
		PartID:            "rewritten-part",
		OriginalQuantity:  8,
		UsedQuantity:      0,
		RemainingQuantity: 8,
		Expiry:            t0.Add(99 * day),
		Status:            CommitmentActive,
	})

	// 之后正常预留成功。
	if _, err := s.Reserve("c-rvlater", emptyReq, rvIsoPart, 4, rvIsoCommitExpiry, nowOK); err != nil {
		t.Fatalf("later reserve: %v", err)
	}

	// 新查询只出现真实承诺：四件、未使用、有效；虚构明细没有进入仓库。
	fresh, err := s.RequestView(emptyReq, nowOK)
	if err != nil {
		t.Fatalf("fresh request view: %v", err)
	}
	if !fresh.Eligibility.Eligible {
		t.Fatalf("fresh eligibility = %+v, want eligible", fresh.Eligibility)
	}
	if len(fresh.Commitments) != 1 {
		t.Fatalf("fresh commitments = %+v, want exactly the real one", fresh.Commitments)
	}
	d := fresh.Commitments[0]
	if d.CommitmentID != "c-rvlater" || d.RequestID != emptyReq || d.PartID != rvIsoPart ||
		d.OriginalQuantity != 4 || d.UsedQuantity != 0 || d.RemainingQuantity != 4 ||
		d.Status != CommitmentActive || !d.Expiry.Equal(rvIsoCommitExpiry) {
		t.Fatalf("real commitment detail = %+v, want active 4/0/4", d)
	}

	// 已经取回的结果不被真实预留补入：里面仍只有调用方自己补的虚构明细。
	if len(empty.Commitments) != 1 || empty.Commitments[0].CommitmentID != "c-fabricated" {
		t.Fatalf("earlier result gained the real reserve: %+v", empty.Commitments)
	}
	if !empty.Eligibility.Eligible {
		t.Fatalf("earlier eligibility changed by later reserve: %+v", empty.Eligibility)
	}
}
