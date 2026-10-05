package warranty

import (
	"errors"
	"math"
	"testing"
	"time"
)

// 已登记编号的重复登记携带明显不同的条款（购买时刻、保修天数、除外清单
// 均不同）时，仍只报 ErrDuplicateID，且取回的产品资料完整保留第一次成功
// 登记的购买时刻、天数和除外集合，不被部分替换。
func TestDuplicateProductDifferentTermsKeepsOriginal(t *testing.T) {
	s := NewStore()
	purchase := t0
	if err := s.RegisterProduct("p1", purchase, 30, []string{"FAULTX", "FAULTZ"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	// 新条款与旧条款明显不同：购买时刻、天数、除外清单全部改变。
	different := t0.Add(5 * day)
	if err := s.RegisterProduct("p1", different, 7, []string{"FAULTY"}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate with different terms: got %v, want ErrDuplicateID", err)
	}
	p, err := s.Product("p1")
	if err != nil {
		t.Fatalf("product: %v", err)
	}
	if !p.PurchaseTime.Equal(purchase) {
		t.Fatalf("purchase time = %v, want original %v", p.PurchaseTime, purchase)
	}
	if p.WarrantyDays != 30 {
		t.Fatalf("warranty days = %d, want original 30", p.WarrantyDays)
	}
	if len(p.ExcludedCodes) != 2 {
		t.Fatalf("excluded codes = %v, want original two entries", p.ExcludedCodes)
	}
	for _, code := range []string{"FAULTX", "FAULTZ"} {
		if _, ok := p.ExcludedCodes[code]; !ok {
			t.Fatalf("excluded codes = %v, missing original %q", p.ExcludedCodes, code)
		}
	}
	if _, ok := p.ExcludedCodes["FAULTY"]; ok {
		t.Fatalf("excluded codes = %v, must not gain new-list-only FAULTY", p.ExcludedCodes)
	}
}

// 已登记编号再次登记时，即使新提交的条款本身非法（保修天数为零或负数、
// 正数但截止时刻超出可表示范围、除外清单含空代码），也优先报 ErrDuplicateID，
// 不能先返回 ErrInvalidParam，原记录保持不变。
func TestDuplicateProductInvalidTermsStillDuplicate(t *testing.T) {
	cases := []struct {
		name     string
		purchase time.Time
		days     int
		excluded []string
	}{
		{"zero days", t0, 0, nil},
		{"negative days", t0, -3, nil},
		{"unrepresentable expiry", t0, math.MaxInt, nil},
		{"empty excluded code", t0, 30, []string{"FAULTX", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
				t.Fatalf("register product: %v", err)
			}
			err := s.RegisterProduct("p1", tc.purchase, tc.days, tc.excluded)
			if !errors.Is(err, ErrDuplicateID) {
				t.Fatalf("duplicate with invalid terms: got %v, want ErrDuplicateID", err)
			}
			if errors.Is(err, ErrInvalidParam) {
				t.Fatalf("duplicate must not surface ErrInvalidParam: %v", err)
			}
			p, err := s.Product("p1")
			if err != nil {
				t.Fatalf("product: %v", err)
			}
			if !p.PurchaseTime.Equal(t0) || p.WarrantyDays != 30 || len(p.ExcludedCodes) != 1 {
				t.Fatalf("original record changed: %+v", p)
			}
			if _, ok := p.ExcludedCodes["FAULTX"]; !ok {
				t.Fatalf("original exclusion lost: %v", p.ExcludedCodes)
			}
		})
	}
}

// 重复登记被拒绝后，资格判断继续按原购买时刻与原期限：按原条款仍在保的
// 请求不因新期限较短而被拒；按原条款已过保的请求也不借较长的新期限重新
// 合格。原清单中的除外故障仍被拒绝，仅出现在新清单中的故障不构成除外。
// 直接查询资格与按请求查看资格依据都显示原保修条款和对应拒绝原因。
func TestDuplicateProductDoesNotChangeEligibility(t *testing.T) {
	s := NewStore()
	// p1：原条款 30 天，除外 FAULTX。
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register p1: %v", err)
	}
	// p2：原条款 10 天（t0+10d 起过保），无除外。
	if err := s.RegisterProduct("p2", t0, 10, nil); err != nil {
		t.Fatalf("register p2: %v", err)
	}
	// 试图用更短期限和新除外清单覆盖 p1，用更长期限覆盖 p2，均被拒绝。
	if err := s.RegisterProduct("p1", t0, 5, []string{"FAULTY"}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate p1: got %v, want ErrDuplicateID", err)
	}
	if err := s.RegisterProduct("p2", t0, 365, nil); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate p2: got %v, want ErrDuplicateID", err)
	}

	// 第 10 天：按原条款 p1 在保；若新条款（5 天）生效则已过保。
	if err := s.SubmitRequest("rOK", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit rOK: %v", err)
	}
	e, err := s.Evaluate("rOK", nowOK)
	if err != nil {
		t.Fatalf("evaluate rOK: %v", err)
	}
	if !e.Eligible || len(e.Reasons) != 0 {
		t.Fatalf("original terms still in warranty: got %+v", e)
	}
	// 资格依据必须是原条款：购买时刻 t0、30 天、截止 t0+30d。
	if !e.PurchaseTime.Equal(t0) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(t0.Add(30*day)) {
		t.Fatalf("eligibility basis = (%v, %d, %v), want original (%v, 30, %v)",
			e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, t0, t0.Add(30*day))
	}
	// FAULTY 仅出现在被拒绝的新清单中，不能成为除外。
	if e.Excluded {
		t.Fatalf("new-list-only fault became excluded: %+v", e)
	}

	// 原清单中的除外故障仍被拒绝。
	if err := s.SubmitRequest("rExcl", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit rExcl: %v", err)
	}
	e, err = s.Evaluate("rExcl", nowOK)
	if err != nil {
		t.Fatalf("evaluate rExcl: %v", err)
	}
	if e.Eligible || !containsReason(e.Reasons, ReasonFaultExcluded) || !e.Excluded {
		t.Fatalf("original exclusion must still reject: %+v", e)
	}

	// 第 20 天：按原条款 p2 已过保；若新条款（365 天）生效则仍在保。
	if err := s.SubmitRequest("rExp", "p2", "FAULTY"); err != nil {
		t.Fatalf("submit rExp: %v", err)
	}
	e, err = s.Evaluate("rExp", t0.Add(20*day))
	if err != nil {
		t.Fatalf("evaluate rExp: %v", err)
	}
	if e.Eligible || !containsReason(e.Reasons, ReasonWarrantyExpired) {
		t.Fatalf("longer new term must not re-qualify expired request: %+v", e)
	}
	if e.WarrantyDays != 10 || !e.WarrantyExpiry.Equal(t0.Add(10*day)) {
		t.Fatalf("expired basis = (%d, %v), want original (10, %v)",
			e.WarrantyDays, e.WarrantyExpiry, t0.Add(10*day))
	}

	// 按请求查看资格依据，同样显示原保修条款和对应拒绝原因。
	view, err := s.RequestView("rExp", t0.Add(20*day))
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	ve := view.Eligibility
	if ve.Eligible || !containsReason(ve.Reasons, ReasonWarrantyExpired) {
		t.Fatalf("view eligibility: %+v", ve)
	}
	if !ve.PurchaseTime.Equal(t0) || ve.WarrantyDays != 10 || !ve.WarrantyExpiry.Equal(t0.Add(10*day)) {
		t.Fatalf("view basis = (%v, %d, %v), want original (%v, 10, %v)",
			ve.PurchaseTime, ve.WarrantyDays, ve.WarrantyExpiry, t0, t0.Add(10*day))
	}
	viewOK, err := s.RequestView("rOK", nowOK)
	if err != nil {
		t.Fatalf("request view rOK: %v", err)
	}
	if !viewOK.Eligibility.Eligible || viewOK.Eligibility.WarrantyDays != 30 {
		t.Fatalf("view for in-warranty request: %+v", viewOK.Eligibility)
	}
}

// 备件预留同样只认原条款：按原条款在保且无除外命中的请求，在库存足够、
// 承诺到期时刻晚于当前时刻时仍可预留；按原条款不合格的请求仍返回
// ErrIneligible，不生成承诺、不增加库存占用。重复登记失败不改动已有承诺
// 和已有预留历史中的资格依据。
func TestDuplicateProductDoesNotChangeReserve(t *testing.T) {
	s := newStore(t) // p1: t0 起 30 天，除外 FAULTX；part1 库存 10；r1 故障 FAULTY。
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit rBad: %v", err)
	}
	// 先建立一笔成功预留和一条历史记录。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	histBefore, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(histBefore) != 1 || !histBefore[0].Success || histBefore[0].Eligibility == nil {
		t.Fatalf("unexpected history before duplicate: %+v", histBefore)
	}

	// 重复登记失败：新条款更短且把 FAULTY 列入除外。
	if err := s.RegisterProduct("p1", t0, 5, []string{"FAULTY"}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate p1: got %v, want ErrDuplicateID", err)
	}

	// 已有承诺不被改动。
	c1, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("commitment c1: %v", err)
	}
	if c1.Quantity != 4 || c1.Used != 0 || c1.Canceled || c1.Expired {
		t.Fatalf("existing commitment changed: %+v", c1)
	}
	// 已有历史中的资格依据不被改动。
	histAfter, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history after duplicate: %v", err)
	}
	if len(histAfter) != 1 {
		t.Fatalf("duplicate registration appended history: %+v", histAfter)
	}
	basis := histAfter[0].Eligibility
	if basis.WarrantyDays != 30 || !basis.PurchaseTime.Equal(t0) ||
		!basis.WarrantyExpiry.Equal(t0.Add(30*day)) || !basis.Eligible {
		t.Fatalf("history eligibility basis changed: %+v", basis)
	}

	// 按原条款在保且无除外命中的请求仍可预留。
	if _, err := s.Reserve("c2", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve under original terms: %v", err)
	}
	// 按原条款不合格（命中原除外清单）的请求仍返回 ErrIneligible，
	// 不生成承诺，也不增加库存占用。
	if _, err := s.Reserve("c3", "rBad", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve excluded fault: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("c3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ineligible reserve must not create a commitment, got %v", err)
	}
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.ActiveOccupied != 8 || st.Committable != 2 {
		t.Fatalf("stock after failed duplicate: occupied=%d committable=%d, want 8/2",
			st.ActiveOccupied, st.Committable)
	}
}

// 区分重复编号和首次登记失败：尚未登记的非空编号提交非法条款时仍返回
// ErrInvalidParam，不留下产品记录；随后用同一编号提交合法条款能正常登记。
func TestUnregisteredProductInvalidTermsRejected(t *testing.T) {
	s := NewStore()
	cases := []struct {
		id       string
		purchase time.Time
		days     int
		excluded []string
	}{
		{"pz", t0, 0, nil},
		{"pn", t0, -1, nil},
		{"pu", t0, math.MaxInt, nil},
		{"pe", t0, 30, []string{""}},
	}
	for _, tc := range cases {
		if err := s.RegisterProduct(tc.id, tc.purchase, tc.days, tc.excluded); !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("first registration with invalid terms (%s): got %v, want ErrInvalidParam", tc.id, err)
		}
		if _, err := s.Product(tc.id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rejected first registration must not save %s, got %v", tc.id, err)
		}
		// 同一编号随后提交合法条款，应能正常登记。
		if err := s.RegisterProduct(tc.id, t0, 30, []string{"FAULTX"}); err != nil {
			t.Fatalf("re-register %s with valid terms: %v", tc.id, err)
		}
		p, err := s.Product(tc.id)
		if err != nil {
			t.Fatalf("product %s: %v", tc.id, err)
		}
		if p.WarrantyDays != 30 {
			t.Fatalf("product %s warranty days = %d, want 30", tc.id, p.WarrantyDays)
		}
	}
}
