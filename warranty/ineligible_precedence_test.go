package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障同一笔“首次预留”同时撞上保修资格不合格与库存不足时，拒绝
// 结果必须归属资格：返回 ErrIneligible 而不是 ErrInsufficientStock，过保与
// 除外同时成立时两项拒绝原因都保留。库存紧张不能把过保或命中除外清单的请求
// 记录成缺货：被拒编号不产生承诺、不占用数量，原承诺的已用/未用数量与备件
// 三项账目（实物剩余/有效占用/可承诺）保持处理前的值；该请求的处理历史新增
// 一条 ineligible 失败记录，保存本次提交的数量与时刻，资格依据与按同一时刻
// 直接查询（Evaluate、RequestView）的结论一致，库存依据是处理前的真实账目，
// 既不是空依据或零库存，也不混入本次申请的数量。
//
// 主例：备件初始十件；先为合格请求 r-epri-ok 预留八件（c-epri-first），再
// 成功使用两件（u-epri-first）。原承诺未到期也未取消，于是实物剩余八件、
// 有效占用六件、可承诺两件。随后各用例以“从未预留成功”的新编号，为另一张
// 已知请求申请三件（数量为正，到期时刻晚于本次当前时刻）。
//
// 边界覆盖：当前时刻恰好达到保修截止时刻、可承诺数量恰好为零，都不能让库存
// 不足覆盖资格拒绝；作为对照，产品在保且故障未被除外的请求在同样库存不足时
// 仍按现有规则返回 ErrInsufficientStock，历史保存合格资格与真实库存依据。
// 这些检查全部围绕尚未成功预留的编号，另含已成功编号原样重试结果不变的
// 核对，不增加任何新的预留规则。

// 本文件专用编号，避免与其他测试文件混用。
const (
	epProductID = "p-epri"
	epPartID    = "part-epri"

	// epQualifiedReq 是先占用库存的在保合格请求（故障 NOISE 不在除外清单）。
	epQualifiedReq = "r-epri-ok"
	// epFirstCommit 是八件的首次成功承诺；epTailCommit 只在可承诺为零的
	// 场景追加两件占用。
	epFirstCommit = "c-epri-first"
	epTailCommit  = "c-epri-tail"
	epFirstUse    = "u-epri-first"

	// 四张各自只提交一次的不合格请求，分别对应仅过保、仅除外、两者同时成立，
	// 以及当前时刻恰好达到保修截止时刻。
	epExpiredReq  = "r-epri-exp"
	epExcludedReq = "r-epri-exc"
	epBothReq     = "r-epri-both"
	epBoundaryReq = "r-epri-edge"

	epExpiredCommit  = "c-epri-exp"
	epExcludedCommit = "c-epri-exc"
	epBothCommit     = "c-epri-both"
	epBoundaryCommit = "c-epri-edge"

	// 合格对照请求：在保且故障未被除外，库存不足时仍按缺货拒绝。
	epGoodReq    = "r-epri-good"
	epGoodCommit = "c-epri-good"

	// 可承诺为零场景下的不合格请求与合格对照请求。
	epZeroReq        = "r-epri-zero"
	epZeroCommit     = "c-epri-zero"
	epZeroEdgeReq    = "r-epri-zeroedge"
	epZeroEdgeCommit = "c-epri-zeroedge"
	epZeroGoodReq    = "r-epri-zerogood"
	epZeroGoodCommit = "c-epri-zerogood"
)

// 本文件的全部时刻：保修三十天，截止 t0+30d；八件承诺在 t0+10d 预留并使用，
// 承诺到期 t0+90d（晚于本文件所有“当前时刻”，因此拒绝期间它始终有效占用）；
// 被拒申请给定的到期时刻 t0+60d，晚于所有拒绝用例的当前时刻。
var (
	epDeadline     = t0.Add(30 * day)
	epReserveAt    = t0.Add(10 * day)
	epWithinAt     = t0.Add(20 * day) // 保修期内（仅除外用例与合格对照用）
	epExpiredAt    = t0.Add(31 * day) // 已过保
	epCommitExpiry = t0.Add(90 * day)
	epReqExpiry    = t0.Add(60 * day)
	epRetryAt      = t0.Add(60 * day) // 已过保但仍早于原承诺到期时刻
)

// epNewStore 构造主例：登记保修三十天、BROKEN_SEAL 在除外清单中的产品，初始
// 十件的备件和合格请求 r-epri-ok；预留八件后成功使用两件。结束时实物八件、
// 有效占用六件、可承诺两件，原承诺有效（未取消、未到期）。
func epNewStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct(epProductID, t0, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(epPartID, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(epQualifiedReq, epProductID, "NOISE"); err != nil {
		t.Fatalf("submit qualified request: %v", err)
	}
	if _, err := s.Reserve(epFirstCommit, epQualifiedReq, epPartID, 8, epCommitExpiry, epReserveAt); err != nil {
		t.Fatalf("reserve eight units: %v", err)
	}
	if _, err := s.Use(epFirstUse, epFirstCommit, 2, epReserveAt); err != nil {
		t.Fatalf("use two units: %v", err)
	}
	return s
}

// epAssertAccount 断言备件三项账目为期望值。
func epAssertAccount(t *testing.T, s *Store, now time.Time, physical, occupied, committable int) {
	t.Helper()
	st, err := s.PartStatus(epPartID, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock account = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// epAssertFirstCommitmentUnchanged 断言八件原承诺的数量账目没有被任何拒绝
// 改动：原定八件、已用两件、未用六件，未取消、未确认到期，在 now（早于其
// 到期时刻）查明细仍为 active。
func epAssertFirstCommitmentUnchanged(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	c, err := s.Commitment(epFirstCommit)
	if err != nil {
		t.Fatalf("load first commitment: %v", err)
	}
	if c.Quantity != 8 || c.Used != 2 || c.Unused() != 6 || c.Canceled || c.Expired ||
		!c.Expiry.Equal(epCommitExpiry) || c.RequestID != epQualifiedReq || c.PartID != epPartID {
		t.Fatalf("first commitment altered: %+v", c)
	}
	st, err := s.PartStatus(epPartID, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	var detail *CommitmentDetail
	for i := range st.Details {
		if st.Details[i].CommitmentID == epFirstCommit {
			detail = &st.Details[i]
		}
	}
	if detail == nil {
		t.Fatalf("first commitment missing from details: %+v", st.Details)
	}
	if detail.OriginalQuantity != 8 || detail.UsedQuantity != 2 ||
		detail.RemainingQuantity != 6 || detail.Status != CommitmentActive {
		t.Fatalf("first commitment detail altered: %+v", detail)
	}
}

// epAssertEligibilityEqual 断言两份资格依据结论完全一致（含拒绝原因列表的
// 内容与次序），用于核对历史快照与按同一时刻直接查询的结论一致。
func epAssertEligibilityEqual(t *testing.T, got, want *Eligibility, ctx string) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("%s: eligibility must not be nil: got=%v want=%v", ctx, got, want)
	}
	if got.RequestID != want.RequestID || got.ProductID != want.ProductID ||
		got.FaultCode != want.FaultCode || got.Eligible != want.Eligible ||
		got.Excluded != want.Excluded || got.WarrantyDays != want.WarrantyDays ||
		!got.PurchaseTime.Equal(want.PurchaseTime) ||
		!got.WarrantyExpiry.Equal(want.WarrantyExpiry) {
		t.Fatalf("%s: eligibility basis mismatch:\n got=%+v\nwant=%+v", ctx, got, want)
	}
	if len(got.Reasons) != len(want.Reasons) {
		t.Fatalf("%s: reasons = %v, want %v", ctx, got.Reasons, want.Reasons)
	}
	for i := range got.Reasons {
		if got.Reasons[i] != want.Reasons[i] {
			t.Fatalf("%s: reasons = %v, want %v", ctx, got.Reasons, want.Reasons)
		}
	}
}

// epAssertIneligibleRecord 核对一条 ineligible 失败记录：归属 reqID、序号
// seq、保存本次提交的编号/备件/三件数量与时刻，资格依据与同一时刻的直接
// 查询结论一致且拒绝原因恰为 wantReasons，库存依据恰为 wantBasis。
func epAssertIneligibleRecord(t *testing.T, s *Store, reqID, commitID string,
	now time.Time, wantReasons []RejectionReason, wantExcluded bool, wantBasis StockBasis, seq int) {
	t.Helper()

	h, err := s.RequestHistory(reqID)
	if err != nil {
		t.Fatalf("history %q: %v", reqID, err)
	}
	if len(h) != seq {
		t.Fatalf("history %q length = %d, want %d", reqID, len(h), seq)
	}
	rec := h[seq-1]
	if rec.Seq != seq || rec.Success {
		t.Fatalf("record seq/success = %d/%v, want %d/false", rec.Seq, rec.Success, seq)
	}
	if rec.Error != HistoryErrorIneligible {
		t.Fatalf("error category = %q, want %q", rec.Error, HistoryErrorIneligible)
	}
	if rec.CommitID != commitID || rec.PartID != epPartID || rec.Quantity != 3 {
		t.Fatalf("submission = %q/%q/%d, want %q/%q/3",
			rec.CommitID, rec.PartID, rec.Quantity, commitID, epPartID)
	}
	if !rec.Expiry.Equal(epReqExpiry) || !rec.Now.Equal(now) {
		t.Fatalf("submitted times = expiry %v now %v, want %v / %v",
			rec.Expiry, rec.Now, epReqExpiry, now)
	}

	if rec.Eligibility == nil {
		t.Fatalf("history %q: eligibility basis must not be nil", reqID)
	}
	if rec.Eligibility.Eligible {
		t.Fatalf("history %q: eligible = true, want false", reqID)
	}
	if rec.Eligibility.Excluded != wantExcluded {
		t.Fatalf("history %q: excluded = %v, want %v", reqID, rec.Eligibility.Excluded, wantExcluded)
	}
	if len(rec.Eligibility.Reasons) != len(wantReasons) {
		t.Fatalf("history %q: reasons = %v, want %v", reqID, rec.Eligibility.Reasons, wantReasons)
	}
	for i := range wantReasons {
		if rec.Eligibility.Reasons[i] != wantReasons[i] {
			t.Fatalf("history %q: reasons = %v, want %v", reqID, rec.Eligibility.Reasons, wantReasons)
		}
	}
	// 资格依据与按同一时刻直接查询的结论一致。
	direct, err := s.Evaluate(reqID, now)
	if err != nil {
		t.Fatalf("direct evaluate %q: %v", reqID, err)
	}
	epAssertEligibilityEqual(t, rec.Eligibility, direct, "history vs Evaluate "+reqID)
	view, err := s.RequestView(reqID, now)
	if err != nil {
		t.Fatalf("request view %q: %v", reqID, err)
	}
	epAssertEligibilityEqual(t, rec.Eligibility, view.Eligibility, "history vs RequestView "+reqID)
	if len(view.Commitments) != 0 {
		t.Fatalf("rejected request %q gained commitments: %+v", reqID, view.Commitments)
	}

	if rec.StockBasis == nil {
		t.Fatalf("history %q: stock basis must not be nil (must not be treated as empty/zero)", reqID)
	}
	if *rec.StockBasis != wantBasis {
		t.Fatalf("history %q: stock basis = %+v, want %+v", reqID, *rec.StockBasis, wantBasis)
	}
}

// TestIneligibleBeatsInsufficientStockWhenStockTight 主例回归：可承诺只有
// 两件时，申请三件的首次预留只要资格不合格，就必须返回 ErrIneligible 而不是
// ErrInsufficientStock。逐例覆盖仅过保、仅命中除外清单、过保与除外同时
// 成立、当前时刻恰好达到保修截止时刻；被拒申请不产生承诺、不改变 8/6/2
// 账目与原承诺，历史新增 ineligible 记录且依据真实完整。
func TestIneligibleBeatsInsufficientStockWhenStockTight(t *testing.T) {
	s := epNewStore(t)

	// 主例基线：实物八件、有效占用六件、可承诺两件。
	epAssertAccount(t, s, epExpiredAt, 8, 6, 2)

	cases := []struct {
		name        string
		reqID       string
		fault       string
		commitID    string
		now         time.Time
		reasons     []RejectionReason
		wantExclude bool
	}{
		{
			name:        "expired_only",
			reqID:       epExpiredReq,
			fault:       "NOISE",
			commitID:    epExpiredCommit,
			now:         epExpiredAt,
			reasons:     []RejectionReason{ReasonWarrantyExpired},
			wantExclude: false,
		},
		{
			name:        "excluded_only_within_warranty",
			reqID:       epExcludedReq,
			fault:       "BROKEN_SEAL",
			commitID:    epExcludedCommit,
			now:         epWithinAt,
			reasons:     []RejectionReason{ReasonFaultExcluded},
			wantExclude: true,
		},
		{
			name:        "expired_and_excluded",
			reqID:       epBothReq,
			fault:       "BROKEN_SEAL",
			commitID:    epBothCommit,
			now:         epExpiredAt,
			reasons:     []RejectionReason{ReasonWarrantyExpired, ReasonFaultExcluded},
			wantExclude: true,
		},
		{
			name:        "current_time_exactly_at_warranty_deadline",
			reqID:       epBoundaryReq,
			fault:       "NOISE",
			commitID:    epBoundaryCommit,
			now:         epDeadline,
			reasons:     []RejectionReason{ReasonWarrantyExpired},
			wantExclude: false,
		},
	}

	wantBasis := StockBasis{PhysicalRemaining: 8, ActiveOccupied: 6, Committable: 2}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SubmitRequest(tc.reqID, epProductID, tc.fault); err != nil {
				t.Fatalf("submit request: %v", err)
			}

			_, err := s.Reserve(tc.commitID, tc.reqID, epPartID, 3, epReqExpiry, tc.now)
			if !errors.Is(err, ErrIneligible) {
				t.Fatalf("reserve: got %v, want ErrIneligible", err)
			}
			// 关键归属：不能因为可承诺只有两件就把不合格请求报成缺货。
			if errors.Is(err, ErrInsufficientStock) {
				t.Fatalf("ineligible reserve wrapped ErrInsufficientStock: %v", err)
			}

			// 拒绝不产生承诺，被拒编号仍未占用。
			if _, err := s.Commitment(tc.commitID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("commitment %q created by rejection: err=%v", tc.commitID, err)
			}

			// 历史新增一条 ineligible 失败记录，资格与库存依据真实完整。
			epAssertIneligibleRecord(t, s, tc.reqID, tc.commitID, tc.now,
				tc.reasons, tc.wantExclude, wantBasis, 1)

			// 原承诺与备件三项账目保持处理前的值：申请的三件没有进入占用。
			epAssertAccount(t, s, tc.now, 8, 6, 2)
			epAssertFirstCommitmentUnchanged(t, s, tc.now)
		})
	}

	// 四个用例的拒绝彼此不累积：最终账目仍是 8/6/2，原承诺仍是八件预留、
	// 两件已用、六件未用的有效承诺。
	epAssertAccount(t, s, epExpiredAt, 8, 6, 2)
	epAssertFirstCommitmentUnchanged(t, s, epExpiredAt)

	// 先占用库存的合格请求只留有首次成功记录；其库存依据是新增占用之前的
	// 10/0/10，使用两件与后续他人的失败都不改变这条记录。
	okHistory, err := s.RequestHistory(epQualifiedReq)
	if err != nil {
		t.Fatalf("qualified request history: %v", err)
	}
	if len(okHistory) != 1 {
		t.Fatalf("qualified request history length = %d, want 1", len(okHistory))
	}
	okRec := okHistory[0]
	if !okRec.Success || okRec.Error != "" || okRec.CommitID != epFirstCommit ||
		okRec.Quantity != 8 || !okRec.Expiry.Equal(epCommitExpiry) || !okRec.Now.Equal(epReserveAt) {
		t.Fatalf("qualified success record altered: %+v", okRec)
	}
	if okRec.Eligibility == nil || !okRec.Eligibility.Eligible ||
		okRec.Eligibility.Excluded || len(okRec.Eligibility.Reasons) != 0 {
		t.Fatalf("qualified eligibility snapshot altered: %+v", okRec.Eligibility)
	}
	if okRec.StockBasis == nil ||
		*okRec.StockBasis != (StockBasis{PhysicalRemaining: 10, ActiveOccupied: 0, Committable: 10}) {
		t.Fatalf("qualified stock basis altered: %+v", okRec.StockBasis)
	}

	// 已成功编号原样重试：即使本次当前时刻已过保，仍取回首次成功快照
	// （已用数量为零、未取消），不重新判断资格与库存、不追加历史。
	snap, err := s.Reserve(epFirstCommit, epQualifiedReq, epPartID, 8, epCommitExpiry, epRetryAt)
	if err != nil {
		t.Fatalf("identical retry of succeeded id: %v", err)
	}
	if snap.Quantity != 8 || snap.Used != 0 || snap.Canceled || snap.Expired {
		t.Fatalf("retry snapshot = %+v, want first snapshot (8/used0/not closed)", snap)
	}
	okHistory2, err := s.RequestHistory(epQualifiedReq)
	if err != nil {
		t.Fatalf("qualified request history after retry: %v", err)
	}
	if len(okHistory2) != 1 {
		t.Fatalf("identical retry appended history: length = %d, want 1", len(okHistory2))
	}
	epAssertAccount(t, s, epRetryAt, 8, 6, 2)

	// 尚未成功预留的被拒编号可按原内容再次提交：仍是资格拒绝（不是缺货），
	// 再追加一条同样完整的 ineligible 记录，仍不产生承诺、不改动账目。
	_, err = s.Reserve(epBothCommit, epBothReq, epPartID, 3, epReqExpiry, epExpiredAt)
	if !errors.Is(err, ErrIneligible) {
		t.Fatalf("reuse rejected id: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment(epBothCommit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected id became a commitment: err=%v", err)
	}
	epAssertIneligibleRecord(t, s, epBothReq, epBothCommit, epExpiredAt,
		[]RejectionReason{ReasonWarrantyExpired, ReasonFaultExcluded}, true, wantBasis, 2)
	epAssertAccount(t, s, epExpiredAt, 8, 6, 2)
	epAssertFirstCommitmentUnchanged(t, s, epExpiredAt)
}

// TestEligibleRequestStillRejectedByStock 对照：产品在保且故障未被除外时，
// 库存不足仍按现有规则返回 ErrInsufficientStock；历史保存合格资格依据与
// 真实库存依据 8/6/2，类别为 insufficient_stock，不产生承诺。
func TestEligibleRequestStillRejectedByStock(t *testing.T) {
	s := epNewStore(t)
	if err := s.SubmitRequest(epGoodReq, epProductID, "NOISE"); err != nil {
		t.Fatalf("submit good request: %v", err)
	}

	_, err := s.Reserve(epGoodCommit, epGoodReq, epPartID, 3, epReqExpiry, epWithinAt)
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve eligible but short: got %v, want ErrInsufficientStock", err)
	}
	if errors.Is(err, ErrIneligible) {
		t.Fatalf("stock rejection wrapped ErrIneligible: %v", err)
	}
	if _, err := s.Commitment(epGoodCommit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stockout rejection created commitment: err=%v", err)
	}

	h, err := s.RequestHistory(epGoodReq)
	if err != nil {
		t.Fatalf("good request history: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("good history length = %d, want 1", len(h))
	}
	rec := h[0]
	if rec.Seq != 1 || rec.Success || rec.Error != HistoryErrorInsufficientStock {
		t.Fatalf("record = seq %d success %v error %q, want 1/false/%q",
			rec.Seq, rec.Success, rec.Error, HistoryErrorInsufficientStock)
	}
	if rec.CommitID != epGoodCommit || rec.PartID != epPartID || rec.Quantity != 3 ||
		!rec.Expiry.Equal(epReqExpiry) || !rec.Now.Equal(epWithinAt) {
		t.Fatalf("submission content altered: %+v", rec)
	}
	// 合格资格依据与同一时刻直接查询一致：合格、无拒绝原因、未命中除外。
	direct, err := s.Evaluate(epGoodReq, epWithinAt)
	if err != nil {
		t.Fatalf("direct evaluate: %v", err)
	}
	if rec.Eligibility == nil {
		t.Fatal("eligible stockout record missing eligibility basis")
	}
	epAssertEligibilityEqual(t, rec.Eligibility, direct, "stockout history vs Evaluate")
	if !rec.Eligibility.Eligible || rec.Eligibility.Excluded || len(rec.Eligibility.Reasons) != 0 {
		t.Fatalf("stockout record kept ineligible basis: %+v", rec.Eligibility)
	}
	// 库存依据是处理前真实账目 8/6/2，而不是空依据或零值。
	if rec.StockBasis == nil ||
		*rec.StockBasis != (StockBasis{PhysicalRemaining: 8, ActiveOccupied: 6, Committable: 2}) {
		t.Fatalf("stock basis = %+v, want 8/6/2", rec.StockBasis)
	}

	epAssertAccount(t, s, epWithinAt, 8, 6, 2)
	epAssertFirstCommitmentUnchanged(t, s, epWithinAt)
}

// TestIneligibleBeatsStockWhenCommittableZero 边界：再为合格请求追加两件
// 占用，使可承诺恰好为零（实物八件、占用八件）。此时不合格请求（过保加
// 除外，以及恰在保修截止时刻的仅过保）仍必须返回 ErrIneligible，历史的
// 库存依据是真实的 8/8/0——可承诺为零不能把资格拒绝改报成缺货，实物也
// 不能被记成零；在保且未除外的合格请求申请一件则按缺货拒绝，依据同为
// 8/8/0 且资格合格。
func TestIneligibleBeatsStockWhenCommittableZero(t *testing.T) {
	s := epNewStore(t)

	// 再占用剩余可承诺两件：8/6/2 -> 8/8/0。
	if _, err := s.Reserve(epTailCommit, epQualifiedReq, epPartID, 2, epCommitExpiry, epReserveAt); err != nil {
		t.Fatalf("reserve tail two units: %v", err)
	}
	epAssertAccount(t, s, epWithinAt, 8, 8, 0)

	wantBasis := StockBasis{PhysicalRemaining: 8, ActiveOccupied: 8, Committable: 0}

	// 过保且命中除外清单：申请三件，资格拒绝优先于零库存。
	if err := s.SubmitRequest(epZeroReq, epProductID, "BROKEN_SEAL"); err != nil {
		t.Fatalf("submit zero-case request: %v", err)
	}
	if _, err := s.Reserve(epZeroCommit, epZeroReq, epPartID, 3, epReqExpiry, epExpiredAt); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve ineligible at zero committable: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment(epZeroCommit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("zero-committable rejection created commitment: err=%v", err)
	}
	epAssertIneligibleRecord(t, s, epZeroReq, epZeroCommit, epExpiredAt,
		[]RejectionReason{ReasonWarrantyExpired, ReasonFaultExcluded}, true, wantBasis, 1)

	// 恰在保修截止时刻（仅过保）申请三件：同样是资格拒绝。
	if err := s.SubmitRequest(epZeroEdgeReq, epProductID, "NOISE"); err != nil {
		t.Fatalf("submit boundary zero-case request: %v", err)
	}
	if _, err := s.Reserve(epZeroEdgeCommit, epZeroEdgeReq, epPartID, 3, epReqExpiry, epDeadline); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve boundary-ineligible at zero committable: got %v, want ErrIneligible", err)
	}
	epAssertIneligibleRecord(t, s, epZeroEdgeReq, epZeroEdgeCommit, epDeadline,
		[]RejectionReason{ReasonWarrantyExpired}, false, wantBasis, 1)

	// 合格对照：在保且未除外，申请一件也因可承诺为零按缺货拒绝。
	if err := s.SubmitRequest(epZeroGoodReq, epProductID, "NOISE"); err != nil {
		t.Fatalf("submit zero-case good request: %v", err)
	}
	if _, err := s.Reserve(epZeroGoodCommit, epZeroGoodReq, epPartID, 1, epReqExpiry, epWithinAt); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve eligible at zero committable: got %v, want ErrInsufficientStock", err)
	}
	goodHistory, err := s.RequestHistory(epZeroGoodReq)
	if err != nil {
		t.Fatalf("zero-case good history: %v", err)
	}
	if len(goodHistory) != 1 || goodHistory[0].Error != HistoryErrorInsufficientStock ||
		goodHistory[0].Eligibility == nil || !goodHistory[0].Eligibility.Eligible {
		t.Fatalf("zero-case good record altered: %+v", goodHistory)
	}
	if goodHistory[0].StockBasis == nil || *goodHistory[0].StockBasis != wantBasis {
		t.Fatalf("zero-case good stock basis = %+v, want 8/8/0", goodHistory[0].StockBasis)
	}

	// 两次资格拒绝与一次缺货拒绝都不产生承诺、不改变账目：仍为 8/8/0，
	// 明细只有八件（已用两件、未用六件）与两件（未用）两笔有效承诺。
	epAssertAccount(t, s, epExpiredAt, 8, 8, 0)
	st, err := s.PartStatus(epPartID, epExpiredAt)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details = %+v, want only the two successful commitments", st.Details)
	}
	first := st.Details[0] // 按承诺编号排序，c-epri-first 在 c-epri-tail 之前
	tail := st.Details[1]
	if first.CommitmentID != epFirstCommit || first.OriginalQuantity != 8 ||
		first.UsedQuantity != 2 || first.RemainingQuantity != 6 || first.Status != CommitmentActive {
		t.Fatalf("first commitment detail altered under zero committable: %+v", first)
	}
	if tail.CommitmentID != epTailCommit || tail.OriginalQuantity != 2 ||
		tail.UsedQuantity != 0 || tail.RemainingQuantity != 2 || tail.Status != CommitmentActive {
		t.Fatalf("tail commitment detail altered: %+v", tail)
	}
	epAssertFirstCommitmentUnchanged(t, s, epExpiredAt)
}
