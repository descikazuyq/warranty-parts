package warranty

import (
	"errors"
	"testing"
	"time"
)

// 购买时刻边界回归：产品购买时刻为 t0（保修 30 天、除外 FAULTX），备件初始
// 库存 10 件，预留数量 4 件，承诺到期时刻晚于购买时刻。当前时刻在购买时刻
// 之前时一律不合格且拒绝原因只有购买时间在未来；当前时刻恰好等于购买时刻
// 时未命中除外规则的请求开始合格。

func TestReserveBeforePurchaseTime(t *testing.T) {
	s := newStore(t)
	now := t0.Add(-time.Second)

	// 资格查询：不合格，拒绝原因只有购买时间在未来。
	e, err := s.Evaluate("r1", now)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || len(e.Reasons) != 1 || e.Reasons[0] != ReasonPurchaseInFuture {
		t.Fatalf("before purchase: got %+v", e)
	}

	// 请求视图同样显示不合格，且没有任何承诺明细。
	view, err := s.RequestView("r1", now)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if view.Eligibility.Eligible || len(view.Eligibility.Reasons) != 1 ||
		view.Eligibility.Reasons[0] != ReasonPurchaseInFuture {
		t.Fatalf("view eligibility: got %+v", view.Eligibility)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("view commitments before reserve: %+v", view.Commitments)
	}

	// 库存充足也不能让未来购买的请求提前获得承诺。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, now); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve before purchase: got %v, want ErrIneligible", err)
	}
	// 不生成承诺。
	if _, err := s.Commitment("c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("commitment after rejection: got %v, want ErrNotFound", err)
	}
	// 请求下仍不出现承诺明细。
	view, err = s.RequestView("r1", now)
	if err != nil {
		t.Fatalf("request view after rejection: %v", err)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("view commitments after rejection: %+v", view.Commitments)
	}
	// 实物剩余与可承诺数量仍为 10，有效占用为 0。
	st, err := s.PartStatus("part1", now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 0 || st.Committable != 10 {
		t.Fatalf("stock after rejection: phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if len(st.Details) != 0 {
		t.Fatalf("part details after rejection: %+v", st.Details)
	}
}

func TestReserveSubSecondBeforePurchaseTime(t *testing.T) {
	s := NewStore()
	// 购买时间带秒以下精度：当前时刻与它处于同一秒但仍早于它。
	purchase := t0.Add(500 * time.Millisecond)
	if err := s.RegisterProduct("p1", purchase, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	now := t0 // 与购买时刻同一秒，早 500 毫秒。

	e, err := s.Evaluate("r1", now)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || len(e.Reasons) != 1 || e.Reasons[0] != ReasonPurchaseInFuture {
		t.Fatalf("same-second earlier now: got %+v", e)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, now); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve same-second earlier: got %v, want ErrIneligible", err)
	}
	st, err := s.PartStatus("part1", now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 0 || st.Committable != 10 {
		t.Fatalf("stock after rejection: phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
}

func TestReserveAtPurchaseTimeRetryAfterRejection(t *testing.T) {
	s := newStore(t)
	before := t0.Add(-time.Second)

	// 购买时刻之前：首次预留被拒绝，失败不占用承诺编号。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, before); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve before purchase: got %v, want ErrIneligible", err)
	}

	// 当前时刻恰好等于购买时刻：开始合格。
	e, err := s.Evaluate("r1", t0)
	if err != nil {
		t.Fatalf("evaluate at purchase: %v", err)
	}
	if !e.Eligible || len(e.Reasons) != 0 {
		t.Fatalf("at purchase time: got %+v", e)
	}

	// 用前一次被拒绝的承诺编号和相同提交内容再次预留：成功获得 4 件承诺。
	c, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, t0)
	if err != nil {
		t.Fatalf("reserve at purchase time: %v", err)
	}
	if c.ID != "c1" || c.Quantity != 4 || c.Used != 0 || c.Canceled {
		t.Fatalf("bad commitment: %+v", c)
	}
	// 实物仍为 10，有效占用 4，可承诺 6。
	st, err := s.PartStatus("part1", t0)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 4 || st.Committable != 6 {
		t.Fatalf("stock after success: phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	// 请求视图出现这一条承诺明细。
	view, err := s.RequestView("r1", t0)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if len(view.Commitments) != 1 || view.Commitments[0].CommitmentID != "c1" ||
		view.Commitments[0].OriginalQuantity != 4 || view.Commitments[0].Status != CommitmentActive {
		t.Fatalf("view commitments: %+v", view.Commitments)
	}

	// 处理历史：先失败后成功各一条，前一次的拒绝依据不被覆盖。
	h, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2 (failure then success)", len(h))
	}
	fail, ok := h[0], h[1]
	if fail.Seq != 1 || fail.Success || fail.Error != HistoryErrorIneligible {
		t.Fatalf("first record should be ineligible failure: %+v", fail)
	}
	// 失败记录保留原提交时刻、购买时间与未来购买的拒绝原因。
	if !fail.Now.Equal(before) {
		t.Fatalf("failure record now = %v, want %v", fail.Now, before)
	}
	if fail.Eligibility == nil {
		t.Fatal("expected eligibility snapshot on failure record")
	}
	if fail.Eligibility.Eligible || !fail.Eligibility.PurchaseTime.Equal(t0) ||
		len(fail.Eligibility.Reasons) != 1 || fail.Eligibility.Reasons[0] != ReasonPurchaseInFuture {
		t.Fatalf("failure eligibility snapshot: %+v", fail.Eligibility)
	}
	// 失败记录的库存依据反映其处理前：实物 10、占用 0、可承诺 10。
	if fail.StockBasis == nil || fail.StockBasis.PhysicalRemaining != 10 ||
		fail.StockBasis.ActiveOccupied != 0 || fail.StockBasis.Committable != 10 {
		t.Fatalf("failure stock basis: %+v", fail.StockBasis)
	}
	// 成功记录另留一条，保存当次合格资格。
	if ok.Seq != 2 || !ok.Success || ok.Error != "" {
		t.Fatalf("second record should be success: %+v", ok)
	}
	if !ok.Now.Equal(t0) || ok.Eligibility == nil || !ok.Eligibility.Eligible ||
		len(ok.Eligibility.Reasons) != 0 {
		t.Fatalf("success eligibility snapshot: %+v", ok.Eligibility)
	}
	// 成功记录的库存依据同样反映处理前：不能把自己新增的 4 件算作已有占用。
	if ok.StockBasis == nil || ok.StockBasis.PhysicalRemaining != 10 ||
		ok.StockBasis.ActiveOccupied != 0 || ok.StockBasis.Committable != 10 {
		t.Fatalf("success stock basis: %+v", ok.StockBasis)
	}
}

func TestReserveExcludedFaultAcrossPurchaseBoundary(t *testing.T) {
	s := newStore(t)
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	before := t0.Add(-time.Second)

	// 购买时刻之前：同时说明购买时间在未来和故障被除外。
	e, err := s.Evaluate("rBad", before)
	if err != nil {
		t.Fatalf("evaluate before purchase: %v", err)
	}
	if e.Eligible || len(e.Reasons) != 2 ||
		!containsReason(e.Reasons, ReasonPurchaseInFuture) || !containsReason(e.Reasons, ReasonFaultExcluded) {
		t.Fatalf("before purchase: got %+v", e)
	}
	if _, err := s.Reserve("c1", "rBad", "part1", 4, expiryOK, before); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve before purchase: got %v, want ErrIneligible", err)
	}

	// 到达购买时刻：只消除购买时间在未来这一项，除外仍然拒绝。
	e, err = s.Evaluate("rBad", t0)
	if err != nil {
		t.Fatalf("evaluate at purchase: %v", err)
	}
	if e.Eligible || len(e.Reasons) != 1 || e.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("at purchase: got %+v", e)
	}
	if _, err := s.Reserve("c2", "rBad", "part1", 4, expiryOK, t0); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve at purchase: got %v, want ErrIneligible", err)
	}
	// 不生成承诺，也不占用数量。
	if _, err := s.Commitment("c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("commitment c1: got %v, want ErrNotFound", err)
	}
	if _, err := s.Commitment("c2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("commitment c2: got %v, want ErrNotFound", err)
	}
	st, err := s.PartStatus("part1", t0)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 0 || st.Committable != 10 {
		t.Fatalf("stock after rejections: phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 失败历史分别保存两个时刻的资格依据。
	h, err := s.RequestHistory("rBad")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	early, at := h[0], h[1]
	if early.Success || early.Error != HistoryErrorIneligible || !early.Now.Equal(before) {
		t.Fatalf("early record: %+v", early)
	}
	if early.Eligibility == nil || early.Eligibility.Eligible ||
		len(early.Eligibility.Reasons) != 2 ||
		!containsReason(early.Eligibility.Reasons, ReasonPurchaseInFuture) ||
		!containsReason(early.Eligibility.Reasons, ReasonFaultExcluded) {
		t.Fatalf("early eligibility snapshot: %+v", early.Eligibility)
	}
	if at.Success || at.Error != HistoryErrorIneligible || !at.Now.Equal(t0) {
		t.Fatalf("at-purchase record: %+v", at)
	}
	if at.Eligibility == nil || at.Eligibility.Eligible ||
		len(at.Eligibility.Reasons) != 1 || at.Eligibility.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("at-purchase eligibility snapshot: %+v", at.Eligibility)
	}
	// 两条记录的库存依据都反映各自处理前的库存：实物 10、占用 0、可承诺 10。
	for i, rec := range []HistoryRecord{early, at} {
		if rec.StockBasis == nil || rec.StockBasis.PhysicalRemaining != 10 ||
			rec.StockBasis.ActiveOccupied != 0 || rec.StockBasis.Committable != 10 {
			t.Fatalf("record %d stock basis: %+v", i, rec.StockBasis)
		}
	}
}
