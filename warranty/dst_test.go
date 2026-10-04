package warranty

import (
	"errors"
	"testing"
	"time"

	// 嵌入 IANA 时区数据库，使 America/New_York 在任何运行环境（包括没有
	// 系统 tzdata 的精简镜像）都能稳定加载，跨夏令时测试不依赖宿主配置。
	_ "time/tzdata"
)

// dstLocPkg 是包级共享的纽约时区；2026 年该时区在 3 月 8 日 2:00 拨快到 3:00
// （春令时，当地一天只有 23 小时），在 11 月 1 日 2:00 拨回 1:00（秋令时，
// 当地一天有 25 小时）。tzdata 已嵌入，加载不会失败，进程内只加载一次。
var dstLocPkg = mustLoadNY()

func mustLoadNY() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}

// 跨夏令时购买的两个条件：春令时与秋令时。保修一天的截止时刻都必须是购买
// 时刻之后恰好二十四小时的实际时刻，而不是次日同一钟点。
var dstPurchaseCases = []struct {
	name          string
	purchase      time.Time
	wantClock     string // 截止时刻在纽约当地的钟面（RFC3339 不含小数秒）
	wantOffsetSec int    // 截止时刻在纽约相对 UTC 的偏移（秒）
}{
	{
		name:          "spring_forward",
		purchase:      time.Date(2026, 3, 7, 12, 0, 0, 0, dstLocPkg),
		wantClock:     "2026-03-08T13:00:00-04:00",
		wantOffsetSec: -4 * 3600,
	},
	{
		name:          "fall_back",
		purchase:      time.Date(2026, 10, 31, 12, 0, 0, 0, dstLocPkg),
		wantClock:     "2026-11-01T11:00:00-05:00",
		wantOffsetSec: -5 * 3600,
	},
}

// TestWarrantyOneDayAcrossDSTDeadlines 锁定两个跨夏令时条件的截止时刻：
// 春令时购买（2026-03-07 12:00 EST，保修一天）截止于 03-08 13:00 EDT；
// 秋令时购买（2026-10-31 12:00 EDT，保修一天）截止于 11-01 11:00 EST。
// 两种情况下实际保修长度都必须正好是二十四小时，截止时刻保留购买时刻的时区
// 表示（资格依据仍含购买时刻、登记的保修天数与该时区下的截止时刻）。
func TestWarrantyOneDayAcrossDSTDeadlines(t *testing.T) {
	for _, tc := range dstPurchaseCases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			if err := s.RegisterProduct("p", tc.purchase, 1, nil); err != nil {
				t.Fatalf("register product: %v", err)
			}
			if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
				t.Fatalf("submit request: %v", err)
			}
			// 直接核对截止时刻计算。
			got, ok := warrantyExpiry(tc.purchase, 1)
			if !ok {
				t.Fatal("warrantyExpiry not ok")
			}
			if got.Format(time.RFC3339) != tc.wantClock {
				t.Fatalf("deadline clock = %q, want %q", got.Format(time.RFC3339), tc.wantClock)
			}
			if _, off := got.Zone(); off != tc.wantOffsetSec {
				t.Fatalf("deadline zone offset = %d sec, want %d", off, tc.wantOffsetSec)
			}
			// 实际长度正好二十四小时，不随当地 23/25 小时伸缩。
			if span := got.Sub(tc.purchase); span != 24*time.Hour {
				t.Fatalf("actual warranty length = %v, want 24h", span)
			}
			if got.Unix()-tc.purchase.Unix() != 24*60*60 {
				t.Fatalf("actual warranty length = %d unix seconds, want 86400",
					got.Unix()-tc.purchase.Unix())
			}

			// 资格依据保留购买时刻、登记天数与该时区下的截止时刻。
			e, err := s.Evaluate("r", tc.purchase)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if !e.PurchaseTime.Equal(tc.purchase) || e.WarrantyDays != 1 {
				t.Fatalf("basis purchase/days = %v/%d, want %v/1",
					e.PurchaseTime, e.WarrantyDays, tc.purchase)
			}
			if !e.WarrantyExpiry.Equal(got) || e.WarrantyExpiry.Format(time.RFC3339) != tc.wantClock {
				t.Fatalf("basis deadline = %v, want %v", e.WarrantyExpiry, got)
			}
			if e.WarrantyExpiry.Location() != tc.purchase.Location() {
				t.Fatalf("deadline zone = %v, want purchase zone %v",
					e.WarrantyExpiry.Location(), tc.purchase.Location())
			}
		})
	}
}

// TestWarrantyAcrossDSTBoundaryNanosecond 对两个跨夏令时条件锁定过保边界：
// 截止前一纳秒仍合格；恰好到达截止时刻及之后过保，拒绝原因明确为
// warranty_expired。钟面是否已回到购买时的 12:00 不作为判断依据——实际经过
// 的时间才是。
func TestWarrantyAcrossDSTBoundaryNanosecond(t *testing.T) {
	for _, tc := range dstPurchaseCases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			if err := s.RegisterProduct("p", tc.purchase, 1, nil); err != nil {
				t.Fatalf("register product: %v", err)
			}
			if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
				t.Fatalf("submit request: %v", err)
			}
			e0, err := s.Evaluate("r", tc.purchase)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			deadline := e0.WarrantyExpiry

			// 截止前一纳秒：仍合格。
			if e, err := s.Evaluate("r", deadline.Add(-time.Nanosecond)); err != nil || !e.Eligible {
				t.Fatalf("one ns before deadline: eligible=%v err=%v reasons=%v",
					e != nil && e.Eligible, err, e)
			}
			// 恰好到达截止时刻：过保，原因明确为保修已到期。
			eAt, err := s.Evaluate("r", deadline)
			if err != nil {
				t.Fatalf("evaluate at deadline: %v", err)
			}
			if eAt.Eligible || len(eAt.Reasons) != 1 || eAt.Reasons[0] != ReasonWarrantyExpired {
				t.Fatalf("at deadline: %+v, want single warranty_expired", eAt)
			}
			// 截止后一纳秒：同样过保。
			if e, _ := s.Evaluate("r", deadline.Add(time.Nanosecond)); e.Eligible ||
				len(e.Reasons) != 1 || e.Reasons[0] != ReasonWarrantyExpired {
				t.Fatalf("one ns after deadline: %+v", e)
			}

			// 钟面陷阱：秋令时次日 11:00 起即过保，不能因为当地钟面尚未回到
			// 购买时的 12:00 就继续认定在保；春令时次日 12:00（比截止的当地
			// 13:00 早一小时）仍在保，不能提前认定过保。
			ny := tc.purchase.Location()
			nextNoon := time.Date(tc.purchase.Year(), tc.purchase.Month(),
				tc.purchase.Day()+1, 12, 0, 0, 0, ny)
			eNoon, _ := s.Evaluate("r", nextNoon)
			switch tc.name {
			case "fall_back":
				// 11:00 已截止，12:00 自然已过保。
				if eNoon.Eligible {
					t.Fatalf("fall-back next noon %v still eligible before clock returns to 12:00", nextNoon)
				}
				// 更尖锐的陷阱：次日 11:30 当地钟面远未到 12:00，但已过保。
				elevenThirty := time.Date(2026, 11, 1, 11, 30, 0, 0, ny)
				if e, _ := s.Evaluate("r", elevenThirty); e.Eligible {
					t.Fatalf("fall-back 11:30 local (before purchase clock 12:00) must be expired: %+v", e)
				}
			case "spring_forward":
				// 当地 12:00 < 截止的当地 13:00，仍在保。
				if !eNoon.Eligible {
					t.Fatalf("spring-forward next noon %v wrongly expired an hour early: %+v", nextNoon, eNoon)
				}
			}
		})
	}
}

// TestWarrantyAcrossDSTInvariantUnderZoneRepresentation 验证把同一查询时刻改用
// 协调世界时或其他时区表示，资格结果与实际截止时刻必须一致，不随日期、小时或
// 时区偏移的文字表示改变。
func TestWarrantyAcrossDSTInvariantUnderZoneRepresentation(t *testing.T) {
	ny := dstLocPkg
	plus8 := time.FixedZone("UTC+8", 8*60*60)
	for _, tc := range dstPurchaseCases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			if err := s.RegisterProduct("p", tc.purchase, 1, nil); err != nil {
				t.Fatalf("register product: %v", err)
			}
			if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
				t.Fatalf("submit request: %v", err)
			}
			base, err := s.Evaluate("r", tc.purchase)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			deadline := base.WarrantyExpiry

			// 同一实际时刻分别用纽约、UTC、固定 +8 表示：截止前一纳秒与截止
			// 时刻各查一遍，结论必须一致。
			probe := func(inst time.Time, wantEligible bool) {
				t.Helper()
				for name, rep := range map[string]time.Time{
					"ny":    inst.In(ny),
					"utc":   inst.UTC(),
					"plus8": inst.In(plus8),
					"local": inst.In(time.Local),
				} {
					e, err := s.Evaluate("r", rep)
					if err != nil {
						t.Fatalf("%s representation evaluate: %v", name, err)
					}
					if e.Eligible != wantEligible {
						t.Fatalf("%s representation of %v: eligible=%v, want %v (reasons %v)",
							name, inst, e.Eligible, wantEligible, e.Reasons)
					}
					if !e.WarrantyExpiry.Equal(deadline) {
						t.Fatalf("%s representation changed deadline: %v, want %v",
							name, e.WarrantyExpiry, deadline)
					}
					if e.WarrantyExpiry.Location() != ny {
						t.Fatalf("%s representation changed deadline zone: %v",
							name, e.WarrantyExpiry.Location())
					}
				}
			}
			probe(deadline.Add(-time.Nanosecond), true)
			probe(deadline, false)

			// 截止时刻本身换时区表示后仍是同一实际时刻（文字差异显著）。
			if deadline.UTC().Format(time.RFC3339) == deadline.Format(time.RFC3339) {
				t.Fatalf("test setup: UTC representation did not differ: %s", deadline.Format(time.RFC3339))
			}
			if !deadline.UTC().Equal(deadline) {
				t.Fatalf("UTC representation is not the same instant")
			}
		})
	}
}

// TestWarrantyAcrossDSTSubSecondPrecision 验证购买时刻带有小数秒时，这部分精度
// 继续体现在跨夏令时的截止时刻上，而不是被取整到整秒、整点或当天零点；边界也
// 按该亚秒时刻判定。
func TestWarrantyAcrossDSTSubSecondPrecision(t *testing.T) {
	ny := dstLocPkg
	const nano = 123456789
	purchase := time.Date(2026, 3, 7, 12, 30, 45, nano, ny) // 春令时切换前一天。
	s := NewStore()
	if err := s.RegisterProduct("p", purchase, 1, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	e, err := s.Evaluate("r", purchase)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	got := e.WarrantyExpiry
	if got.Nanosecond() != nano {
		t.Fatalf("sub-second precision lost: deadline ns = %d, want %d (%v)",
			got.Nanosecond(), nano, got)
	}
	// 截止时刻应保留 12:30:45.123456789 的钟面加二十四小时（春令时当地为
	// 次日 13:30:45.123456789 EDT），不得对齐整点或零点。
	if clock := got.Format("15:04:05.000000000"); clock != "13:30:45.123456789" {
		t.Fatalf("deadline clock = %q, want 13:30:45.123456789", clock)
	}
	if got.Hour() == 0 && got.Minute() == 0 && got.Second() == 0 && got.Nanosecond() == 0 {
		t.Fatalf("deadline snapped to local midnight: %v", got)
	}
	if span := got.UnixNano() - purchase.UnixNano(); span != int64(24*time.Hour) {
		t.Fatalf("actual length = %d ns, want 24h", span)
	}
	// 按亚秒边界判定：整秒后的同一秒内（.000000000）仍可能在保或过保，
	// 不能把截止时刻取整到整秒。
	if before, err := s.Evaluate("r", got.Add(-time.Nanosecond)); err != nil || !before.Eligible {
		t.Fatalf("before sub-second deadline: %+v err=%v", before, err)
	}
	if after, _ := s.Evaluate("r", got); after.Eligible {
		t.Fatalf("at sub-second deadline still eligible: %+v", after)
	}

	// 秋令时方向同样保留亚秒精度（10-31 12:30:45.123456789 EDT 起一天，
	// 实际 24 小时后为 11-01 11:30:45.123456789 EST）。
	purchase2 := time.Date(2026, 10, 31, 12, 30, 45, nano, ny)
	s2 := NewStore()
	if err := s2.RegisterProduct("p2", purchase2, 1, nil); err != nil {
		t.Fatalf("register product p2: %v", err)
	}
	if err := s2.SubmitRequest("r2", "p2", "NOISE"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}
	e2, _ := s2.Evaluate("r2", purchase2)
	if e2.WarrantyExpiry.Nanosecond() != nano {
		t.Fatalf("fall-back sub-second precision lost: %v", e2.WarrantyExpiry)
	}
	if clock := e2.WarrantyExpiry.Format("15:04:05.000000000"); clock != "11:30:45.123456789" {
		t.Fatalf("fall-back deadline clock = %q, want 11:30:45.123456789", clock)
	}
	if e2.WarrantyExpiry.UnixNano()-purchase2.UnixNano() != int64(24*time.Hour) {
		t.Fatalf("fall-back actual length != 24h")
	}
}

// TestReserveAcrossDSTWarrantyBoundary 验证跨夏令时的保修时间规则体现在首次
// 预留备件上：库存充足、承诺自身到期时刻晚于保修截止时刻时，截止前提交的新
// 承诺成功；到达保修截止时刻后提交另一笔新承诺，返回现有的不合格错误
// （ErrIneligible），不创建承诺、不增加库存占用。
func TestReserveAcrossDSTWarrantyBoundary(t *testing.T) {
	for _, tc := range dstPurchaseCases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			if err := s.RegisterProduct("p", tc.purchase, 1, nil); err != nil {
				t.Fatalf("register product: %v", err)
			}
			if err := s.RegisterPart("sp", 10); err != nil {
				t.Fatalf("register part: %v", err)
			}
			if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
				t.Fatalf("submit request: %v", err)
			}
			deadline, _ := warrantyExpiry(tc.purchase, 1)
			// 承诺自身的到期时刻晚于保修截止时刻。
			commitExpiry := deadline.Add(48 * time.Hour)

			// 截止前提交新承诺：成功并占用一件。
			c1, err := s.Reserve("c-before", "r", "sp", 1, commitExpiry, deadline.Add(-time.Nanosecond))
			if err != nil {
				t.Fatalf("reserve one ns before warranty deadline: %v", err)
			}
			if !c1.Expiry.Equal(commitExpiry) {
				t.Fatalf("commitment expiry = %v, want %v", c1.Expiry, commitExpiry)
			}

			// 到达保修截止时刻提交另一笔新承诺：不合格错误，不创建、不占用。
			_, err = s.Reserve("c-after", "r", "sp", 1, commitExpiry, deadline)
			if !errors.Is(err, ErrIneligible) {
				t.Fatalf("reserve at warranty deadline: got %v, want ErrIneligible", err)
			}
			if _, err := s.Commitment("c-after"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rejected reserve created a commitment: %v", err)
			}
			st, err := s.PartStatus("sp", deadline)
			if err != nil {
				t.Fatalf("part status: %v", err)
			}
			if st.PhysicalRemaining != 10 || st.ActiveOccupied != 1 || st.Committable != 9 {
				t.Fatalf("stock after rejected reserve: phys=%d occupied=%d committable=%d, want 10/1/9",
					st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
			}
			if len(st.Details) != 1 || st.Details[0].CommitmentID != "c-before" {
				t.Fatalf("details = %+v, want only c-before", st.Details)
			}

			// 截止后一纳秒再提交一笔：同样被拒，不改变库存。
			if _, err := s.Reserve("c-later", "r", "sp", 1, commitExpiry, deadline.Add(time.Nanosecond)); !errors.Is(err, ErrIneligible) {
				t.Fatalf("reserve after deadline: got %v, want ErrIneligible", err)
			}
			st2, _ := s.PartStatus("sp", deadline.Add(time.Nanosecond))
			if st2.ActiveOccupied != 1 || st2.Committable != 9 {
				t.Fatalf("second rejected reserve changed stock: %+v", st2)
			}
		})
	}
}

// TestReserveAcrossDSTPriorCommitmentSurvivesWarrantyExpiry 验证产品刚刚过保不会
// 取消此前成功的承诺或释放其未用数量：该承诺仍按它自己的到期时刻有效（active），
// 其未用数量仍被占用；只是新承诺因保修已到期无法再建。
func TestReserveAcrossDSTPriorCommitmentSurvivesWarrantyExpiry(t *testing.T) {
	// 秋令时条件最易踩钟面陷阱，两个方向都核对一遍。
	for _, tc := range dstPurchaseCases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			if err := s.RegisterProduct("p", tc.purchase, 1, nil); err != nil {
				t.Fatalf("register product: %v", err)
			}
			if err := s.RegisterPart("sp", 10); err != nil {
				t.Fatalf("register part: %v", err)
			}
			if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
				t.Fatalf("submit request: %v", err)
			}
			deadline, _ := warrantyExpiry(tc.purchase, 1)
			// 承诺自身到期时刻明显晚于保修截止：保修截止后一天它仍未到期。
			commitExpiry := deadline.Add(24 * time.Hour)

			if _, err := s.Reserve("c1", "r", "sp", 3, commitExpiry, deadline.Add(-time.Hour)); err != nil {
				t.Fatalf("reserve before deadline: %v", err)
			}
			// 产品刚过保：新承诺被拒。
			if _, err := s.Reserve("c2", "r", "sp", 1, commitExpiry, deadline); !errors.Is(err, ErrIneligible) {
				t.Fatalf("reserve after warranty expiry: got %v, want ErrIneligible", err)
			}

			// 保修截止时刻、且仍早于承诺自身到期时刻查询：c1 仍 active，
			// 三件未用数量继续占用，不被产品过保取消或释放。
			st, err := s.PartStatus("sp", deadline)
			if err != nil {
				t.Fatalf("part status: %v", err)
			}
			if st.PhysicalRemaining != 10 || st.ActiveOccupied != 3 || st.Committable != 7 {
				t.Fatalf("stock at warranty deadline: phys=%d occupied=%d committable=%d, want 10/3/7",
					st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
			}
			d, ok := detailByID(st, "c1")
			if !ok {
				t.Fatal("c1 missing from part details")
			}
			if d.Status != CommitmentActive || d.OriginalQuantity != 3 ||
				d.UsedQuantity != 0 || d.RemainingQuantity != 3 {
				t.Fatalf("c1 after warranty expiry: %+v, want active qty 3 unused 3", d)
			}
			if !d.Expiry.Equal(commitExpiry) {
				t.Fatalf("c1 expiry = %v, want %v", d.Expiry, commitExpiry)
			}

			// 过保后该承诺仍可正常使用（它按自己的到期时刻有效）。
			if _, err := s.Use("u1", "c1", 1, deadline.Add(time.Minute)); err != nil {
				t.Fatalf("use prior commitment after warranty expiry: %v", err)
			}
			st2, _ := s.PartStatus("sp", deadline.Add(time.Minute))
			if st2.PhysicalRemaining != 9 || st2.ActiveOccupied != 2 {
				t.Fatalf("stock after use: phys=%d occupied=%d, want 9/2",
					st2.PhysicalRemaining, st2.ActiveOccupied)
			}

			// 直到承诺自身到期时刻确认后，余量才释放；产品过保与此无关。
			st3, _ := s.PartStatus("sp", commitExpiry)
			d3, _ := detailByID(st3, "c1")
			if d3.Status != CommitmentExpired || d3.UsedQuantity != 1 || d3.RemainingQuantity != 2 {
				t.Fatalf("c1 at own expiry: %+v, want expired used=1 remaining=2", d3)
			}
			if st3.PhysicalRemaining != 9 || st3.ActiveOccupied != 0 || st3.Committable != 9 {
				t.Fatalf("stock at own expiry: phys=%d occupied=%d committable=%d, want 9/0/9",
					st3.PhysicalRemaining, st3.ActiveOccupied, st3.Committable)
			}
		})
	}
}

// TestRequestViewAcrossDSTMatchesEvaluate 验证按请求查看资格与直接资格查询一致：
// 能看见准确的跨夏令时截止时刻和过保原因，并保留此前成功的承诺明细；查询时刻
// 的时区表示不改变结论。
func TestRequestViewAcrossDSTMatchesEvaluate(t *testing.T) {
	ny := dstLocPkg
	plus8 := time.FixedZone("UTC+8", 8*60*60)
	for _, tc := range dstPurchaseCases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			if err := s.RegisterProduct("p", tc.purchase, 1, nil); err != nil {
				t.Fatalf("register product: %v", err)
			}
			if err := s.RegisterPart("sp", 10); err != nil {
				t.Fatalf("register part: %v", err)
			}
			if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
				t.Fatalf("submit request: %v", err)
			}
			deadline, _ := warrantyExpiry(tc.purchase, 1)
			commitExpiry := deadline.Add(24 * time.Hour)
			if _, err := s.Reserve("c1", "r", "sp", 2, commitExpiry, deadline.Add(-time.Hour)); err != nil {
				t.Fatalf("reserve: %v", err)
			}
			// 截止时刻的失败预留留下不合格历史。
			if _, err := s.Reserve("c2", "r", "sp", 1, commitExpiry, deadline); !errors.Is(err, ErrIneligible) {
				t.Fatalf("reserve at deadline: %v", err)
			}

			// 同一截止时刻分别用纽约、UTC、+8 表示查看：资格与明细完全一致。
			var firstView *RequestView
			for name, now := range map[string]time.Time{
				"ny":    deadline.In(ny),
				"utc":   deadline.UTC(),
				"plus8": deadline.In(plus8),
			} {
				v, err := s.RequestView("r", now)
				if err != nil {
					t.Fatalf("%s request view: %v", name, err)
				}
				if v.Eligibility == nil {
					t.Fatalf("%s view missing eligibility", name)
				}
				if v.Eligibility.Eligible || len(v.Eligibility.Reasons) != 1 ||
					v.Eligibility.Reasons[0] != ReasonWarrantyExpired {
					t.Fatalf("%s view eligibility = %+v, want single warranty_expired", name, v.Eligibility)
				}
				if !v.Eligibility.WarrantyExpiry.Equal(deadline) ||
					v.Eligibility.WarrantyExpiry.Format(time.RFC3339) != deadline.Format(time.RFC3339) {
					t.Fatalf("%s view deadline = %v, want %v", name,
						v.Eligibility.WarrantyExpiry, deadline)
				}
				if v.Eligibility.WarrantyDays != 1 || !v.Eligibility.PurchaseTime.Equal(tc.purchase) {
					t.Fatalf("%s view basis = days %d purchase %v", name,
						v.Eligibility.WarrantyDays, v.Eligibility.PurchaseTime)
				}
				// 此前成功的承诺明细保留：c1 仍 active、两件未用；c2 从未创建。
				if len(v.Commitments) != 1 || v.Commitments[0].CommitmentID != "c1" {
					t.Fatalf("%s view commitments = %+v, want only c1", name, v.Commitments)
				}
				d := v.Commitments[0]
				if d.Status != CommitmentActive || d.OriginalQuantity != 2 ||
					d.UsedQuantity != 0 || d.RemainingQuantity != 2 || !d.Expiry.Equal(commitExpiry) {
					t.Fatalf("%s view c1 detail = %+v", name, d)
				}
				if firstView == nil {
					firstView = v
				} else {
					prev := firstView.Eligibility
					cur := v.Eligibility
					if prev.Eligible != cur.Eligible ||
						!prev.WarrantyExpiry.Equal(cur.WarrantyExpiry) ||
						len(prev.Reasons) != len(cur.Reasons) {
						t.Fatalf("%s view diverged from first: %+v vs %+v", name, cur, prev)
					}
				}
			}

			// 与直接资格查询逐项一致（截止前一纳秒合格、截止时刻过保）。
			directBefore, err := s.Evaluate("r", deadline.Add(-time.Nanosecond))
			if err != nil {
				t.Fatalf("evaluate before: %v", err)
			}
			viewBefore, err := s.RequestView("r", deadline.Add(-time.Nanosecond))
			if err != nil {
				t.Fatalf("view before: %v", err)
			}
			if viewBefore.Eligibility.Eligible != directBefore.Eligible ||
				!viewBefore.Eligibility.WarrantyExpiry.Equal(directBefore.WarrantyExpiry) {
				t.Fatalf("before-deadline view/evaluate differ: %+v vs %+v",
					viewBefore.Eligibility, directBefore)
			}
			directAt, _ := s.Evaluate("r", deadline)
			viewAt, _ := s.RequestView("r", deadline)
			if viewAt.Eligibility.Eligible != directAt.Eligible ||
				len(viewAt.Eligibility.Reasons) != len(directAt.Reasons) ||
				viewAt.Eligibility.Reasons[0] != directAt.Reasons[0] {
				t.Fatalf("at-deadline view/evaluate differ: %+v vs %+v",
					viewAt.Eligibility, directAt)
			}

			// 历史保留成功承诺与失败提交，失败记录资格依据含准确截止时刻与原因。
			hist, err := s.RequestHistory("r")
			if err != nil {
				t.Fatalf("history: %v", err)
			}
			if len(hist) != 2 || !hist[0].Success ||
				hist[1].Success || hist[1].Error != HistoryErrorIneligible {
				t.Fatalf("history = %+v, want c1 success + c2 ineligible", hist)
			}
			basis := hist[1].Eligibility
			if basis == nil || basis.Eligible ||
				!basis.WarrantyExpiry.Equal(deadline) ||
				len(basis.Reasons) != 1 || basis.Reasons[0] != ReasonWarrantyExpired {
				t.Fatalf("failed record eligibility basis = %+v", basis)
			}
		})
	}
}

// TestExcludedFaultAcrossDSTStillEligibleBeforeDeadline 对故障未被除外的请求补一
// 个直接护栏：跨夏令时期间只有实际经过时间决定资格，截止前不因其他原因被拒。
func TestExcludedFaultAcrossDSTStillEligibleBeforeDeadline(t *testing.T) {
	ny := dstLocPkg
	for _, tc := range dstPurchaseCases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			// 登记一个除外代码，但请求使用不同的、未被除外的代码。
			if err := s.RegisterProduct("p", tc.purchase, 1, []string{"BROKEN_SEAL"}); err != nil {
				t.Fatalf("register product: %v", err)
			}
			if err := s.SubmitRequest("r", "p", "NOISE"); err != nil {
				t.Fatalf("submit request: %v", err)
			}
			deadline, _ := warrantyExpiry(tc.purchase, 1)
			for _, inst := range []time.Time{
				tc.purchase,
				// 覆盖当地钟面歧义区间：秋令时 1:30（回拨前后各出现一次）附近，
				// 只要实际时刻早于截止就必须合格。
				deadline.Add(-time.Nanosecond),
			} {
				e, err := s.Evaluate("r", inst)
				if err != nil {
					t.Fatalf("evaluate %v: %v", inst, err)
				}
				if !e.Eligible || len(e.Reasons) != 0 {
					t.Fatalf("non-excluded request at %v rejected: %+v", inst, e)
				}
			}
			// 秋令时重复钟面：用同一钟面 1:30 的两个实际时刻（EDT 与 EST）构造，
			// 验证存储比较的是实际时刻而非钟面文字（两者均早于截止，都应合格）。
			if tc.name == "fall_back" {
				edt130 := time.Date(2026, 11, 1, 1, 30, 0, 0, ny) // 第一次 1:30，EDT
				// Date 对重复钟面取第一次（偏移较大的 EDT）；显式构造第二次 1:30 EST。
				est130 := time.Date(2026, 11, 1, 1, 30, 0, 0, time.FixedZone("EST", -5*3600))
				if edt130.Equal(est130) {
					t.Fatal("test setup: repeated clock-hour instants coincide")
				}
				for _, inst := range []time.Time{edt130, est130} {
					if !inst.Before(deadline) {
						t.Fatalf("test setup: %v not before deadline %v", inst, deadline)
					}
					e, err := s.Evaluate("r", inst)
					if err != nil {
						t.Fatalf("evaluate repeated hour %v: %v", inst, err)
					}
					if !e.Eligible {
						t.Fatalf("repeated-clock-hour instant %v rejected: %+v", inst, e)
					}
				}
			}
		})
	}
}
