package warranty

import (
	"errors"
	"testing"
	"time"
)

// futurePurchaseStore 构造一个购买时刻晚于多数查询时刻的场景：
// 购买时刻带秒以下精度、保修 30 天、故障代码未被除外；备件初始库存十件。
func futurePurchaseStore(t *testing.T, purchase time.Time) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", purchase, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	return s
}

// wantStockBasis 校验实物剩余、有效占用与可承诺数量。
func wantStockBasis(t *testing.T, s *Store, now time.Time, physical, occupied, committable int) {
	t.Helper()
	st, err := s.PartStatus("part1", now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock = phys %d/occupy %d/committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// TestReserveFuturePurchaseRejected 主场景：当前时刻早于购买时刻时，资格查询与
// 请求视图都只有“购买时间在未来”一项拒绝原因；首次预留返回 ErrIneligible，
// 不生成承诺、不出现承诺明细、不占用任何数量。
func TestReserveFuturePurchaseRejected(t *testing.T) {
	// 购买时刻带秒以下精度：.500000000 秒。
	purchase := time.Date(2026, 2, 1, 12, 0, 0, 500_000_000, time.UTC)
	expiry := purchase.Add(60 * day) // 承诺到期时刻晚于购买时刻。
	s := futurePurchaseStore(t, purchase)

	// 与购买时刻处于同一秒、但仍早一毫秒：不能因为秒数相同就算已购买。
	before := time.Unix(purchase.Unix(), 499_000_000)
	if !before.Before(purchase) || before.Unix() != purchase.Unix() {
		t.Fatalf("test setup: %v should be in the same second as and before %v", before, purchase)
	}

	// 资格查询：不合格，拒绝原因只有购买时间在未来。
	e, err := s.Evaluate("r1", before)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || len(e.Reasons) != 1 || e.Reasons[0] != ReasonPurchaseInFuture {
		t.Fatalf("eligibility before purchase = %+v, want only purchase_time_in_future", e)
	}
	if !e.PurchaseTime.Equal(purchase) || e.PurchaseTime.Nanosecond() != purchase.Nanosecond() {
		t.Fatalf("purchase basis = %v, want %v", e.PurchaseTime, purchase)
	}
	if e.WarrantyDays != 30 || e.Excluded {
		t.Fatalf("warranty/excluded basis changed: %+v", e)
	}

	// 请求视图：同样不合格、只有未来购买一项原因，且没有任何承诺明细。
	view, err := s.RequestView("r1", before)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if view.Eligibility == nil || view.Eligibility.Eligible ||
		len(view.Eligibility.Reasons) != 1 || view.Eligibility.Reasons[0] != ReasonPurchaseInFuture {
		t.Fatalf("view eligibility before purchase = %+v", view.Eligibility)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("commitments before purchase = %+v, want none", view.Commitments)
	}

	// 首次预留：ErrIneligible，不生成承诺。
	if c, err := s.Reserve("c1", "r1", "part1", 4, expiry, before); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve before purchase = c %+v err %v, want ErrIneligible", c, err)
	}
	if _, err := s.Commitment("c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("commitment created despite rejection: %v", err)
	}
	view2, _ := s.RequestView("r1", before)
	if len(view2.Commitments) != 0 {
		t.Fatalf("commitment detail appeared under request: %+v", view2.Commitments)
	}

	// 实物仍为十件、有效占用为零、可承诺仍为十件。
	wantStockBasis(t, s, before, 10, 0, 10)

	// 秒以下精度边界：当前时刻只比购买时刻早一纳秒（仍在同一秒），照样拒绝。
	oneNanoBefore := purchase.Add(-time.Nanosecond)
	if oneNanoBefore.Unix() != purchase.Unix() || !oneNanoBefore.Before(purchase) {
		t.Fatalf("test setup: %v should be 1ns before %v in the same second", oneNanoBefore, purchase)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiry, oneNanoBefore); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve 1ns before purchase: got %v, want ErrIneligible", err)
	}
	wantStockBasis(t, s, oneNanoBefore, 10, 0, 10)
}

// TestReserveFuturePurchaseRejectedThenSucceedsAtBoundary 验证当前时刻恰好等于
// 购买时刻时开始合格：沿用被拒绝过的承诺编号和相同提交内容再次预留应成功取得
// 四件承诺，说明失败没有占用编号；库存为实物十件、有效占用四件、可承诺六件。
// 失败历史保留提前提交时的依据，不被后来的合格结论覆盖；两条记录的库存依据
// 都反映各自处理前的库存，成功记录不把自己新增的四件算进已有占用。
func TestReserveFuturePurchaseRejectedThenSucceedsAtBoundary(t *testing.T) {
	purchase := time.Date(2026, 2, 1, 12, 0, 0, 500_000_000, time.UTC)
	expiry := purchase.Add(60 * day)
	s := futurePurchaseStore(t, purchase)
	before := purchase.Add(-time.Nanosecond)

	if _, err := s.Reserve("c1", "r1", "part1", 4, expiry, before); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve before purchase: got %v, want ErrIneligible", err)
	}

	// 当前时刻恰好等于购买时刻：未命中除外规则，开始合格。
	c, err := s.Reserve("c1", "r1", "part1", 4, expiry, purchase)
	if err != nil {
		t.Fatalf("reserve at purchase time with the previously rejected id: %v", err)
	}
	if c.Quantity != 4 || c.Used != 0 || c.Canceled || c.Expired || !c.Expiry.Equal(expiry) {
		t.Fatalf("commitment = %+v, want 4 active pieces", c)
	}

	// 预留不扣减实物：实物十件、有效占用四件、可承诺六件。
	wantStockBasis(t, s, purchase, 10, 4, 6)

	// 请求视图此刻合格，且出现一条四件的有效承诺明细。
	view, err := s.RequestView("r1", purchase)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if !view.Eligibility.Eligible || len(view.Eligibility.Reasons) != 0 {
		t.Fatalf("eligibility at purchase = %+v, want eligible with no reasons", view.Eligibility)
	}
	if len(view.Commitments) != 1 {
		t.Fatalf("commitments = %+v, want one detail", view.Commitments)
	}
	d := view.Commitments[0]
	if d.CommitmentID != "c1" || d.OriginalQuantity != 4 || d.RemainingQuantity != 4 ||
		d.UsedQuantity != 0 || d.Status != CommitmentActive {
		t.Fatalf("commitment detail = %+v", d)
	}

	// 历史两条：先失败、后成功，序号严格递增，失败依据不被当前资格覆盖。
	h, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	failed, succeeded := h[0], h[1]
	if failed.Seq != 1 || failed.Success || failed.Error != HistoryErrorIneligible {
		t.Fatalf("first record = %+v, want seq 1 ineligible failure", failed)
	}
	if succeeded.Seq != 2 || !succeeded.Success || succeeded.Error != "" {
		t.Fatalf("second record = %+v, want seq 2 success", succeeded)
	}

	// 失败记录保留原提交时刻、购买时间、未来购买拒绝原因与不合格类别。
	if !failed.Now.Equal(before) {
		t.Fatalf("failure now = %v, want original submission time %v", failed.Now, before)
	}
	fe := failed.Eligibility
	if fe == nil {
		t.Fatal("failure record missing eligibility basis")
	}
	if fe.Eligible || len(fe.Reasons) != 1 || fe.Reasons[0] != ReasonPurchaseInFuture {
		t.Fatalf("failure eligibility = %+v, want ineligible with future-purchase reason only", fe)
	}
	if !fe.PurchaseTime.Equal(purchase) || fe.WarrantyDays != 30 {
		t.Fatalf("failure eligibility product basis = %+v", fe)
	}
	// 失败记录的库存依据反映处理前库存：10/0/10。
	if failed.StockBasis == nil || failed.StockBasis.PhysicalRemaining != 10 ||
		failed.StockBasis.ActiveOccupied != 0 || failed.StockBasis.Committable != 10 {
		t.Fatalf("failure stock basis = %+v, want 10/0/10", failed.StockBasis)
	}

	// 成功记录另留一条，资格为当次（购买时刻）合格，库存依据仍是处理前的
	// 10/0/10，不能把自己新增的四件算作已有占用。
	if !succeeded.Now.Equal(purchase) {
		t.Fatalf("success now = %v, want %v", succeeded.Now, purchase)
	}
	se := succeeded.Eligibility
	if se == nil || !se.Eligible || len(se.Reasons) != 0 {
		t.Fatalf("success eligibility = %+v, want eligible snapshot", se)
	}
	if !se.PurchaseTime.Equal(purchase) {
		t.Fatalf("success eligibility purchase = %v, want %v", se.PurchaseTime, purchase)
	}
	if succeeded.StockBasis == nil || succeeded.StockBasis.PhysicalRemaining != 10 ||
		succeeded.StockBasis.ActiveOccupied != 0 || succeeded.StockBasis.Committable != 10 {
		t.Fatalf("success stock basis = %+v, want pre-processing 10/0/10 (new 4 not counted)", succeeded.StockBasis)
	}
}

// TestReserveFuturePurchaseAndFaultExcludedBothReasons 购买时间边界与故障除外
// 叠加：购买时刻之前同时列出未来购买与故障被除外两项原因；到达购买时刻只消除
// 前一项，预留仍返回 ErrIneligible，不生成承诺或占用。两次失败分别保存各自
// 时刻的资格依据，调用方既能看到当前仍被除外拒绝，也能核对提前提交时的依据。
func TestReserveFuturePurchaseAndFaultExcludedBothReasons(t *testing.T) {
	purchase := time.Date(2026, 2, 1, 12, 0, 0, 500_000_000, time.UTC)
	expiry := purchase.Add(60 * day)
	s := NewStore()
	if err := s.RegisterProduct("p1", purchase, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	before := purchase.Add(-time.Nanosecond)

	reasons := func(t *testing.T, e *Eligibility) map[RejectionReason]bool {
		t.Helper()
		got := make(map[RejectionReason]bool, len(e.Reasons))
		for _, r := range e.Reasons {
			got[r] = true
		}
		return got
	}

	// 购买时刻之前：资格查询同时说明未来购买与故障被除外。
	e, err := s.Evaluate("r1", before)
	if err != nil {
		t.Fatalf("evaluate before purchase: %v", err)
	}
	got := reasons(t, e)
	if e.Eligible || !got[ReasonPurchaseInFuture] || !got[ReasonFaultExcluded] || len(got) != 2 {
		t.Fatalf("eligibility before purchase = %+v, want future + excluded", e)
	}

	// 提前预留：ErrIneligible，无承诺、无占用。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiry, before); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve before purchase: got %v, want ErrIneligible", err)
	}
	wantStockBasis(t, s, before, 10, 0, 10)

	// 到达购买时刻：只消除未来购买一项，故障仍被除外，预留依旧失败。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiry, purchase); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve at purchase with excluded fault: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("commitment created despite exclusion: %v", err)
	}
	wantStockBasis(t, s, purchase, 10, 0, 10)

	// 当前请求视图只显示故障被除外一项原因，且没有承诺明细。
	view, err := s.RequestView("r1", purchase)
	if err != nil {
		t.Fatalf("request view at purchase: %v", err)
	}
	if view.Eligibility == nil || view.Eligibility.Eligible ||
		len(view.Eligibility.Reasons) != 1 || view.Eligibility.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("current eligibility = %+v, want fault_code_excluded only", view.Eligibility)
	}
	if len(view.Commitments) != 0 {
		t.Fatalf("commitments = %+v, want none", view.Commitments)
	}

	// 两条失败历史分别保存两个时刻的资格依据。
	h, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2 failure records", len(h))
	}
	for i, rec := range h {
		if rec.Success || rec.Error != HistoryErrorIneligible {
			t.Fatalf("record %d = %+v, want ineligible failure", i, rec)
		}
		if rec.StockBasis == nil || rec.StockBasis.PhysicalRemaining != 10 ||
			rec.StockBasis.ActiveOccupied != 0 || rec.StockBasis.Committable != 10 {
			t.Fatalf("record %d stock basis = %+v, want 10/0/10", i, rec.StockBasis)
		}
	}
	early, later := h[0], h[1]
	if !early.Now.Equal(before) || early.Eligibility == nil {
		t.Fatalf("early record = %+v", early)
	}
	earlyReasons := reasons(t, early.Eligibility)
	if early.Eligibility.Eligible || !earlyReasons[ReasonPurchaseInFuture] ||
		!earlyReasons[ReasonFaultExcluded] {
		t.Fatalf("early eligibility basis = %+v, want future + excluded preserved", early.Eligibility)
	}
	if !later.Now.Equal(purchase) || later.Eligibility == nil {
		t.Fatalf("later record = %+v", later)
	}
	laterReasons := reasons(t, later.Eligibility)
	if later.Eligibility.Eligible || len(laterReasons) != 1 || !laterReasons[ReasonFaultExcluded] {
		t.Fatalf("later eligibility basis = %+v, want excluded only", later.Eligibility)
	}
	// 提前提交的依据不被当前资格覆盖。
	if len(early.Eligibility.Reasons) != 2 {
		t.Fatalf("early reasons overwritten by current eligibility: %+v", early.Eligibility.Reasons)
	}
}
