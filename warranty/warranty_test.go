package warranty

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 固定时间基线，避免任何系统时钟依赖。
var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func hour(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }

// setup 构建一个常见现场：
// 产品 P1：t0 购买，保修 10 天，除外代码 E1；
// 产品 P2：t0 购买，保修 1 天，除外代码 X1。
// 备件 A 库存 10，备件 B 库存 0。
func setup(t *testing.T) *Service {
	t.Helper()
	s := NewService()
	must(t, s.RegisterProduct("P1", t0, 10, []string{"E1"}, t0))
	must(t, s.RegisterProduct("P2", t0, 1, []string{"X1"}, t0))
	must(t, s.RegisterPart("A", 10))
	must(t, s.RegisterPart("B", 0))
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadyPreserved(t *testing.T) {
	if !Ready() {
		t.Fatal("Ready() must keep returning true")
	}
}

// ---------- 登记 ----------

func TestRegisterDuplicatesKeepOriginal(t *testing.T) {
	s := setup(t)

	if err := s.RegisterProduct("P1", hour(1), 20, nil, hour(5)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("dup product: want ErrDuplicate, got %v", err)
	}
	if err := s.RegisterPart("A", 999); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("dup part: want ErrDuplicate, got %v", err)
	}
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	if err := s.RegisterRequest("R1", "P1", "F2"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("dup request: want ErrDuplicate, got %v", err)
	}

	//原记录保持不变：P1 仍是 10 天保修；A 库存仍为 10；R1 故障码仍为 F1。
	end := t0.Add(10 * 24 * time.Hour)
	eligible, reasons, err := s.Eligible("R1", end.Add(-time.Nanosecond))
	must(t, err)
	if !eligible {
		t.Fatalf("original product altered: reasons=%v", reasons)
	}
	v, err := s.QueryPart("A", t0)
	must(t, err)
	if v.PhysicalRemaining != 10 {
		t.Fatalf("original stock altered: %d", v.PhysicalRemaining)
	}
}

func TestRegisterProductInvalid(t *testing.T) {
	s := NewService()
	cases := []struct {
		name string
		id   string
		at   time.Time
		days int
		now  time.Time
	}{
		{"empty id", "", t0, 10, t0},
		{"zero days", "P", t0, 0, t0},
		{"negative days", "P", t0, -3, t0},
		{"purchase after now", "P", t0.Add(time.Hour), 10, t0},
	}
	for _, c := range cases {
		if err := s.RegisterProduct(c.id, c.at, c.days, nil, c.now); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: want ErrInvalidArgument, got %v", c.name, err)
		}
	}
	if err := s.RegisterProduct("P", t0, 10, []string{""}, t0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty excluded code: want ErrInvalidArgument, got %v", err)
	}
}

func TestRegisterPartInvalid(t *testing.T) {
	s := NewService()
	if err := s.RegisterPart("", 5); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty id: %v", err)
	}
	if err := s.RegisterPart("A", -1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative stock: %v", err)
	}
}

func TestRegisterRequestValidation(t *testing.T) {
	s := setup(t)
	if err := s.RegisterRequest("R", "NOPE", "F1"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown product: want ErrUnknown, got %v", err)
	}
	for _, tc := range [][3]string{
		{"", "P1", "F1"},
		{"R", "", "F1"},
		{"R", "P1", ""},
	} {
		if err := s.RegisterRequest(tc[0], tc[1], tc[2]); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("tc=%v: want ErrInvalidArgument, got %v", tc, err)
		}
	}
	if _, err := s.QueryRequest("R", t0); !errors.Is(err, ErrUnknown) {
		t.Fatalf("failed request must not be created, got %v", err)
	}
}

// ---------- 资格边界 ----------

func TestWarrantyBoundary24hDays(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	end := t0.Add(10 * 24 * time.Hour) // 购买时刻 + 10 天

	if ok, _, _ := s.Eligible("R1", end.Add(-time.Nanosecond)); !ok {
		t.Fatal("one ns before end must be eligible")
	}
	ok, reasons, err := s.Eligible("R1", end) // 截止时刻开始算过保
	must(t, err)
	if ok || len(reasons) != 1 || reasons[0] != ReasonExpired {
		t.Fatalf("at end: want expired, got ok=%v reasons=%v", ok, reasons)
	}
}

func TestExcludedCodeRejected(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "E1"))
	ok, reasons, err := s.Eligible("R1", t0)
	must(t, err)
	if ok || len(reasons) != 1 || reasons[0] != ReasonExcluded {
		t.Fatalf("want excluded, got ok=%v reasons=%v", ok, reasons)
	}
}

func TestBothReasonsListed(t *testing.T) {
	s := setup(t)
	// P2 保修 1 天，X1 为除外代码：在过保时刻用除外代码 -> 两项原因，固定顺序。
	must(t, s.RegisterRequest("R2", "P2", "X1"))
	end := t0.Add(24 * time.Hour)
	ok, reasons, err := s.Eligible("R2", end)
	must(t, err)
	if ok {
		t.Fatal("want not eligible")
	}
	if len(reasons) != 2 || reasons[0] != ReasonExpired || reasons[1] != ReasonExcluded {
		t.Fatalf("want [expired excluded], got %v", reasons)
	}
}

func TestEligibleUnknownRequest(t *testing.T) {
	s := setup(t)
	if _, _, err := s.Eligible("GHOST", t0); !errors.Is(err, ErrUnknown) {
		t.Fatalf("want ErrUnknown, got %v", err)
	}
}

// ---------- 预留 ----------

func TestReserveHappyAndIdempotent(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	exp := hour(24)

	cid, err := s.Reserve("S1", "R1", "A", 3, exp, hour(1))
	must(t, err)
	if cid == "" {
		t.Fatal("empty commitment id")
	}

	// 相同提交编号 + 相同内容 -> 同一承诺。
	cid2, err := s.Reserve("S1", "R1", "A", 3, exp, hour(2))
	must(t, err)
	if cid2 != cid {
		t.Fatalf("retry must return same commitment: %q vs %q", cid, cid2)
	}

	v, err := s.QueryPart("A", hour(2))
	must(t, err)
	if v.PhysicalRemaining != 10 || v.ActiveHeld != 3 || v.Available != 7 {
		t.Fatalf("unexpected part view: %+v", v)
	}
}

func TestReserveConflictOnAnyChangedField(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	must(t, s.RegisterRequest("R2", "P1", "F2"))
	exp := hour(48)
	_, err := s.Reserve("S1", "R1", "A", 2, exp, hour(1))
	must(t, err)

	alt := hour(72)
	cases := []struct {
		name      string
		req, part string
		qty       int
		exp       time.Time
	}{
		{"request", "R2", "A", 2, exp},
		{"part", "R1", "B", 2, exp},
		{"qty", "R1", "A", 3, exp},
		{"expiry", "R1", "A", 2, alt},
	}
	for _, c := range cases {
		// 冲突检测先于库存/未知校验：即使 B 库存为 0，也应先报冲突。
		if _, err := s.Reserve("S1", c.req, c.part, c.qty, c.exp, hour(1)); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: want ErrConflict, got %v", c.name, err)
		}
	}
	// 冲突不得改变库存/占用。
	v, _ := s.QueryPart("A", hour(1))
	if v.ActiveHeld != 2 || v.Available != 8 {
		t.Fatalf("conflict altered state: %+v", v)
	}
}

func TestReserveValidation(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	exp := hour(24)

	if _, err := s.Reserve("", "R1", "A", 1, exp, hour(1)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty submit: %v", err)
	}
	if _, err := s.Reserve("S", "R1", "A", 0, exp, hour(1)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero qty: %v", err)
	}
	if _, err := s.Reserve("S", "R1", "A", -2, exp, hour(1)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative qty: %v", err)
	}
	if _, err := s.Reserve("S", "R1", "A", 1, hour(1), hour(1)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("expiry == now: %v", err)
	}
	if _, err := s.Reserve("S", "R1", "A", 1, hour(0), hour(1)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("expiry before now: %v", err)
	}
	if _, err := s.Reserve("S", "GHOST", "A", 1, exp, hour(1)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown request: %v", err)
	}
	if _, err := s.Reserve("S", "R1", "GHOST", 1, exp, hour(1)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown part: %v", err)
	}
}

func TestReserveIneligibleCarriesReasons(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R2", "P2", "X1"))
	end := t0.Add(24 * time.Hour)
	_, err := s.Reserve("S1", "R2", "A", 1, end.Add(time.Hour), end)
	if !errors.Is(err, ErrNotEligible) {
		t.Fatalf("want ErrNotEligible, got %v", err)
	}
	reasons, ok := EligibilityErrorReasons(err)
	if !ok || len(reasons) != 2 {
		t.Fatalf("want both reasons, got %v ok=%v", reasons, ok)
	}
	// 不合格不得占用。
	v, _ := s.QueryPart("A", end)
	if v.ActiveHeld != 0 || v.Available != 10 {
		t.Fatalf("ineligible reserve altered stock: %+v", v)
	}
	if len(s.commitments) != 0 {
		t.Fatal("ineligible reserve must not create a commitment")
	}
}

func TestReserveReevaluatesEachTime(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	// 在保修期内可预留。
	_, err := s.Reserve("S1", "R1", "A", 1, hour(100), hour(1))
	must(t, err)
	// 过保后的新预留被拒（即使库存充足）。
	_, err = s.Reserve("S2", "R1", "A", 1, hour(300), hour(250))
	if !errors.Is(err, ErrNotEligible) {
		t.Fatalf("late reserve: want ErrNotEligible, got %v", err)
	}
}

func TestReserveInsufficient(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	_, err := s.Reserve("S1", "R1", "A", 6, hour(24), hour(1))
	must(t, err)
	// 仅剩 4，申请 5 失败。
	_, err = s.Reserve("S2", "R1", "A", 5, hour(24), hour(1))
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("want ErrInsufficient, got %v", err)
	}
	if avail, ok := AvailableFromError(err); !ok || avail != 4 {
		t.Fatalf("want available=4, got %d ok=%v", avail, ok)
	}
	// 失败不占用：仍可申请 4。
	_, err = s.Reserve("S3", "R1", "A", 4, hour(24), hour(1))
	must(t, err)
}

func TestMultipleCommitmentsPerRequestIndependent(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	c1, err := s.Reserve("S1", "R1", "A", 2, hour(24), hour(1))
	must(t, err)
	c2, err := s.Reserve("S2", "R1", "A", 3, hour(24), hour(1))
	must(t, err)
	if c1 == c2 {
		t.Fatal("commitments must be independent")
	}
	qv, _ := s.QueryRequest("R1", hour(1))
	if len(qv.Commitments) != 2 {
		t.Fatalf("want 2 linked commitments, got %d", len(qv.Commitments))
	}
}

// ---------- 使用 ----------

func TestUseBatchAndDeduct(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, err := s.Reserve("S1", "R1", "A", 5, hour(48), hour(1))
	must(t, err)

	must(t, s.Use("U1", cid, 2, hour(1)))
	must(t, s.Use("U2", cid, 2, hour(1)))

	v, _ := s.QueryPart("A", hour(1))
	if v.PhysicalRemaining != 6 || v.ActiveHeld != 1 || v.Available != 5 {
		t.Fatalf("after 4 used: %+v", v)
	}
	it := v.Items[0]
	if it.Quantity != 5 || it.Used != 4 || it.Remaining != 1 {
		t.Fatalf("item mismatch: %+v", it)
	}
}

func TestUseOverRemainingFailsWhole(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, _ := s.Reserve("S1", "R1", "A", 3, hour(48), hour(1))

	if err := s.Use("U1", cid, 4, hour(1)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("over use: want ErrInvalidArgument, got %v", err)
	}
	v, _ := s.QueryPart("A", hour(1))
	if v.PhysicalRemaining != 10 || v.ActiveHeld != 3 {
		t.Fatalf("failed use must not deduct: %+v", v)
	}
	// 失败的使用编号可被复用（此前未成功）。
	must(t, s.Use("U1", cid, 3, hour(1)))
}

func TestUseIdempotentAndConflict(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	c1, _ := s.Reserve("S1", "R1", "A", 5, hour(48), hour(1))
	c2, _ := s.Reserve("S2", "R1", "A", 5, hour(48), hour(1))

	must(t, s.Use("U1", c1, 2, hour(1)))
	// 相同编号相同内容 -> 返回首次结果，不再扣减。
	must(t, s.Use("U1", c1, 2, hour(1)))
	v, _ := s.QueryPart("A", hour(1))
	if v.PhysicalRemaining != 8 {
		t.Fatalf("idempotent use double-deducted: physical=%d", v.PhysicalRemaining)
	}
	// 改数量 -> 冲突。
	if err := s.Use("U1", c1, 3, hour(1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("change qty: want ErrConflict, got %v", err)
	}
	// 改承诺 -> 冲突。
	if err := s.Use("U1", c2, 2, hour(1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("change commitment: want ErrConflict, got %v", err)
	}
}

func TestUseRetriedAfterCancelOrExpiryReturnsOriginal(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, _ := s.Reserve("S1", "R1", "A", 5, hour(48), hour(1))
	must(t, s.Use("U1", cid, 2, hour(1)))

	must(t, s.Cancel(cid, hour(2)))
	// 承诺已取消，原使用重试仍成功且不再扣减。
	must(t, s.Use("U1", cid, 2, hour(2)))
	v, _ := s.QueryPart("A", hour(2))
	if v.PhysicalRemaining != 8 { // 只扣过一次 2
		t.Fatalf("retry after cancel changed stock: %d", v.PhysicalRemaining)
	}

	// 到期场景另起一条。
	c2, _ := s.Reserve("S2", "R1", "A", 5, hour(48), hour(1))
	must(t, s.Use("U2", c2, 2, hour(1)))
	// 到期后新使用被拒。
	if err := s.Use("U3", c2, 1, hour(48)); !errors.Is(err, ErrCommitmentInactive) {
		t.Fatalf("use expired: want inactive, got %v", err)
	}
	// 原使用重试仍返回首次结果。
	must(t, s.Use("U2", c2, 2, hour(48)))
}

func TestUsedUpRejectsNewUse(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, _ := s.Reserve("S1", "R1", "A", 2, hour(48), hour(1))
	must(t, s.Use("U1", cid, 2, hour(1)))
	if err := s.Use("U2", cid, 1, hour(1)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("used_up new use: want ErrInvalidArgument, got %v", err)
	}
	qv, _ := s.QueryRequest("R1", hour(1))
	if qv.Commitments[0].Status != StatusUsedUp {
		t.Fatalf("want used_up, got %s", qv.Commitments[0].Status)
	}
}

func TestUseUnknownCommitment(t *testing.T) {
	s := setup(t)
	if err := s.Use("U1", "GHOST", 1, t0); !errors.Is(err, ErrUnknown) {
		t.Fatalf("want ErrUnknown, got %v", err)
	}
}

// ---------- 取消 ----------

func TestCancelReleasesUnusedOnce(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, _ := s.Reserve("S1", "R1", "A", 5, hour(48), hour(1))
	must(t, s.Use("U1", cid, 2, hour(1)))
	// 未用 3 被占用。
	v, _ := s.QueryPart("A", hour(1))
	if v.ActiveHeld != 3 || v.Available != 5 {
		t.Fatalf("pre-cancel: %+v", v)
	}

	must(t, s.Cancel(cid, hour(2)))
	v, _ = s.QueryPart("A", hour(2))
	if v.PhysicalRemaining != 8 || v.ActiveHeld != 0 || v.Available != 8 {
		t.Fatalf("after cancel: %+v", v)
	}
	if v.Items[0].Status != StatusCanceled || v.Items[0].Remaining != 0 || v.Items[0].Used != 2 {
		t.Fatalf("cancel item mismatch: %+v", v.Items[0])
	}

	// 重复取消不增加库存。
	must(t, s.Cancel(cid, hour(3)))
	v, _ = s.QueryPart("A", hour(3))
	if v.PhysicalRemaining != 8 || v.Available != 8 {
		t.Fatalf("repeat cancel altered stock: %+v", v)
	}
	// 取消后不能再使用。
	if err := s.Use("U9", cid, 1, hour(3)); !errors.Is(err, ErrCommitmentInactive) {
		t.Fatalf("use canceled: want inactive, got %v", err)
	}
	// 释放出的量可被再次预留。
	_, err := s.Reserve("S3", "R1", "A", 8, hour(48), hour(3))
	must(t, err)
}

func TestCancelExpiredDoesNotDoubleRelease(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, _ := s.Reserve("S1", "R1", "A", 5, hour(48), hour(1))
	// 到期自动释放。
	v, _ := s.QueryPart("A", hour(48))
	if v.ActiveHeld != 0 || v.Available != 10 || v.Items[0].Status != StatusExpired {
		t.Fatalf("at expiry: %+v", v)
	}
	must(t, s.Cancel(cid, hour(49)))
	v, _ = s.QueryPart("A", hour(49))
	if v.PhysicalRemaining != 10 || v.Available != 10 {
		t.Fatalf("cancel after expiry must not add stock: %+v", v)
	}
}

func TestCancelUnknown(t *testing.T) {
	s := setup(t)
	if err := s.Cancel("GHOST", t0); !errors.Is(err, ErrUnknown) {
		t.Fatalf("want ErrUnknown, got %v", err)
	}
}

// ---------- 到期自动失效 ----------

func TestExpiryAutoReleasesWithoutCleanup(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, _ := s.Reserve("S1", "R1", "A", 4, hour(48), hour(1))
	must(t, s.Use("U1", cid, 1, hour(1)))

	// 临到期前：实物 9，占用 3。
	before, _ := s.QueryPart("A", hour(48).Add(-time.Nanosecond))
	if before.ActiveHeld != 3 || before.Available != 6 {
		t.Fatalf("just before expiry: %+v", before)
	}
	// 到期时刻：剩余 3 自动释放，实物仍为 9（已用 1 不回补）。
	at, _ := s.QueryPart("A", hour(48))
	if at.PhysicalRemaining != 9 || at.ActiveHeld != 0 || at.Available != 9 {
		t.Fatalf("at expiry: %+v", at)
	}
	// 到期承诺不能使用。
	if err := s.Use("U2", cid, 1, hour(48)); !errors.Is(err, ErrCommitmentInactive) {
		t.Fatalf("use at expiry: %v", err)
	}
}

func TestInactiveStatusCarried(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, _ := s.Reserve("S1", "R1", "A", 2, hour(48), hour(1))
	errExp := s.Use("U1", cid, 1, hour(48))
	st, ok := InactiveStatusFromError(errExp)
	if !ok || st != StatusExpired {
		t.Fatalf("want expired status, got %q ok=%v", st, ok)
	}
	must(t, s.Cancel(cid, hour(2)))
	errCxl := s.Use("U2", cid, 1, hour(2))
	st, ok = InactiveStatusFromError(errCxl)
	if !ok || st != StatusCanceled {
		t.Fatalf("want canceled status, got %q ok=%v", st, ok)
	}
}

// ---------- 查询 ----------

func TestQueryRequestIncludesHistory(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	c1, _ := s.Reserve("S1", "R1", "A", 3, hour(24), hour(1))
	c2, _ := s.Reserve("S2", "R1", "A", 2, hour(100), hour(1))
	must(t, s.Use("U1", c1, 3, hour(1))) // c1 全部使用
	must(t, s.Cancel(c2, hour(2)))       // c2 取消

	qv, err := s.QueryRequest("R1", hour(2))
	must(t, err)
	if !qv.Eligible || len(qv.Reasons) != 0 {
		t.Fatalf("want eligible no reasons, got %+v", qv)
	}
	if len(qv.Commitments) != 2 {
		t.Fatalf("want 2 commitments (history kept), got %d", len(qv.Commitments))
	}
	status := map[string]string{}
	for _, c := range qv.Commitments {
		status[c.CommitmentID] = c.Status
	}
	if status[c1] != StatusUsedUp || status[c2] != StatusCanceled {
		t.Fatalf("unexpected statuses: %v", status)
	}

	if _, err := s.QueryRequest("GHOST", hour(2)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown request query: %v", err)
	}
}

func TestQueryPartItemsFields(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	c1, _ := s.Reserve("S1", "R1", "A", 4, hour(24), hour(1))
	must(t, s.Use("U1", c1, 1, hour(1)))

	v, err := s.QueryPart("A", hour(1))
	must(t, err)
	if len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(v.Items))
	}
	it := v.Items[0]
	if it.RequestID != "R1" || it.CommitmentID != c1 || it.PartID != "A" ||
		it.Quantity != 4 || it.Used != 1 || it.Remaining != 3 ||
		it.ExpiresAt != hour(24) || it.Status != StatusActive {
		t.Fatalf("item fields mismatch: %+v", it)
	}

	if _, err := s.QueryPart("GHOST", hour(1)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown part query: %v", err)
	}
}

// ---------- 并发 ----------

func TestConcurrentReserveNoOversell(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))

	const n = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	var successes int64
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Reserve(fmt.Sprintf("S-%d", i), "R1", "A", 1, hour(48), hour(1))
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 10 {
		t.Fatalf("exactly 10 reservations should succeed (stock 10), got %d", successes)
	}
	v, _ := s.QueryPart("A", hour(1))
	if v.ActiveHeld != 10 || v.Available != 0 || v.PhysicalRemaining != 10 {
		t.Fatalf("invariant broken: %+v", v)
	}
}

func TestConcurrentUseNoOverdeduct(t *testing.T) {
	s := setup(t)
	must(t, s.RegisterRequest("R1", "P1", "F1"))
	cid, _ := s.Reserve("S1", "R1", "A", 10, hour(48), hour(1))

	const n = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	var successes int64
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			if err := s.Use(fmt.Sprintf("U-%d", i), cid, 1, hour(1)); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 10 {
		t.Fatalf("exactly 10 uses should succeed, got %d", successes)
	}
	v, _ := s.QueryPart("A", hour(1))
	if v.PhysicalRemaining != 0 || v.ActiveHeld != 0 || v.Available != 0 {
		t.Fatalf("invariant broken: %+v", v)
	}
}
