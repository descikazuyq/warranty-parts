package warranty

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// newStore 已在 warranty_test.go 定义：p1（30 天保修、FAULTX 除外）、part1 库存 10、r1。

func mustReserve(t *testing.T, s *Store, commitID, requestID, partID string, qty int, expiry, now time.Time) Commitment {
	t.Helper()
	c, err := s.Reserve(commitID, requestID, partID, qty, expiry, now)
	if err != nil {
		t.Fatalf("reserve %s: %v", commitID, err)
	}
	return c
}

func TestReservationHistoryLookup(t *testing.T) {
	s := newStore(t)

	// 空请求编号。
	if _, err := s.ReservationHistory(""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v", err)
	}
	// 未知请求。
	if _, err := s.ReservationHistory("rMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v", err)
	}
	// 已知请求没有记录：返回非 nil 空列表。
	h, err := s.ReservationHistory("r1")
	if err != nil {
		t.Fatalf("empty history: %v", err)
	}
	if h == nil || len(h) != 0 {
		t.Fatalf("empty history = %#v, want non-nil empty slice", h)
	}
}

func TestReservationHistorySuccessSnapshot(t *testing.T) {
	s := newStore(t)
	mustReserve(t, s, "c1", "r1", "part1", 4, expiryOK, nowOK)

	h, err := s.ReservationHistory("r1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("entries = %d, want 1", len(h))
	}
	e := h[0]
	if e.Seq != 1 || e.CommitmentID != "c1" || e.PartID != "part1" ||
		e.Quantity != 4 || !e.Expiry.Equal(expiryOK) || !e.SubmittedAt.Equal(nowOK) {
		t.Fatalf("bad entry header: %+v", e)
	}
	if e.Outcome != HistorySucceeded {
		t.Fatalf("outcome = %q, want succeeded", e.Outcome)
	}
	if e.Eligibility == nil || e.Stock == nil {
		t.Fatalf("success missing basis: elig=%+v stock=%+v", e.Eligibility, e.Stock)
	}
	if !e.Eligibility.Eligible || len(e.Eligibility.Reasons) != 0 {
		t.Fatalf("success eligibility = %+v", e.Eligibility)
	}
	if e.Eligibility.PurchaseTime != t0 || e.Eligibility.WarrantyDays != 30 ||
		!e.Eligibility.WarrantyExpiry.Equal(t0.Add(30*day)) || e.Eligibility.Excluded {
		t.Fatalf("success eligibility basis wrong: %+v", e.Eligibility)
	}
	// 处理前：实物 10、占用 0、可承诺 10。
	if e.Stock.PhysicalRemaining != 10 || e.Stock.ActiveOccupied != 0 || e.Stock.Committable != 10 {
		t.Fatalf("success stock basis = %+v", e.Stock)
	}
}

func TestReservationHistoryIneligibleKeepsBothReasons(t *testing.T) {
	s := newStore(t)
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// 第 31 天且命中除外代码：过保 + 除外，两项原因都保留。
	if _, err := s.Reserve("c1", "rBad", "part1", 1, expiryOK, t0.Add(31*day)); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve: got %v, want ErrIneligible", err)
	}
	h, _ := s.ReservationHistory("rBad")
	if len(h) != 1 {
		t.Fatalf("entries = %d, want 1", len(h))
	}
	e := h[0]
	if e.Outcome != HistoryIneligible || e.CommitmentID != "c1" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Eligibility == nil || e.Stock == nil {
		t.Fatalf("ineligible record missing basis")
	}
	if e.Eligibility.Eligible || len(e.Eligibility.Reasons) != 2 {
		t.Fatalf("reasons = %v, want both", e.Eligibility.Reasons)
	}
	got := map[RejectionReason]bool{}
	for _, r := range e.Eligibility.Reasons {
		got[r] = true
	}
	if !got[ReasonWarrantyExpired] || !got[ReasonFaultExcluded] {
		t.Fatalf("reasons = %v", e.Eligibility.Reasons)
	}
	if !e.Eligibility.Excluded {
		t.Fatal("Excluded should be true")
	}
	if e.Eligibility.PurchaseTime != t0 || e.Eligibility.WarrantyDays != 30 ||
		!e.Eligibility.WarrantyExpiry.Equal(t0.Add(30*day)) {
		t.Fatalf("eligibility basis = %+v", e.Eligibility)
	}
	if e.Stock.PhysicalRemaining != 10 || e.Stock.ActiveOccupied != 0 || e.Stock.Committable != 10 {
		t.Fatalf("stock basis = %+v, want {10 0 10}", e.Stock)
	}
}

func TestReservationHistoryInsufficientStockBasis(t *testing.T) {
	s := newStore(t)
	// 11 > 10：失败，依据为处理前 {10 0 10}。
	if _, err := s.Reserve("c1", "r1", "part1", 11, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("overstock: got %v", err)
	}
	// 恰好 10 成功：依据仍是处理前 {10 0 10}。
	mustReserve(t, s, "c2", "r1", "part1", 10, expiryOK, nowOK)
	// 再要 1：处理前 {10 10 0}。
	if _, err := s.Reserve("c3", "r1", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("after full: got %v", err)
	}

	h, _ := s.ReservationHistory("r1")
	if len(h) != 3 {
		t.Fatalf("entries = %d, want 3", len(h))
	}
	want := []struct {
		outcome                    HistoryOutcome
		phys, occupied, commitable int
		commit                     string
		qty                        int
	}{
		{HistoryInsufficientStock, 10, 0, 10, "c1", 11},
		{HistorySucceeded, 10, 0, 10, "c2", 10},
		{HistoryInsufficientStock, 10, 10, 0, "c3", 1},
	}
	for i, w := range want {
		e := h[i]
		if e.Seq != i+1 || e.Outcome != w.outcome || e.CommitmentID != w.commit || e.Quantity != w.qty {
			t.Fatalf("entry %d = %+v, want outcome %s commit %s qty %d", i, e, w.outcome, w.commit, w.qty)
		}
		if e.Stock == nil || e.Stock.PhysicalRemaining != w.phys ||
			e.Stock.ActiveOccupied != w.occupied || e.Stock.Committable != w.commitable {
			t.Fatalf("entry %d stock = %+v, want {%d %d %d}", i, e.Stock, w.phys, w.occupied, w.commitable)
		}
		if e.Eligibility == nil || !e.Eligibility.Eligible {
			t.Fatalf("entry %d eligibility = %+v", i, e.Eligibility)
		}
	}
}

func TestReservationHistoryMissingProductAndPart(t *testing.T) {
	s := newStore(t)
	// 关联产品未登记的请求：失败历史仍可查询。
	if err := s.SubmitRequest("rNoProduct", "pMissing", "F"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.Reserve("c1", "rNoProduct", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing product: got %v", err)
	}
	h, err := s.ReservationHistory("rNoProduct")
	if err != nil {
		t.Fatalf("history for missing-product request: %v", err)
	}
	if len(h) != 1 || h[0].Outcome != HistoryNotFound {
		t.Fatalf("entry = %+v", h)
	}
	if h[0].Eligibility != nil || h[0].Stock != nil {
		t.Fatalf("missing product must leave basis empty: elig=%+v stock=%+v", h[0].Eligibility, h[0].Stock)
	}

	// 备件缺失（请求合格）：同样 not_found，依据明确为空，不能用合格/零库存代替。
	if _, err := s.Reserve("c2", "r1", "partMissing", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing part: got %v", err)
	}
	// 备件缺失且请求同时不合格时，仍按备件缺失处理，依据仍为空。
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.Reserve("c3", "rBad", "partMissing", 1, expiryOK, t0.Add(31*day)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing part precedes eligibility: got %v", err)
	}
	h1, _ := s.ReservationHistory("r1")
	hp := h1[0]
	if hp.Outcome != HistoryNotFound || hp.Eligibility != nil || hp.Stock != nil {
		t.Fatalf("missing part record = %+v", hp)
	}
	hb, _ := s.ReservationHistory("rBad")
	if hb[0].Outcome != HistoryNotFound || hb[0].Eligibility != nil || hb[0].Stock != nil {
		t.Fatalf("missing part + ineligible record = %+v", hb[0])
	}

	// 之后补登产品，早先缺失记录的依据保持为空，不被补算。
	if err := s.RegisterProduct("pMissing", t0, 30, nil); err != nil {
		t.Fatalf("register product late: %v", err)
	}
	h2, _ := s.ReservationHistory("rNoProduct")
	if h2[0].Eligibility != nil || h2[0].Stock != nil {
		t.Fatalf("late registration mutated historical record: %+v", h2[0])
	}
}

func TestReservationHistoryInvalidParam(t *testing.T) {
	s := newStore(t)

	// 数量非正整数。
	if _, err := s.Reserve("c1", "r1", "part1", 0, expiryOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero qty: got %v", err)
	}
	// 到期时刻不晚于当前时刻。
	if _, err := s.Reserve("c2", "r1", "part1", 1, nowOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("expiry == now: got %v", err)
	}
	// 备件编号为空。
	if _, err := s.Reserve("c3", "r1", "", 1, expiryOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty part: got %v", err)
	}

	h, _ := s.ReservationHistory("r1")
	if len(h) != 3 {
		t.Fatalf("entries = %d, want 3", len(h))
	}
	for i, e := range h {
		if e.Seq != i+1 || e.Outcome != HistoryInvalidParam {
			t.Fatalf("entry %d = %+v", i, e)
		}
		if e.Eligibility != nil || e.Stock != nil {
			t.Fatalf("invalid param entry %d must have empty basis: %+v", i, e)
		}
	}
	if h[0].CommitmentID != "c1" || h[0].Quantity != 0 || h[0].PartID != "part1" {
		t.Fatalf("zero-qty entry fields = %+v", h[0])
	}
	if h[2].PartID != "" {
		t.Fatalf("empty-part entry = %+v", h[2])
	}

	// 请求编号为空：无法挂到任何请求，也不留记录。
	if _, err := s.Reserve("c4", "", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request: got %v", err)
	}
	if h2, _ := s.ReservationHistory("r1"); len(h2) != 3 {
		t.Fatalf("empty-request submission created a record: %d", len(h2))
	}
}

func TestReservationHistoryIdempotentRetryNoAppend(t *testing.T) {
	s := newStore(t)
	mustReserve(t, s, "c1", "r1", "part1", 4, expiryOK, nowOK)
	// 不同当前时刻重试：同编号同内容仍返回同一承诺，不追加历史、不重复占用。
	c2 := mustReserve(t, s, "c1", "r1", "part1", 4, expiryOK, t0.Add(20*day))
	if c2.ID != "c1" {
		t.Fatalf("retry returned %+v", c2)
	}
	h, _ := s.ReservationHistory("r1")
	if len(h) != 1 {
		t.Fatalf("idempotent retry appended history: %d entries", len(h))
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 4 {
		t.Fatalf("retry double-occupied: %d", st.ActiveOccupied)
	}
}

func TestReservationHistoryConflictUnderSubmittedRequest(t *testing.T) {
	s := newStore(t)
	if err := s.SubmitRequest("r2", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}
	mustReserve(t, s, "c1", "r1", "part1", 4, expiryOK, nowOK)

	// c1 原属 r1，改请求为 r2 重提：ErrConflict，失败记录挂在 r2 下，不能挂到 r1。
	if _, err := s.Reserve("c1", "r2", "part1", 4, expiryOK, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-request reuse: got %v", err)
	}
	// 同请求改数量：失败记录挂在 r1 下。
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed qty: got %v", err)
	}

	h1, _ := s.ReservationHistory("r1")
	if len(h1) != 2 {
		t.Fatalf("r1 entries = %d, want 2", len(h1))
	}
	if h1[0].Outcome != HistorySucceeded || h1[1].Outcome != HistoryConflict ||
		h1[1].Quantity != 5 || h1[1].CommitmentID != "c1" {
		t.Fatalf("r1 history = %+v", h1)
	}
	if h1[1].Eligibility != nil || h1[1].Stock != nil {
		t.Fatalf("conflict record must have empty basis: %+v", h1[1])
	}
	h2, _ := s.ReservationHistory("r2")
	if len(h2) != 1 || h2[0].Outcome != HistoryConflict || h2[0].CommitmentID != "c1" {
		t.Fatalf("r2 history = %+v, want one conflict for c1", h2)
	}
	if h2[0].Eligibility != nil || h2[0].Stock != nil {
		t.Fatalf("conflict record must have empty basis: %+v", h2[0])
	}

	// 指定未知请求的冲突：按原约定报错，不在任何请求下留记录，也不创建请求。
	if _, err := s.Reserve("c1", "rMissing", "part1", 4, expiryOK, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict with unknown request: got %v", err)
	}
	if h3, _ := s.ReservationHistory("r1"); len(h3) != 2 {
		t.Fatalf("unknown-request conflict appended to r1: %d", len(h3))
	}
	if h4, _ := s.ReservationHistory("r2"); len(h4) != 1 {
		t.Fatalf("unknown-request conflict appended to r2: %d", len(h4))
	}
	if _, err := s.ReservationHistory("rMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request should still be unknown: %v", err)
	}
}

func TestReservationHistoryUnknownRequestCreatesNothing(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "rMissing", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve unknown request: got %v", err)
	}
	if _, err := s.ReservationHistory("rMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request history: got %v", err)
	}
	if _, err := s.Request("rMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("request should not be created: %v", err)
	}
	// 失败提交不占用承诺编号：条件具备后同编号可成功。
	if err := s.SubmitRequest("rMissing", "p1", "FAULTY"); err != nil {
		t.Fatalf("register request late: %v", err)
	}
	mustReserve(t, s, "c1", "rMissing", "part1", 1, expiryOK, nowOK)
}

func TestReservationHistoryFailedCommitIDReusableAndFailuresKept(t *testing.T) {
	s := newStore(t)
	// c1 先因过保失败，编号未被占用。
	if _, err := s.Reserve("c1", "r1", "part1", 1, expiryOK, t0.Add(31*day)); !errors.Is(err, ErrIneligible) {
		t.Fatalf("first attempt: got %v", err)
	}
	// 同一编号在保修期内重新提交成功；早先失败记录保留，不被后来成功覆盖。
	mustReserve(t, s, "c1", "r1", "part1", 1, expiryOK, nowOK)

	h, _ := s.ReservationHistory("r1")
	if len(h) != 2 {
		t.Fatalf("entries = %d, want 2", len(h))
	}
	if h[0].Seq != 1 || h[0].Outcome != HistoryIneligible || h[0].CommitmentID != "c1" {
		t.Fatalf("first entry = %+v", h[0])
	}
	if h[1].Seq != 2 || h[1].Outcome != HistorySucceeded || h[1].CommitmentID != "c1" {
		t.Fatalf("second entry = %+v", h[1])
	}
	// 失败提交没有占用数量：成功时处理前的可承诺量仍为 10。
	if h[1].Stock.Committable != 10 {
		t.Fatalf("failed attempt occupied stock: basis %+v", h[1].Stock)
	}
}

func TestReservationHistoryOrderFollowsProcessingNotTime(t *testing.T) {
	s := newStore(t)
	// 提交时刻前后颠倒：先在第 31 天失败，再在第 10 天成功。
	if _, err := s.Reserve("c1", "r1", "part1", 1, expiryOK, t0.Add(31*day)); !errors.Is(err, ErrIneligible) {
		t.Fatalf("late attempt: %v", err)
	}
	mustReserve(t, s, "c2", "r1", "part1", 1, expiryOK, nowOK)
	// 与成功记录同一提交时刻再失败一次。
	if _, err := s.Reserve("c3", "r1", "part1", 50, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("same-time attempt: %v", err)
	}

	h, _ := s.ReservationHistory("r1")
	if len(h) != 3 {
		t.Fatalf("entries = %d, want 3", len(h))
	}
	want := []struct {
		seq     int
		outcome HistoryOutcome
		at      time.Time
	}{
		{1, HistoryIneligible, t0.Add(31 * day)},
		{2, HistorySucceeded, nowOK},
		{3, HistoryInsufficientStock, nowOK},
	}
	for i, w := range want {
		if h[i].Seq != w.seq || h[i].Outcome != w.outcome || !h[i].SubmittedAt.Equal(w.at) {
			t.Fatalf("entry %d = %+v, want %+v", i, h[i], w)
		}
	}
}

func TestReservationHistoryFrozenAfterUseCancelAndExpire(t *testing.T) {
	// 使用 + 取消：实物 10-3=7、占用归零，但历史快照仍是提交前 {10 0 10} 与数量 5。
	s := newStore(t)
	mustReserve(t, s, "c1", "r1", "part1", 5, expiryOK, nowOK)
	if _, err := s.Use("u1", "c1", 3, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 7 || st.ActiveOccupied != 0 {
		t.Fatalf("setup stock = %+v", st)
	}
	h, _ := s.ReservationHistory("r1")
	e := h[0]
	if e.Quantity != 5 || e.Outcome != HistorySucceeded {
		t.Fatalf("entry changed: %+v", e)
	}
	if e.Stock.PhysicalRemaining != 10 || e.Stock.ActiveOccupied != 0 || e.Stock.Committable != 10 {
		t.Fatalf("basis changed after use+cancel: qty=%d stock=%+v", e.Quantity, e.Stock)
	}
	if !e.Eligibility.Eligible {
		t.Fatalf("eligibility basis changed: %+v", e.Eligibility)
	}

	// 到期：承诺失效、余量释放，历史快照同样不变。
	s2 := newStore(t)
	mustReserve(t, s2, "c1", "r1", "part1", 5, expiryOK, nowOK)
	if _, err := s2.Use("u1", "c1", 2, expiryOK.Add(-time.Second)); err != nil {
		t.Fatalf("use before expiry: %v", err)
	}
	st2, _ := s2.PartStatus("part1", expiryOK)
	if st2.ActiveOccupied != 0 || st2.PhysicalRemaining != 8 {
		t.Fatalf("expiry setup = %+v", st2)
	}
	h2, _ := s2.ReservationHistory("r1")
	e2 := h2[0]
	if e2.Quantity != 5 || e2.Stock.PhysicalRemaining != 10 || e2.Stock.ActiveOccupied != 0 || e2.Stock.Committable != 10 {
		t.Fatalf("basis changed after expiry: qty=%d stock=%+v", e2.Quantity, e2.Stock)
	}
}

func TestReservationHistoryDefensiveCopies(t *testing.T) {
	s := newStore(t)
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.Reserve("c1", "rBad", "part1", 1, expiryOK, t0.Add(31*day)); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve: got %v, want ErrIneligible", err)
	}

	h, _ := s.ReservationHistory("rBad")
	// 调用方任意修改返回的列表、元素和拒绝原因。
	h[0].Outcome = HistorySucceeded
	h[0].Eligibility.Reasons[0] = "tampered"
	h[0].Eligibility.Reasons = append(h[0].Eligibility.Reasons, "extra")
	h[0].Eligibility.WarrantyDays = 999
	h[0].Stock.PhysicalRemaining = -1
	h = append(h, HistoryEntry{Seq: 99})

	h2, _ := s.ReservationHistory("rBad")
	if len(h2) != 1 {
		t.Fatalf("mutation changed length: %d", len(h2))
	}
	e := h2[0]
	if e.Outcome != HistoryIneligible {
		t.Fatalf("outcome mutated: %q", e.Outcome)
	}
	if e.Eligibility.WarrantyDays != 30 {
		t.Fatalf("eligibility mutated: %+v", e.Eligibility)
	}
	if e.Stock.PhysicalRemaining != 10 {
		t.Fatalf("stock mutated: %+v", e.Stock)
	}
	if len(e.Eligibility.Reasons) != 2 {
		t.Fatalf("reasons mutated: %v", e.Eligibility.Reasons)
	}
	for _, r := range e.Eligibility.Reasons {
		if r == "tampered" || r == "extra" {
			t.Fatalf("reasons slice shared with caller: %v", e.Eligibility.Reasons)
		}
	}

	// 两次查询互不影响。
	h3, _ := s.ReservationHistory("rBad")
	if h3[0].Eligibility.WarrantyDays != 30 || len(h3[0].Eligibility.Reasons) != 2 {
		t.Fatalf("queries share storage: %+v", h3[0])
	}
}

func TestReservationHistoryConcurrentMatchesResults(t *testing.T) {
	s := newStore(t)
	const n = 30
	var wg sync.WaitGroup
	outcomes := make([]HistoryOutcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Reserve(fmt.Sprintf("c%02d", i), "r1", "part1", 1, expiryOK, nowOK)
			switch {
			case err == nil:
				outcomes[i] = HistorySucceeded
			case errors.Is(err, ErrInsufficientStock):
				outcomes[i] = HistoryInsufficientStock
			default:
				t.Errorf("unexpected err: %v", err)
			}
		}(i)
	}
	wg.Wait()

	h, err := s.ReservationHistory("r1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != n {
		t.Fatalf("entries = %d, want %d (one per actual submission)", len(h), n)
	}

	// 序号严格递增、按处理次序排列；每条记录的库存依据等于它处理前的真实库存。
	byCommit := map[string]HistoryEntry{}
	priorSuccess := 0
	for i, e := range h {
		if e.Seq != i+1 {
			t.Fatalf("seq = %d at index %d", e.Seq, i)
		}
		if _, dup := byCommit[e.CommitmentID]; dup {
			t.Fatalf("duplicate commit id in history: %s", e.CommitmentID)
		}
		byCommit[e.CommitmentID] = e
		if e.Stock == nil || e.Eligibility == nil {
			t.Fatalf("entry %s missing basis: %+v", e.CommitmentID, e)
		}
		// 同一 now 下，处理前的有效占用等于此前成功次数，实物始终 10。
		if e.Stock.ActiveOccupied != priorSuccess ||
			e.Stock.PhysicalRemaining != 10 ||
			e.Stock.Committable != 10-priorSuccess {
			t.Fatalf("entry %s basis %+v inconsistent with prior successes %d", e.CommitmentID, e.Stock, priorSuccess)
		}
		switch e.Outcome {
		case HistorySucceeded:
			if e.Stock.Committable < 1 {
				t.Fatalf("success %s with committable %d", e.CommitmentID, e.Stock.Committable)
			}
			priorSuccess++
		case HistoryInsufficientStock:
			if e.Stock.Committable != 0 {
				t.Fatalf("failure %s with committable %d", e.CommitmentID, e.Stock.Committable)
			}
		default:
			t.Fatalf("unexpected outcome %s", e.Outcome)
		}
	}
	if priorSuccess != 10 {
		t.Fatalf("succeeded records = %d, want 10", priorSuccess)
	}

	// 每条记录必须与同一次调用实际返回的结果一致。
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%02d", i)
		if byCommit[id].Outcome != outcomes[i] {
			t.Fatalf("commit %s: history says %s, call returned %s", id, byCommit[id].Outcome, outcomes[i])
		}
	}

	// 不存在“只占用库存却没有成功记录”的中间状态：成功记录数即占用量。
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != priorSuccess || st.Committable != 0 {
		t.Fatalf("stock %+v inconsistent with history", st)
	}
}

func TestReservationHistoryConcurrentSameIDOneEntry(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Reserve("same", "r1", "part1", 1, expiryOK, nowOK); err != nil {
				t.Errorf("concurrent same-id reserve: %v", err)
			}
		}()
	}
	wg.Wait()
	h, _ := s.ReservationHistory("r1")
	if len(h) != 1 || h[0].Outcome != HistorySucceeded {
		t.Fatalf("same-id history = %+v, want single success", h)
	}
}
