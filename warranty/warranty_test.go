package warranty

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var (
	t0       = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	day      = 24 * time.Hour
	nowOK    = t0.Add(10 * day)
	expiryOK = t0.Add(60 * day)
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	return s
}

func TestReadyStillTrue(t *testing.T) {
	if !Ready() {
		t.Fatal("baseline not ready")
	}
}

func TestRegisterDuplicatesAndValidation(t *testing.T) {
	s := NewStore()

	if err := s.RegisterProduct("p1", t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 5); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit request: %v", err)
	}

	// 重复登记一律报错，原记录保留。
	if err := s.RegisterProduct("p1", t0, 30, nil); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate product: got %v, want ErrDuplicateID", err)
	}
	if err := s.RegisterPart("part1", 5); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate part: got %v, want ErrDuplicateID", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate request: got %v, want ErrDuplicateID", err)
	}
	// 原记录未被覆盖。
	if p, err := s.Product("p1"); err != nil || p.WarrantyDays != 30 {
		t.Fatalf("original product changed: %+v, err %v", p, err)
	}

	// 非法参数。
	if err := s.RegisterProduct("", t0, 30, nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty product id: got %v", err)
	}
	if err := s.RegisterProduct("p2", t0, 0, nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero warranty days: got %v", err)
	}
	if err := s.RegisterProduct("p3", t0, -1, nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("negative warranty days: got %v", err)
	}
	if err := s.RegisterPart("", 1); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty part id: got %v", err)
	}
	if err := s.RegisterPart("part2", -1); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("negative stock: got %v", err)
	}
	if err := s.SubmitRequest("", "p1", "F"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v", err)
	}
	if err := s.SubmitRequest("r2", "p1", ""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty fault code: got %v", err)
	}
}

func TestEvaluateEligible(t *testing.T) {
	s := newStore(t)
	e, err := s.Evaluate("r1", nowOK)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !e.Eligible || len(e.Reasons) != 0 {
		t.Fatalf("expected eligible, got %+v", e)
	}
	if e.WarrantyExpiry != t0.Add(30*day) {
		t.Fatalf("warranty expiry = %v, want %v", e.WarrantyExpiry, t0.Add(30*day))
	}
}

func TestEvaluateExpiryBoundary(t *testing.T) {
	s := newStore(t)
	// 到期时刻之前一秒：仍合格。
	if e, err := s.Evaluate("r1", t0.Add(30*day).Add(-time.Second)); err != nil || !e.Eligible {
		t.Fatalf("1s before expiry: eligible=%v err=%v", e.Eligible, err)
	}
	// 到期时刻开始（等于）：算过保。
	e, err := s.Evaluate("r1", t0.Add(30*day))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || len(e.Reasons) != 1 || e.Reasons[0] != ReasonWarrantyExpired {
		t.Fatalf("at expiry: got %+v", e)
	}
}

func TestEvaluateExcludedAndExpiredBothListed(t *testing.T) {
	s := newStore(t)
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// 第 31 天：已过保，且故障代码被除外 → 两项原因。
	e, err := s.Evaluate("rBad", t0.Add(31*day))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || len(e.Reasons) != 2 {
		t.Fatalf("expected two reasons, got %+v", e)
	}
	got := map[RejectionReason]bool{}
	for _, r := range e.Reasons {
		got[r] = true
	}
	if !got[ReasonWarrantyExpired] || !got[ReasonFaultExcluded] {
		t.Fatalf("reasons = %v, want both warranty_expired and fault_code_excluded", e.Reasons)
	}
	if !e.Excluded {
		t.Fatal("expected Excluded=true")
	}

	// 仅除外（保修期内）。
	e2, _ := s.Evaluate("rBad", nowOK)
	if e2.Eligible || len(e2.Reasons) != 1 || e2.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("excluded only: got %+v", e2)
	}
}

func TestEvaluatePurchaseInFuture(t *testing.T) {
	s := newStore(t)
	e, err := s.Evaluate("r1", t0.Add(-time.Second))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || len(e.Reasons) != 1 || e.Reasons[0] != ReasonPurchaseInFuture {
		t.Fatalf("future purchase: got %+v", e)
	}
}

func TestEvaluateUnknownProductAndMissingFaultCode(t *testing.T) {
	s := newStore(t)
	if err := s.SubmitRequest("rUnknown", "pMissing", "F"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.Evaluate("rUnknown", nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown product: got %v, want ErrNotFound", err)
	}
	if err := s.SubmitRequest("rEmpty", "p1", ""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty fault code at submit: got %v", err)
	}
}

func TestReserveSuccessAndStockMath(t *testing.T) {
	s := newStore(t)
	c, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if c.Quantity != 4 || c.Used != 0 || c.Unused() != 0+4 {
		t.Fatalf("bad commitment: %+v", c)
	}
	// 实物库存不变，可承诺量下降。
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 4 || st.Committable != 6 {
		t.Fatalf("stock math: phys=%d occupied=%d committable=%d", st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
}

func TestReserveIneligibleAndUnknown(t *testing.T) {
	s := newStore(t)
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.Reserve("c1", "rBad", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrIneligible) {
		t.Fatalf("ineligible reserve: got %v", err)
	}
	if _, err := s.Reserve("c2", "rMissing", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v", err)
	}
	if _, err := s.Reserve("c3", "r1", "partMissing", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown part: got %v", err)
	}
	if _, err := s.Reserve("c4", "r1", "part1", 0, expiryOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero quantity: got %v", err)
	}
	if _, err := s.Reserve("c5", "r1", "part1", 1, nowOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("expiry equal to now: got %v", err)
	}
	if _, err := s.Reserve("c6", "r1", "part1", 1, nowOK.Add(-time.Second), nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("expiry in past: got %v", err)
	}
	// 失败的预留不占用数量。
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 0 || st.Committable != 10 {
		t.Fatalf("failed reserves occupied stock: %+v", st)
	}
}

func TestReserveInsufficientStock(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 11, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("over-stock reserve: got %v", err)
	}
	// 恰好 10 可以。
	if _, err := s.Reserve("c2", "r1", "part1", 10, expiryOK, nowOK); err != nil {
		t.Fatalf("exact stock reserve: %v", err)
	}
	// 再来 1 不够。
	if _, err := s.Reserve("c3", "r1", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve after full: got %v", err)
	}
}

func TestReserveIdempotentAndConflict(t *testing.T) {
	s := newStore(t)
	c1, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 相同编号相同内容重试：返回同一承诺，不重复占用。
	c2, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if c2.ID != c1.ID || c2.Quantity != c1.Quantity {
		t.Fatalf("retry returned different commitment: %+v vs %+v", c2, c1)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 4 {
		t.Fatalf("retry double-occupied: %d", st.ActiveOccupied)
	}

	// 换数量 / 换备件 / 换请求 / 换到期时刻 → 冲突。
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict on quantity: got %v", err)
	}
	if _, err := s.Reserve("c1", "r1", "part2", 4, expiryOK, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict on part: got %v", err)
	}
	if _, err := s.Reserve("c1", "r2", "part1", 4, expiryOK, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict on request: got %v", err)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK.Add(time.Second), nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict on expiry: got %v", err)
	}
	// 冲突不改变任何记录。
	st2, _ := s.PartStatus("part1", nowOK)
	if st2.ActiveOccupied != 4 || st2.Committable != 6 {
		t.Fatalf("conflict changed state: %+v", st2)
	}
}

func TestReserveRejudgesEligibilityAtEachTime(t *testing.T) {
	s := newStore(t)
	// 过保时刻预留 → 拒绝。
	if _, err := s.Reserve("c1", "r1", "part1", 1, expiryOK, t0.Add(31*day)); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve after warranty expiry: got %v", err)
	}
	// 购买时刻之前 → 拒绝。
	if _, err := s.Reserve("c2", "r1", "part1", 1, expiryOK, t0.Add(-time.Second)); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve before purchase: got %v", err)
	}
}

func TestUsePartialAndExceedsFailsWhole(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 分批使用 2 + 3。
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use 2: %v", err)
	}
	if _, err := s.Use("u2", "c1", 3, nowOK); err != nil {
		t.Fatalf("use 3: %v", err)
	}
	c, _ := s.Commitment("c1")
	if c.Used != 5 || c.Unused() != 0 {
		t.Fatalf("commitment after uses: used=%d unused=%d", c.Used, c.Unused())
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 5 {
		t.Fatalf("physical remaining = %d, want 5", st.PhysicalRemaining)
	}
	// 已全部使用但仍 active（未取消、未到期）：这是数量不足，不是承诺关闭，
	// 新使用必须恰好返回 ErrUsageExceeded，不能与 ErrCommitmentClosed 互换。
	_, err := s.Use("u3", "c1", 1, nowOK)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use on fully-used active commitment: got %v, want ErrUsageExceeded", err)
	}
	if errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("fully-used active commitment must not report ErrCommitmentClosed: %v", err)
	}

	// 超未用数量：整次失败，不扣减。
	if _, err := s.Reserve("c2", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	if _, err := s.Use("u4", "c2", 4, nowOK); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use: got %v", err)
	}
	c2, _ := s.Commitment("c2")
	if c2.Used != 0 {
		t.Fatalf("failed use changed commitment: used=%d", c2.Used)
	}
	st2, _ := s.PartStatus("part1", nowOK)
	if st2.PhysicalRemaining != 5 {
		t.Fatalf("failed use changed stock: %d", st2.PhysicalRemaining)
	}
}

func TestUseIdempotentAndConflict(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	u1, err := s.Use("u1", "c1", 2, nowOK)
	if err != nil {
		t.Fatalf("use: %v", err)
	}
	// 相同编号相同内容重试：返回原结果，不再次扣减。
	u2, err := s.Use("u1", "c1", 2, nowOK)
	if err != nil {
		t.Fatalf("idempotent use: %v", err)
	}
	if u2.ID != u1.ID || u2.Quantity != 2 {
		t.Fatalf("retry returned different usage: %+v vs %+v", u2, u1)
	}
	c, _ := s.Commitment("c1")
	if c.Used != 2 {
		t.Fatalf("idempotent use double-deducted: used=%d", c.Used)
	}
	// 改数量 / 改承诺 → 冲突。
	if _, err := s.Use("u1", "c1", 3, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict on quantity: got %v", err)
	}
	if _, err := s.Use("u1", "c2", 2, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict on commitment: got %v", err)
	}
	// 冲突不改变记录。
	c2, _ := s.Commitment("c1")
	if c2.Used != 2 {
		t.Fatalf("conflict changed usage: used=%d", c2.Used)
	}
}

func TestUseAfterCancelOrExpireRetriesOriginal(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	// 取消后重试同一使用：仍返回原结果，不再次扣减。
	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	u, err := s.Use("u1", "c1", 2, nowOK)
	if err != nil || u.Quantity != 2 {
		t.Fatalf("use after cancel: %+v err %v", u, err)
	}
	c, _ := s.Commitment("c1")
	if c.Used != 2 {
		t.Fatalf("retry after cancel double-deducted: used=%d", c.Used)
	}

	// 到期后重试同一使用：仍返回原结果。
	s2 := newStore(t)
	if _, err := s2.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s2.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	later := expiryOK.Add(10 * day)
	u2, err := s2.Use("u1", "c1", 2, later)
	if err != nil || u2.Quantity != 2 {
		t.Fatalf("use after expiry: %+v err %v", u2, err)
	}
}

func TestCancelReleasesOnlyUnused(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	// 取消：只释放未用的 3，已用的 2 不回补实物库存。
	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 8 {
		t.Fatalf("physical after cancel = %d, want 8", st.PhysicalRemaining)
	}
	if st.ActiveOccupied != 0 || st.Committable != 8 {
		t.Fatalf("cancel did not release: occupied=%d committable=%d", st.ActiveOccupied, st.Committable)
	}
	// 重复取消不增加库存。
	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	st2, _ := s.PartStatus("part1", nowOK)
	if st2.PhysicalRemaining != 8 || st2.Committable != 8 {
		t.Fatalf("repeat cancel changed stock: %+v", st2)
	}
	// 取消后不能继续使用。
	if _, err := s.Use("u2", "c1", 1, nowOK); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use after cancel: got %v", err)
	}
}

func TestExpiryAutoReleasesAndBlocksUse(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	// 到期时刻之前：仍有效，可使用。
	if _, err := s.Use("u2", "c1", 1, expiryOK.Add(-time.Second)); err != nil {
		t.Fatalf("use just before expiry: %v", err)
	}
	// 到期时刻：自动失效，余量释放，后续使用被拒。
	if _, err := s.Use("u3", "c1", 1, expiryOK); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use at expiry: got %v", err)
	}
	st, _ := s.PartStatus("part1", expiryOK)
	if st.ActiveOccupied != 0 || st.Committable != 7 {
		t.Fatalf("expiry did not release: occupied=%d committable=%d", st.ActiveOccupied, st.Committable)
	}
	// 查询直接体现失效，无需清理。
	view, _ := s.RequestView("r1", expiryOK)
	found := false
	for _, d := range view.Commitments {
		if d.CommitmentID == "c1" {
			found = true
			if d.Status != CommitmentExpired {
				t.Fatalf("status = %q, want expired", d.Status)
			}
			if d.UsedQuantity != 3 || d.RemainingQuantity != 2 {
				t.Fatalf("detail: used=%d remaining=%d", d.UsedQuantity, d.RemainingQuantity)
			}
		}
	}
	if !found {
		t.Fatal("commitment not found in request view")
	}
}

func TestRequestView(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c2", "r1", "part1", 2, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	view, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if !view.Eligibility.Eligible {
		t.Fatalf("expected eligible: %+v", view.Eligibility)
	}
	if len(view.Commitments) != 2 {
		t.Fatalf("commitments = %d, want 2", len(view.Commitments))
	}
	// 按承诺编号排序。
	if view.Commitments[0].CommitmentID != "c1" || view.Commitments[1].CommitmentID != "c2" {
		t.Fatalf("order = %s, %s", view.Commitments[0].CommitmentID, view.Commitments[1].CommitmentID)
	}
	d := view.Commitments[0]
	if d.RequestID != "r1" || d.PartID != "part1" || d.OriginalQuantity != 3 ||
		d.UsedQuantity != 0 || d.RemainingQuantity != 3 || d.Status != CommitmentActive {
		t.Fatalf("bad detail: %+v", d)
	}
	// 未知请求。
	if _, err := s.RequestView("rMissing", nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request view: got %v", err)
	}
}

func TestPartStatusDetailsIncludeClosed(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.Reserve("c2", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	// 实物 10 - 2 = 8；有效占用 3；可承诺 5。
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 3 || st.Committable != 5 {
		t.Fatalf("stock: phys=%d occupied=%d committable=%d", st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details = %d, want 2 (canceled record still traceable)", len(st.Details))
	}
	byID := map[string]CommitmentDetail{}
	for _, d := range st.Details {
		byID[d.CommitmentID] = d
	}
	d1 := byID["c1"]
	if d1.Status != CommitmentCanceled || d1.UsedQuantity != 2 || d1.RemainingQuantity != 3 ||
		d1.OriginalQuantity != 5 || d1.RequestID != "r1" {
		t.Fatalf("canceled detail: %+v", d1)
	}
	d2 := byID["c2"]
	if d2.Status != CommitmentActive || d2.RemainingQuantity != 3 {
		t.Fatalf("active detail: %+v", d2)
	}
}

func TestConcurrentReservesDoNotOversell(t *testing.T) {
	s := newStore(t)
	const n = 30
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, failed := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Reserve(fmt.Sprintf("c%02d", i), "r1", "part1", 1, expiryOK, nowOK)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if errors.Is(err, ErrInsufficientStock) {
				failed++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if success != 10 || failed != 20 {
		t.Fatalf("success=%d failed=%d, want 10/20", success, failed)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 10 || st.Committable != 0 || st.PhysicalRemaining != 10 {
		t.Fatalf("after concurrency: phys=%d occupied=%d committable=%d", st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
}

func TestConcurrentIdempotentReserveSameID(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	results := make([]Commitment, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.Reserve("same", "r1", "part1", 1, expiryOK, nowOK)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if results[i].ID != "same" || results[i].Quantity != 1 {
			t.Fatalf("goroutine %d got %+v", i, results[i])
		}
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 1 {
		t.Fatalf("same-id concurrent reserve occupied %d", st.ActiveOccupied)
	}
}

func TestConcurrentUsesDoNotExceedStock(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 10, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, failed := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Use(fmt.Sprintf("u%02d", i), "c1", 1, nowOK)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if errors.Is(err, ErrUsageExceeded) || errors.Is(err, ErrCommitmentClosed) {
				failed++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if success != 10 || failed != 10 {
		t.Fatalf("use success=%d failed=%d, want 10/10", success, failed)
	}
	c, _ := s.Commitment("c1")
	if c.Used != 10 || c.Unused() != 0 {
		t.Fatalf("commitment: used=%d unused=%d", c.Used, c.Unused())
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 0 {
		t.Fatalf("physical remaining = %d, want 0", st.PhysicalRemaining)
	}
}
