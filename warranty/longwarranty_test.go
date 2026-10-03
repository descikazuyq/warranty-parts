package warranty

import (
	"errors"
	"math"
	"testing"
	"time"
)

// 长保修期限：截止时刻按购买时刻起完整的天数个二十四小时计算，
// 不因 time.Duration 的纳秒上限溢出而落到购买时刻之前。
func TestLongWarrantyExpiry(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	expiries := make(map[int64]int)
	for _, days := range []int{106751, 106752, 200000} {
		s := NewStore()
		if err := s.RegisterProduct("p1", purchase, days, nil); err != nil {
			t.Fatalf("register %d days: %v", days, err)
		}
		if err := s.RegisterPart("part1", 10); err != nil {
			t.Fatalf("register part: %v", err)
		}
		if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
			t.Fatalf("submit request: %v", err)
		}

		// 购买当天合格，截止时刻在购买时刻之后完整天数个二十四小时。
		e, err := s.Evaluate("r1", purchase)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if !e.Eligible || len(e.Reasons) != 0 {
			t.Fatalf("days=%d: expected eligible on purchase day, got %+v", days, e)
		}
		if got := e.WarrantyExpiry.Unix() - purchase.Unix(); got != int64(days)*86400 {
			t.Fatalf("days=%d: expiry offset = %ds, want %ds", days, got, int64(days)*86400)
		}
		// 秒以下精度保留。
		if e.WarrantyExpiry.Nanosecond() != purchase.Nanosecond() {
			t.Fatalf("days=%d: sub-second precision lost: %v", days, e.WarrantyExpiry)
		}
		expiries[e.WarrantyExpiry.Unix()] = days

		// 截止时刻之前仍可预留。
		if _, err := s.Reserve("c1", "r1", "part1", 1, e.WarrantyExpiry.Add(day), e.WarrantyExpiry.Add(-time.Second)); err != nil {
			t.Fatalf("days=%d: reserve before expiry: %v", days, err)
		}
		// 截止时刻及之后返回不合格，不创建承诺。
		if _, err := s.Reserve("c2", "r1", "part1", 1, e.WarrantyExpiry.Add(2*day), e.WarrantyExpiry); !errors.Is(err, ErrIneligible) {
			t.Fatalf("days=%d: reserve at expiry = %v, want ErrIneligible", days, err)
		}
		if _, err := s.Commitment("c2"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("days=%d: rejected reserve must not create commitment: %v", days, err)
		}
		// 历史中的资格依据显示正确的购买时刻、原保修天数和截止时刻。
		hist, err := s.RequestHistory("r1")
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		last := hist[len(hist)-1]
		if last.Eligibility == nil || !last.Eligibility.PurchaseTime.Equal(purchase) ||
			last.Eligibility.WarrantyDays != days || !last.Eligibility.WarrantyExpiry.Equal(e.WarrantyExpiry) {
			t.Fatalf("days=%d: history eligibility basis wrong: %+v", days, last.Eligibility)
		}
	}
	// 不同期限各自得到不同的截止时刻，不共用某个上限。
	if len(expiries) != 3 {
		t.Fatalf("expiries not distinct: %v", expiries)
	}
}

// 期限对应的截止时刻超出现有时间类型的可表示范围时，登记返回
// ErrInvalidParam 且不保存产品记录；同一编号随后可正常登记。
func TestRegisterUnrepresentableWarranty(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, math.MaxInt64/2, nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("unrepresentable warranty: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Product("p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("product must not be saved: %v", err)
	}
	if err := s.RegisterProduct("p1", t0, 30, nil); err != nil {
		t.Fatalf("re-register with representable term: %v", err)
	}
}
