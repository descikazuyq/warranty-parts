package warranty

import (
	"errors"
	"math"
	"testing"
	"time"
)

// 较长保修期限（超过 time.Duration 上限约 106751 天）的截止时刻必须按
// 购买时刻起完整天数（每天二十四小时）计算，不得溢出回绕或共用上限。
func TestLongWarrantyExpiry(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, days := range []int{106751, 106752, 200000} {
		s := NewStore()
		id := "p"
		if err := s.RegisterProduct(id, purchase, days, nil); err != nil {
			t.Fatalf("RegisterProduct(%d days): %v", days, err)
		}
		if err := s.SubmitRequest("r1", id, "F1"); err != nil {
			t.Fatal(err)
		}
		// 购买当天查询应当合格。
		elig, err := s.Evaluate("r1", purchase)
		if err != nil {
			t.Fatalf("Evaluate(%d days): %v", days, err)
		}
		if !elig.Eligible {
			t.Fatalf("%d days: purchase-day request rejected, reasons %v", days, elig.Reasons)
		}
		// 期望值分两段累加，避免单次 Add 的 Duration 溢出。
		d1, d2 := days/2, days-days/2
		want := purchase.Add(time.Duration(d1) * 24 * time.Hour).Add(time.Duration(d2) * 24 * time.Hour)
		if !elig.WarrantyExpiry.Equal(want) {
			t.Fatalf("%d days: expiry %v, want %v", days, elig.WarrantyExpiry, want)
		}
		// 截止时刻前一刻合格，截止时刻起过保。
		before := elig.WarrantyExpiry.Add(-time.Second)
		if e, _ := s.Evaluate("r1", before); !e.Eligible {
			t.Fatalf("%d days: one second before expiry should be eligible", days)
		}
		if e, _ := s.Evaluate("r1", elig.WarrantyExpiry); e.Eligible ||
			!containsReason(e.Reasons, ReasonWarrantyExpired) {
			t.Fatalf("%d days: at expiry should be expired, got %+v", days, e)
		}
	}
	// 三个期限的截止时刻互不相同，不共用某个上限。
	s := NewStore()
	expiries := map[int]time.Time{}
	ids := map[int]string{106751: "pa", 106752: "pb", 200000: "pc"}
	for _, days := range []int{106751, 106752, 200000} {
		id := ids[days]
		if err := s.RegisterProduct(id, purchase, days, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitRequest(id, id, "F1"); err != nil {
			t.Fatal(err)
		}
		e, err := s.Evaluate(id, purchase)
		if err != nil {
			t.Fatal(err)
		}
		expiries[days] = e.WarrantyExpiry
	}
	if expiries[106751].Equal(expiries[106752]) || expiries[106752].Equal(expiries[200000]) {
		t.Fatalf("distinct terms share an expiry: %v", expiries)
	}
	if got := expiries[200000].Sub(expiries[106752]); got != time.Duration(200000-106752)*24*time.Hour {
		t.Fatalf("gap between 200000 and 106752 day expiries = %v", got)
	}
}

// 购买时刻的秒以下精度必须保留在截止时刻中；时区只影响表示，不把期限
// 改成自然日、不把截止时刻对齐到当地零点。
func TestLongWarrantyExpiryPrecisionAndZone(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 123456789, loc)
	s := NewStore()
	if err := s.RegisterProduct("p", purchase, 106752, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitRequest("r", "p", "F1"); err != nil {
		t.Fatal(err)
	}
	e, err := s.Evaluate("r", purchase)
	if err != nil {
		t.Fatal(err)
	}
	if e.WarrantyExpiry.Nanosecond() != 123456789 {
		t.Fatalf("sub-second precision lost: %v", e.WarrantyExpiry)
	}
	// Sub 对超大间隔会饱和，用 Unix 秒比较实际跨度。
	if got := e.WarrantyExpiry.Unix() - purchase.Unix(); got != int64(106752)*24*3600 {
		t.Fatalf("span = %d seconds, want %d", got, int64(106752)*24*3600)
	}
	// 同一时刻换时区表示，截止时刻仍是同一实际时刻。
	e2, err := s.Evaluate("r", purchase.In(time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !e2.WarrantyExpiry.Equal(e.WarrantyExpiry) {
		t.Fatalf("zone changed the instant: %v vs %v", e2.WarrantyExpiry, e.WarrantyExpiry)
	}
}

// 截止时刻超出可表示范围的正整数期限登记失败（ErrInvalidParam），不保存
// 产品记录；同一编号随后以可表示的条款仍能正常登记。
func TestUnrepresentableWarrantyRejected(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewStore()
	if err := s.RegisterProduct("p", purchase, math.MaxInt, nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("MaxInt days: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Product("p"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected registration must not save the product, got %v", err)
	}
	if err := s.RegisterProduct("p", purchase, 200000, nil); err != nil {
		t.Fatalf("re-register with representable term: %v", err)
	}
	// 购买时刻接近可表示上限时，普通天数也可能不可表示。
	far := time.Unix(math.MaxInt64-62135596800-100, 0).UTC()
	if err := s.RegisterProduct("q", far, 2, nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("near-limit purchase: got %v, want ErrInvalidParam", err)
	}
}

// 首次预留与资格查询使用一致的截止时刻：截止前可预留，截止时刻起返回
// 不合格错误，不创建承诺、不占用备件，历史中的资格依据显示正确的
// 购买时刻、原保修天数和截止时刻。
func TestLongWarrantyReserveBoundary(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewStore()
	if err := s.RegisterProduct("p", purchase, 106752, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterPart("sp", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitRequest("r", "p", "F1"); err != nil {
		t.Fatal(err)
	}
	elig, err := s.Evaluate("r", purchase)
	if err != nil {
		t.Fatal(err)
	}
	expiry := elig.WarrantyExpiry
	// 承诺自身的到期时刻只需晚于本次提交的当前时刻，与保修截止时刻各自独立。
	commitExpiry := expiry.Add(24 * time.Hour)

	// 截止时刻前一秒：预留成功。
	if _, err := s.Reserve("c1", "r", "sp", 1, commitExpiry, expiry.Add(-time.Second)); err != nil {
		t.Fatalf("reserve before warranty expiry: %v", err)
	}
	// 截止时刻：不合格，不创建承诺、不占用备件。
	if _, err := s.Reserve("c2", "r", "sp", 1, commitExpiry, expiry); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve at warranty expiry: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("c2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected reserve must not create a commitment, got %v", err)
	}
	st, err := s.PartStatus("sp", expiry)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveOccupied != 1 {
		t.Fatalf("occupied = %d, want 1 (only the successful reserve)", st.ActiveOccupied)
	}

	hist, err := s.RequestHistory("r")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || !hist[0].Success || hist[1].Success || hist[1].Error != HistoryErrorIneligible {
		t.Fatalf("unexpected history: %+v", hist)
	}
	basis := hist[1].Eligibility
	if basis == nil {
		t.Fatal("rejected record missing eligibility basis")
	}
	if !basis.PurchaseTime.Equal(purchase) || basis.WarrantyDays != 106752 || !basis.WarrantyExpiry.Equal(expiry) {
		t.Fatalf("basis = (%v, %d, %v), want (%v, 106752, %v)",
			basis.PurchaseTime, basis.WarrantyDays, basis.WarrantyExpiry, purchase, expiry)
	}
	if !containsReason(basis.Reasons, ReasonWarrantyExpired) {
		t.Fatalf("rejection reasons %v missing warranty_expired", basis.Reasons)
	}
}

// 较长期限不能绕过其他拒绝条件：购买时刻在未来、故障代码命中除外清单
// 仍各自构成拒绝原因，过保与除外同时成立时保留两项原因。
func TestLongWarrantyOtherRejectionReasons(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewStore()
	if err := s.RegisterProduct("p", purchase, 106752, []string{"FX"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitRequest("r", "p", "FX"); err != nil {
		t.Fatal(err)
	}
	// 购买时刻晚于当前时刻。
	e, err := s.Evaluate("r", purchase.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if e.Eligible || !containsReason(e.Reasons, ReasonPurchaseInFuture) {
		t.Fatalf("future purchase: %+v", e)
	}
	// 除外故障代码。
	e, err = s.Evaluate("r", purchase)
	if err != nil {
		t.Fatal(err)
	}
	if e.Eligible || !containsReason(e.Reasons, ReasonFaultExcluded) {
		t.Fatalf("excluded fault: %+v", e)
	}
	// 过保与除外同时成立时保留两项原因。
	e, err = s.Evaluate("r", e.WarrantyExpiry)
	if err != nil {
		t.Fatal(err)
	}
	if e.Eligible || !containsReason(e.Reasons, ReasonWarrantyExpired) || !containsReason(e.Reasons, ReasonFaultExcluded) {
		t.Fatalf("expired+excluded: %+v", e)
	}
}

func containsReason(rs []RejectionReason, want RejectionReason) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}
