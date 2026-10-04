package warranty

import (
	"errors"
	"math"
	"testing"
	"time"
)

// 已存在的非空产品编号始终返回 ErrDuplicateID：新提交的保修天数非正、
// 截止时刻无法表示、除外清单含空代码，都不能改变结果；调用方仍可用
// errors.Is 识别错误类别。
func TestRegisterProductDuplicatePrecedence(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewStore()
	if err := s.RegisterProduct("p", purchase, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("initial register: %v", err)
	}

	far := time.Unix(math.MaxInt64-62135596800-100, 0).UTC()
	badSubmits := []struct {
		name      string
		purchase  time.Time
		days      int
		excluded  []string
	}{
		{"zero days", purchase, 0, nil},
		{"negative days", purchase, -7, nil},
		{"unrepresentable expiry", purchase, math.MaxInt, nil},
		{"near-limit purchase", far, 2, nil},
		{"empty excluded code", purchase, 365, []string{""}},
		{"empty code among valid ones", purchase, 365, []string{"OTHER", ""}},
		{"all bad together", far, 0, []string{""}},
	}
	for _, tc := range badSubmits {
		err := s.RegisterProduct("p", tc.purchase, tc.days, tc.excluded)
		if !errors.Is(err, ErrDuplicateID) {
			t.Errorf("%s: got %v, want ErrDuplicateID", tc.name, err)
		}
		if errors.Is(err, ErrInvalidParam) {
			t.Errorf("%s: duplicate must not also match ErrInvalidParam", tc.name)
		}
	}
}

// 重复登记失败后，原购买时刻、保修天数与完整除外清单原样保留，不被新提交
// 覆盖、清空或部分替换。
func TestRegisterProductDuplicateKeepsOriginalIntact(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewStore()
	original := []string{"BROKEN_SEAL", "WATER_DAMAGE"}
	if err := s.RegisterProduct("p", purchase, 30, original); err != nil {
		t.Fatalf("initial register: %v", err)
	}

	// 试图改成一年保修、移除全部除外代码（且本次参数本身也有非法项）。
	if err := s.RegisterProduct("p", purchase.AddDate(1, 0, 0), 365, nil); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate with different terms: got %v", err)
	}
	if err := s.RegisterProduct("p", purchase.Add(5*24*time.Hour), 0, []string{"NEW_CODE", ""}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate with invalid terms: got %v", err)
	}

	p, err := s.Product("p")
	if err != nil {
		t.Fatalf("get product: %v", err)
	}
	if !p.PurchaseTime.Equal(purchase) {
		t.Fatalf("purchase time changed: got %v, want %v", p.PurchaseTime, purchase)
	}
	if p.WarrantyDays != 30 {
		t.Fatalf("warranty days changed: got %d, want 30", p.WarrantyDays)
	}
	if len(p.ExcludedCodes) != len(original) {
		t.Fatalf("excluded codes changed: got %v, want %v", p.ExcludedCodes, original)
	}
	for _, c := range original {
		if _, ok := p.ExcludedCodes[c]; !ok {
			t.Fatalf("excluded code %q missing from preserved record: %v", c, p.ExcludedCodes)
		}
	}
	if _, ok := p.ExcludedCodes["NEW_CODE"]; ok {
		t.Fatal("failed duplicate partially replaced the exclusion list")
	}
}

// 尚未登记的编号继续按条款校验：非法提交不留下记录、不占住编号；失败后用
// 同一非空编号提交合法资料应能正常登记，之后再提交才算重复。
func TestRegisterProductInvalidDoesNotOccupyID(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	far := time.Unix(math.MaxInt64-62135596800-100, 0).UTC()

	cases := []struct {
		name     string
		purchase time.Time
		days     int
		excluded []string
	}{
		{"zero days", purchase, 0, nil},
		{"negative days", purchase, -1, nil},
		{"unrepresentable days", purchase, math.MaxInt, nil},
		{"near-limit purchase", far, 2, nil},
		{"empty excluded code", purchase, 30, []string{"OK", ""}},
	}
	for _, tc := range cases {
		s := NewStore()
		if err := s.RegisterProduct("p", tc.purchase, tc.days, tc.excluded); !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("%s: got %v, want ErrInvalidParam", tc.name, err)
		}
		if _, err := s.Product("p"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: failed registration left a product, got %v", tc.name, err)
		}
		// 用同一非空编号提交合法资料：正常登记。
		if err := s.RegisterProduct("p", purchase, 30, []string{"BROKEN_SEAL"}); err != nil {
			t.Fatalf("%s: valid re-register after failure: %v", tc.name, err)
		}
		// 之后再提交才算重复，即使新内容非法也按重复处理。
		if err := s.RegisterProduct("p", purchase, 0, nil); !errors.Is(err, ErrDuplicateID) {
			t.Fatalf("%s: after success got %v, want ErrDuplicateID", tc.name, err)
		}
	}

	// 空编号始终是参数错误，从不落库。
	s := NewStore()
	if err := s.RegisterProduct("", purchase, 30, nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty id: got %v", err)
	}
}

// 端到端场景：原记录三十天保修且除外 BROKEN_SEAL；重复登记试图改成一年
// 保修并移除该代码失败后，资格判断始终以原记录为准——原保修截止时刻起
// 过保，保修期内该故障仍因除外被拒绝；按请求查看资格依据与直接查询资格
// 一致；对不合格请求预留备件仍返回 ErrIneligible，不创建承诺或占用数量。
func TestDuplicateProductDoesNotAffectEligibilityOrReserve(t *testing.T) {
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry30 := purchase.Add(30 * 24 * time.Hour)
	s := NewStore()
	if err := s.RegisterProduct("p", purchase, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterPart("sp", 10); err != nil {
		t.Fatal(err)
	}
	// 重复登记：一年保修、移除除外代码，且天数以外的参数仍合法——必须失败。
	if err := s.RegisterProduct("p", purchase, 365, nil); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate with one-year terms: got %v", err)
	}
	if err := s.SubmitRequest("rSeal", "p", "BROKEN_SEAL"); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitRequest("rNoise", "p", "NOISE"); err != nil {
		t.Fatal(err)
	}
	commitExpiry := purchase.Add(400 * 24 * time.Hour)

	// 保修期内：BROKEN_SEAL 仍因除外被拒绝；RequestView 与 Evaluate 一致。
	inWarranty := expiry30.Add(-time.Second)
	elig, err := s.Evaluate("rSeal", inWarranty)
	if err != nil {
		t.Fatal(err)
	}
	if elig.Eligible || !containsReason(elig.Reasons, ReasonFaultExcluded) {
		t.Fatalf("excluded fault within warranty: %+v", elig)
	}
	view, err := s.RequestView("rSeal", inWarranty)
	if err != nil {
		t.Fatal(err)
	}
	if view.Eligibility.Eligible != elig.Eligible ||
		len(view.Eligibility.Reasons) != len(elig.Reasons) ||
		!view.Eligibility.WarrantyExpiry.Equal(expiry30) ||
		view.Eligibility.WarrantyDays != 30 {
		t.Fatalf("RequestView %+v inconsistent with Evaluate %+v", view.Eligibility, elig)
	}

	// 原保修截止时刻起：普通故障也过保（不是一年）。
	atExpiry := expiry30
	e2, err := s.Evaluate("rNoise", atExpiry)
	if err != nil {
		t.Fatal(err)
	}
	if e2.Eligible || !containsReason(e2.Reasons, ReasonWarrantyExpired) {
		t.Fatalf("noise request at original expiry: %+v", e2)
	}
	view2, err := s.RequestView("rNoise", atExpiry)
	if err != nil {
		t.Fatal(err)
	}
	if view2.Eligibility.Eligible != e2.Eligible {
		t.Fatalf("RequestView eligible=%v, Evaluate eligible=%v", view2.Eligibility.Eligible, e2.Eligible)
	}

	// 对不合格请求预留：返回已有不合格错误，不创建承诺、不占用数量。
	if _, err := s.Reserve("cSeal", "rSeal", "sp", 1, commitExpiry, inWarranty); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve for excluded request: got %v, want ErrIneligible", err)
	}
	if _, err := s.Reserve("cExpired", "rNoise", "sp", 1, commitExpiry, atExpiry); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve for expired request: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("cSeal"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ineligible reserve created a commitment: %v", err)
	}
	if _, err := s.Commitment("cExpired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ineligible reserve created a commitment: %v", err)
	}
	st, err := s.PartStatus("sp", inWarranty)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveOccupied != 0 || st.Committable != 10 || st.PhysicalRemaining != 10 {
		t.Fatalf("ineligible reserves occupied stock: %+v", st)
	}
}
