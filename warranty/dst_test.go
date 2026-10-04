package warranty

// 引入 time/tzdata 让测试二进制自带时区数据库：夏令时切换规则不依赖运行
// 机器是否安装 /usr/share/zoneinfo，回归保障在任何环境都真正生效。
import _ "time/tzdata"

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// nyc 返回美国纽约时区（2026 年春进为 3 月 8 日、秋退为 11 月 1 日）。
func nyc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	return loc
}

// dstCase 描述一次跨越夏令时切换的一天期保修。
type dstCase struct {
	name        string
	purchase    time.Time
	purchaseOff int // 购买时刻的 UTC 偏移（秒）
	expMonth    time.Month
	expDay      int
	expHour     int
	expMinute   int
	expOff      int // 保修截止时刻的 UTC 偏移（秒）
	expZone     string
	// 次日钟面陷阱时刻：春进时次日十二点（早于十三点截止）仍应合格；
	// 秋退时刻度尚未回到购买时的十二点（如十一点半）也应已过保。
	clockTrap     time.Time
	clockTrapElig bool
}

func dstCases(t *testing.T) []dstCase {
	loc := nyc(t)
	return []dstCase{
		{
			name:          "spring_forward",
			purchase:      time.Date(2026, time.March, 7, 12, 0, 0, 123456789, loc),
			purchaseOff:   -5 * 3600, // EST
			expMonth:      time.March,
			expDay:        8,
			expHour:       13, // 当地只有 23 小时：钟面截止时刻被推到十三点
			expMinute:     0,
			expOff:        -4 * 3600, // EDT
			expZone:       "EDT",
			clockTrap:     time.Date(2026, time.March, 8, 12, 0, 0, 0, loc),
			clockTrapElig: true, // 次日十二点距离购买只过了 23 小时，不能提前认定过保
		},
		{
			name:          "fall_back",
			purchase:      time.Date(2026, time.October, 31, 12, 0, 0, 123456789, loc),
			purchaseOff:   -4 * 3600, // EDT
			expMonth:      time.November,
			expDay:        1,
			expHour:       11, // 当地有 25 小时：钟面截止时刻提前到十一点
			expMinute:     0,
			expOff:        -5 * 3600, // EST
			expZone:       "EST",
			clockTrap:     time.Date(2026, time.November, 1, 11, 30, 0, 0, loc),
			clockTrapElig: false, // 钟面尚未回到购买时的十二点，但实际已满 24 小时
		},
	}
}

// 注册一个一天期、故障 NOISE 不在除外清单的产品及请求，返回保修截止时刻。
func registerOneDayWarranty(t *testing.T, s *Store, purchase time.Time) time.Time {
	t.Helper()
	if err := s.RegisterProduct("p", purchase, 1, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	e, err := s.Evaluate("r", purchase)
	if err != nil {
		t.Fatalf("evaluate at purchase: %v", err)
	}
	if !e.Eligible {
		t.Fatalf("purchase instant must be eligible, reasons %v", e.Reasons)
	}
	return e.WarrantyExpiry
}

// TestWarrantyDSTOneDayAlwaysTwentyFourHours 覆盖时钟向前和向后调整两种
// 购买条件：保修一天的截止时刻都是购买时刻之后完整二十四小时的实际时刻，
// 不因为当地一天只有二十三小时或二十五小时而提前或推后；截止时刻保留购买
// 时刻的时区表示与小数秒精度，不取整到整秒、整点或当天零点。
func TestWarrantyDSTOneDayAlwaysTwentyFourHours(t *testing.T) {
	for _, tc := range dstCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			expiry := registerOneDayWarranty(t, s, tc.purchase)

			if name, off := tc.purchase.Zone(); off != tc.purchaseOff {
				t.Fatalf("purchase offset = %s(%d), want %d", name, off, tc.purchaseOff)
			}
			// 实际跨度必须正好二十四小时：Unix 秒差 86400，纳秒差也为 0。
			if got := expiry.Sub(tc.purchase); got != 24*time.Hour {
				t.Fatalf("actual warranty length = %v, want 24h", got)
			}
			if got := expiry.Unix() - tc.purchase.Unix(); got != secondsPerDay {
				t.Fatalf("actual warranty length = %d seconds, want %d", got, secondsPerDay)
			}

			loc := nyc(t)
			got := expiry.In(loc)
			if got.Year() != 2026 || got.Month() != tc.expMonth || got.Day() != tc.expDay ||
				got.Hour() != tc.expHour || got.Minute() != tc.expMinute || got.Second() != 0 {
				t.Fatalf("expiry clock = %v, want 2026-%02d-%02d %02d:%02d:00",
					got.Format("2006-01-02 15:04:05"), tc.expMonth, tc.expDay, tc.expHour, tc.expMinute)
			}
			if name, off := got.Zone(); name != tc.expZone || off != tc.expOff {
				t.Fatalf("expiry zone = %s(%d), want %s(%d)", name, off, tc.expZone, tc.expOff)
			}
			// 小数秒原样保留在截止时刻。
			if got.Nanosecond() != tc.purchase.Nanosecond() || got.Nanosecond() != 123456789 {
				t.Fatalf("sub-second precision lost: expiry nanos = %d, purchase = %d",
					got.Nanosecond(), tc.purchase.Nanosecond())
			}

			e, err := s.Evaluate("r", tc.purchase)
			if err != nil {
				t.Fatal(err)
			}
			if !e.PurchaseTime.Equal(tc.purchase) || e.WarrantyDays != 1 {
				t.Fatalf("basis = (%v, %d days), want (%v, 1)", e.PurchaseTime, e.WarrantyDays, tc.purchase)
			}
		})
	}
}

// alternativeZones 返回同一实际时刻的不同文字表示：纽约本地、协调世界时，
// 以及日期/小时完全不同的两个固定偏移（秋退截止时刻 16:00Z 在 UTC+8 已是
// 次日零点）。结论不得因表示不同而改变。
func alternativeZones(t *testing.T) []*time.Location {
	return []*time.Location{
		nyc(t),
		time.UTC,
		time.FixedZone("UTC+8", 8*3600),
		time.FixedZone("UTC-8", -8*3600),
	}
}

func reasonsEqual(a, b []RejectionReason) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestWarrantyDSTEligibilityBoundaryAndZoneRepresentation 验证两种切换下：
// 截止前一纳秒仍合格；恰好到达截止时刻及之后过保，拒绝原因明确只有保修已
// 到期；同一查询时刻换协调世界时或其他时区表示，资格结果与实际截止时刻一致。
func TestWarrantyDSTEligibilityBoundaryAndZoneRepresentation(t *testing.T) {
	for _, tc := range dstCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			expiry := registerOneDayWarranty(t, s, tc.purchase)

			before := expiry.Add(-time.Nanosecond)
			at := expiry
			after := expiry.Add(time.Nanosecond)

			for _, z := range alternativeZones(t) {
				t.Run(z.String(), func(t *testing.T) {
					// 截止前一纳秒：合格，无拒绝原因。
					e, err := s.Evaluate("r", before.In(z))
					if err != nil {
						t.Fatalf("evaluate 1ns before: %v", err)
					}
					if !e.Eligible || len(e.Reasons) != 0 {
						t.Fatalf("1ns before expiry: %+v", e)
					}
					if !e.WarrantyExpiry.Equal(expiry) {
						t.Fatalf("expiry instant %v drifted with query zone %s", e.WarrantyExpiry, z)
					}

					// 恰好到达截止时刻：过保，原因明确为保修已到期。
					eAt, err := s.Evaluate("r", at.In(z))
					if err != nil {
						t.Fatalf("evaluate at expiry: %v", err)
					}
					if eAt.Eligible || !reasonsEqual(eAt.Reasons, []RejectionReason{ReasonWarrantyExpired}) {
						t.Fatalf("at expiry: %+v", eAt)
					}
					if !eAt.WarrantyExpiry.Equal(expiry) {
						t.Fatalf("expiry instant %v drifted with query zone %s", eAt.WarrantyExpiry, z)
					}

					// 截止之后一纳秒：同样过保。
					eAfter, err := s.Evaluate("r", after.In(z))
					if err != nil {
						t.Fatalf("evaluate after expiry: %v", err)
					}
					if eAfter.Eligible || !reasonsEqual(eAfter.Reasons, []RejectionReason{ReasonWarrantyExpired}) {
						t.Fatalf("after expiry: %+v", eAfter)
					}
					if !eAfter.WarrantyExpiry.Equal(eAt.WarrantyExpiry) {
						t.Fatalf("expiry instants differ across query times/zones")
					}
				})
			}

			// 钟面陷阱：按当地钟面小时而非实际经过时间判断就会得出错误结论。
			e, err := s.Evaluate("r", tc.clockTrap)
			if err != nil {
				t.Fatal(err)
			}
			if e.Eligible != tc.clockTrapElig {
				t.Fatalf("clock-face trap at %v: eligible=%v, want %v, reasons %v",
					tc.clockTrap, e.Eligible, tc.clockTrapElig, e.Reasons)
			}
			if tc.clockTrapElig {
				if len(e.Reasons) != 0 {
					t.Fatalf("spring-forward next noon must be eligible, reasons %v", e.Reasons)
				}
			} else if !containsReason(e.Reasons, ReasonWarrantyExpired) {
				t.Fatalf("fall-back 11:30 wall clock must be expired, reasons %v", e.Reasons)
			}
		})
	}
}

// TestWarrantyDSTReserveBoundary 验证跨夏令时的时间规则同样约束首次预留：
// 库存充足且承诺自身到期时刻晚于保修截止时刻时，截止前的新承诺成功；到达
// 保修截止时刻后（无论用哪个时区表示该时刻）的新承诺一律返回现有不合格
// 错误，不创建承诺、不增加占用。此前成功的承诺只按它自己的到期时刻有效，
// 产品刚过保不会取消它或释放其未用数量；按请求查看资格与直接查询一致，
// 能看到准确的截止时刻、过保原因和成功承诺明细。
func TestWarrantyDSTReserveBoundary(t *testing.T) {
	for _, tc := range dstCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			expiry := registerOneDayWarranty(t, s, tc.purchase)
			if err := s.RegisterPart("sp", 5); err != nil {
				t.Fatal(err)
			}
			// 承诺自身到期时刻晚于保修截止时刻，两者各自独立。
			commitExpiry := expiry.Add(2 * time.Hour)

			// 保修截止前一纳秒：新承诺成功。
			c1, err := s.Reserve("c1", "r", "sp", 2, commitExpiry, expiry.Add(-time.Nanosecond))
			if err != nil {
				t.Fatalf("reserve before warranty expiry: %v", err)
			}
			if c1.Quantity != 2 || c1.Used != 0 || c1.Canceled || c1.Expired ||
				!c1.Expiry.Equal(commitExpiry) {
				t.Fatalf("unexpected commitment: %+v", c1)
			}

			// 到达保修截止时刻：同一实际时刻换纽约、协调世界时和 UTC+8
			// （秋退时已是另一天的零点）三种表示，结论必须一致。
			for i, z := range []*time.Location{nyc(t), time.UTC, time.FixedZone("UTC+8", 8*3600)} {
				failID := fmt.Sprintf("c-fail-%d", i)
				_, err := s.Reserve(failID, "r", "sp", 1, commitExpiry.In(z), expiry.In(z))
				if !errors.Is(err, ErrIneligible) {
					t.Fatalf("reserve at warranty expiry in %s: got %v, want ErrIneligible", z, err)
				}
				if _, err := s.Commitment(failID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("rejected reserve %s must not create a commitment, got %v", failID, err)
				}
			}

			// 全部失败提交都不增加占用：仍只有 c1 的 2 件。
			st, err := s.PartStatus("sp", expiry)
			if err != nil {
				t.Fatal(err)
			}
			if st.PhysicalRemaining != 5 || st.ActiveOccupied != 2 || st.Committable != 3 {
				t.Fatalf("stock after rejected reserves: %+v", st)
			}
			if len(st.Details) != 1 || st.Details[0].CommitmentID != "c1" {
				t.Fatalf("only the successful commitment may appear, got %+v", st.Details)
			}

			// 产品刚过保不取消 c1，也不释放其未用数量：在保修截止后、
			// c1 自身到期前仍可按它自己的到期时刻使用。
			afterWarranty := expiry.Add(10 * time.Minute)
			u, err := s.Use("u1", "c1", 1, afterWarranty)
			if err != nil {
				t.Fatalf("successful commitment stays usable after warranty expiry: %v", err)
			}
			if u.Quantity != 1 {
				t.Fatalf("usage = %+v", u)
			}
			st, err = s.PartStatus("sp", afterWarranty)
			if err != nil {
				t.Fatal(err)
			}
			if st.PhysicalRemaining != 4 || st.ActiveOccupied != 1 || st.Committable != 3 {
				t.Fatalf("stock after use: %+v", st)
			}

			// 按请求查看资格与直接资格查询一致，并保留成功承诺明细。
			view, err := s.RequestView("r", afterWarranty)
			if err != nil {
				t.Fatal(err)
			}
			direct, err := s.Evaluate("r", afterWarranty)
			if err != nil {
				t.Fatal(err)
			}
			if view.Eligibility == nil {
				t.Fatal("request view missing eligibility")
			}
			if view.Eligibility.Eligible ||
				!reasonsEqual(view.Eligibility.Reasons, []RejectionReason{ReasonWarrantyExpired}) {
				t.Fatalf("view eligibility: %+v", view.Eligibility)
			}
			if !view.Eligibility.WarrantyExpiry.Equal(direct.WarrantyExpiry) ||
				!view.Eligibility.WarrantyExpiry.Equal(expiry) ||
				view.Eligibility.WarrantyDays != 1 ||
				!view.Eligibility.PurchaseTime.Equal(tc.purchase) {
				t.Fatalf("view eligibility basis differs from direct query: %+v vs %+v",
					view.Eligibility, direct)
			}
			if local := view.Eligibility.WarrantyExpiry.In(nyc(t)); local.Hour() != tc.expHour ||
				local.Minute() != tc.expMinute || local.Nanosecond() != 123456789 {
				t.Fatalf("view expiry not the precise local instant: %v", local)
			}
			if len(view.Commitments) != 1 {
				t.Fatalf("want only c1 detail, got %+v", view.Commitments)
			}
			d := view.Commitments[0]
			if d.CommitmentID != "c1" || d.OriginalQuantity != 2 || d.UsedQuantity != 1 ||
				d.RemainingQuantity != 1 || d.Status != CommitmentActive || !d.Expiry.Equal(commitExpiry) {
				t.Fatalf("successful commitment detail not preserved: %+v", d)
			}

			// 截止前查看仍合格，且同样保留该承诺明细。
			viewBefore, err := s.RequestView("r", expiry.Add(-time.Nanosecond))
			if err != nil {
				t.Fatal(err)
			}
			if !viewBefore.Eligibility.Eligible || len(viewBefore.Commitments) != 1 {
				t.Fatalf("view before expiry: elig=%+v commitments=%+v",
					viewBefore.Eligibility, viewBefore.Commitments)
			}

			// c1 只按它自己的到期时刻有效：到达其自身到期时刻时新使用关闭，
			// 与保修截止时刻无关。
			if _, err := s.Use("u2", "c1", 1, commitExpiry); !errors.Is(err, ErrCommitmentClosed) {
				t.Fatalf("use at commitment own expiry: got %v, want ErrCommitmentClosed", err)
			}

			// 失败提交的历史记录保留准确资格依据快照：购买时刻、一天期限、
			// 跨夏令时的截止时刻和保修已到期原因。
			hist, err := s.RequestHistory("r")
			if err != nil {
				t.Fatal(err)
			}
			if len(hist) != 4 || !hist[0].Success {
				t.Fatalf("history = %+v", hist)
			}
			last := hist[len(hist)-1]
			if last.Success || last.Error != HistoryErrorIneligible || last.Eligibility == nil {
				t.Fatalf("last history record = %+v, want ineligible failure with basis", last)
			}
			basis := last.Eligibility
			if !basis.WarrantyExpiry.Equal(expiry) || basis.WarrantyDays != 1 ||
				!basis.PurchaseTime.Equal(tc.purchase) ||
				!containsReason(basis.Reasons, ReasonWarrantyExpired) {
				t.Fatalf("failure eligibility basis = %+v", basis)
			}
		})
	}
}
