package warranty

import (
	"errors"
	"testing"
	"time"
)

// expiryStore 构造一个库存为 stock 的备件仓库。
func expiryStore(t *testing.T, stock int) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", stock); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	return s
}

// TestExpiryConfirmedByReserveStaysExpiredWhenClockMovesBack 对应用户给出的
// 典型回退场景：实物库存五件，承诺甲预留五件并在当天十二点到期；十二点零一分
// 为合格请求创建承诺乙又预留五件；此后用十一点五十九分查询或使用，甲都不得
// 重新占用、不得继续扣减。
func TestExpiryConfirmedByReserveStaysExpiredWhenClockMovesBack(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	beforeNoon := noon.Add(-time.Minute)
	afterNoon := noon.Add(time.Minute)

	// 承诺甲预留五件，当天十二点到期。
	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve a: %v", err)
	}
	// 十二点零一分：甲已到期释放，乙又预留五件。
	if _, err := s.Reserve("b", "r1", "part1", 5, noon.Add(24*time.Hour), afterNoon); err != nil {
		t.Fatalf("reserve b: %v", err)
	}
	// 回退到十一点五十九分查询：甲不得重新占用，可承诺数量为零。
	st, err := s.PartStatus("part1", beforeNoon)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.ActiveOccupied != 5 || st.Committable != 0 {
		t.Fatalf("after rollback: occupied=%d committable=%d, want 5/0", st.ActiveOccupied, st.Committable)
	}
	byID := map[string]CommitmentDetail{}
	for _, d := range st.Details {
		byID[d.CommitmentID] = d
	}
	if byID["a"].Status != CommitmentExpired {
		t.Fatalf("a status = %q, want expired", byID["a"].Status)
	}
	if byID["b"].Status != CommitmentActive {
		t.Fatalf("b status = %q, want active", byID["b"].Status)
	}
	// 尝试使用甲：新使用一律拒绝，且不改变乙的占用与实物数量。
	if _, err := s.Use("u-a", "a", 1, beforeNoon); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use a after confirmed expiry: got %v", err)
	}
	st2, _ := s.PartStatus("part1", beforeNoon)
	if st2.ActiveOccupied != 5 || st2.PhysicalRemaining != 5 {
		t.Fatalf("use a changed state: occupied=%d physical=%d, want 5/5", st2.ActiveOccupied, st2.PhysicalRemaining)
	}
}

// TestExpiredStatusPersistsOnCommitment 验证到期确认后，从仓库取回的当前承诺
// 本身带有到期标记，按较早时刻查看状态也应保持到期。
func TestExpiredStatusPersistsOnCommitment(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	beforeNoon := noon.Add(-time.Minute)
	afterNoon := noon.Add(time.Minute)

	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.PartStatus("part1", afterNoon); err != nil {
		t.Fatalf("part status: %v", err)
	}
	c, err := s.Commitment("a")
	if err != nil {
		t.Fatalf("commitment: %v", err)
	}
	if !c.Expired {
		t.Fatal("expected Expired flag to be persisted on the commitment")
	}
	if got := c.Status(beforeNoon); got != CommitmentExpired {
		t.Fatalf("status at earlier time = %q, want expired", got)
	}
}

// TestPartStatusConfirmsExpiry 验证库存查询在当前时刻达到到期时刻时确认到期，
// 且确认不可撤销。
func TestPartStatusConfirmsExpiry(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	beforeNoon := noon.Add(-time.Minute)

	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 到期时刻之前查询：仍有效。
	st, err := s.PartStatus("part1", beforeNoon)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.ActiveOccupied != 5 || st.Committable != 0 {
		t.Fatalf("before expiry: occupied=%d committable=%d, want 5/0", st.ActiveOccupied, st.Committable)
	}
	// 到期时刻查询：确认到期。
	if _, err := s.PartStatus("part1", noon); err != nil {
		t.Fatalf("part status at expiry: %v", err)
	}
	c, _ := s.Commitment("a")
	if !c.Expired {
		t.Fatal("part status at expiry did not confirm expiry")
	}
	// 回退时刻查询：保持到期，占用为零。
	st2, _ := s.PartStatus("part1", beforeNoon)
	if st2.ActiveOccupied != 0 || st2.Committable != 5 {
		t.Fatalf("after rollback: occupied=%d committable=%d, want 0/5", st2.ActiveOccupied, st2.Committable)
	}
}

// TestRequestViewConfirmsExpiry 验证请求明细查询在当前时刻达到到期时刻时
// 确认该请求的承诺到期，且确认不可撤销。
func TestRequestViewConfirmsExpiry(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	beforeNoon := noon.Add(-time.Minute)
	afterNoon := noon.Add(time.Minute)

	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 到期时刻之后的请求明细查询：确认到期。
	view, err := s.RequestView("r1", afterNoon)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if len(view.Commitments) != 1 || view.Commitments[0].Status != CommitmentExpired {
		t.Fatalf("view at expiry: %+v", view.Commitments)
	}
	c, _ := s.Commitment("a")
	if !c.Expired {
		t.Fatal("request view did not confirm expiry")
	}
	// 回退时刻查询：保持到期。
	view2, _ := s.RequestView("r1", beforeNoon)
	if len(view2.Commitments) != 1 || view2.Commitments[0].Status != CommitmentExpired {
		t.Fatalf("view after rollback: %+v", view2.Commitments)
	}
}

// TestReserveConfirmsExpiryDuringStockAccounting 验证新预留的库存核算会确认
// 到期承诺；即使本次预留因库存不足失败，已确认的到期也不回退。
func TestReserveConfirmsExpiryDuringStockAccounting(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	beforeNoon := noon.Add(-time.Minute)
	afterNoon := noon.Add(time.Minute)

	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve a: %v", err)
	}
	// 数量 6 超过可承诺量（5）→ 库存不足失败；但库存核算已确认甲到期。
	_, err := s.Reserve("b", "r1", "part1", 6, noon.Add(24*time.Hour), afterNoon)
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve b: got %v, want ErrInsufficientStock", err)
	}
	c, _ := s.Commitment("a")
	if !c.Expired {
		t.Fatal("reserve stock accounting did not confirm expiry")
	}
	// 回退时刻查询：甲保持到期，可承诺量恢复为 5。
	st, _ := s.PartStatus("part1", beforeNoon)
	if st.ActiveOccupied != 0 || st.Committable != 5 {
		t.Fatalf("after rollback: occupied=%d committable=%d, want 0/5", st.ActiveOccupied, st.Committable)
	}
}

// TestRetriesDoNotConfirmExpiry 验证 Reserve/Use 的原样重试只取回旧结果，
// 不因本次时刻确认任何承诺到期。
func TestRetriesDoNotConfirmExpiry(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	afterNoon := noon.Add(time.Minute)

	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve a: %v", err)
	}
	// 原样重试（即使时刻晚于到期）只取回旧结果，不确认到期。
	if _, err := s.Reserve("a", "r1", "part1", 5, noon, afterNoon); err != nil {
		t.Fatalf("reserve retry: %v", err)
	}
	c, _ := s.Commitment("a")
	if c.Expired {
		t.Fatal("idempotent reserve retry confirmed expiry")
	}
	// 使用重试同样不确认到期。
	if _, err := s.Use("u1", "a", 1, morning); err != nil {
		t.Fatalf("use: %v", err)
	}
	if _, err := s.Use("u1", "a", 1, afterNoon); err != nil {
		t.Fatalf("use retry: %v", err)
	}
	c2, _ := s.Commitment("a")
	if c2.Expired {
		t.Fatal("idempotent use retry confirmed expiry")
	}
	// 新使用判断在到期时刻确认到期。
	if _, err := s.Use("u2", "a", 1, afterNoon); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new use after expiry: got %v", err)
	}
	c3, _ := s.Commitment("a")
	if !c3.Expired {
		t.Fatal("new use judgment did not confirm expiry")
	}
}

// TestEarlyFailuresDoNotConfirmExpiry 验证空编号、非正数量等参数无效，或
// 引用对象不存在而提前失败的调用，不能用传入的时刻确认承诺到期。
func TestEarlyFailuresDoNotConfirmExpiry(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	afterNoon := noon.Add(time.Minute)
	nextDay := noon.Add(24 * time.Hour)

	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve a: %v", err)
	}
	// 无效参数：空编号、非正数量、到期时刻未晚于当前时刻。
	if _, err := s.Reserve("", "r1", "part1", 1, nextDay, afterNoon); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty commit id: got %v", err)
	}
	if _, err := s.Reserve("b", "r1", "part1", 0, nextDay, afterNoon); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero quantity: got %v", err)
	}
	if _, err := s.Reserve("b", "r1", "part1", 1, afterNoon, afterNoon); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("expiry equal now: got %v", err)
	}
	// 引用对象不存在。
	if _, err := s.Reserve("b", "rMissing", "part1", 1, nextDay, afterNoon); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v", err)
	}
	if _, err := s.Reserve("b", "r1", "partMissing", 1, nextDay, afterNoon); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown part: got %v", err)
	}
	c, _ := s.Commitment("a")
	if c.Expired {
		t.Fatal("early failure confirmed expiry")
	}
}

// TestExpiryKeepsUsedAndStock 验证到期只释放未用占用：不增加实物库存，
// 不抹去已用数量，原数量、已用数量、未用数量和到期时刻都保留。
func TestExpiryKeepsUsedAndStock(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	afterNoon := noon.Add(time.Minute)

	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 到期前已用三件。
	if _, err := s.Use("u1", "a", 3, morning); err != nil {
		t.Fatalf("use: %v", err)
	}
	// 到期释放：只释放未用的两件，不增加实物库存，不抹去已用数量。
	if _, err := s.PartStatus("part1", afterNoon); err != nil {
		t.Fatalf("part status: %v", err)
	}
	st, _ := s.PartStatus("part1", afterNoon)
	if st.PhysicalRemaining != 2 {
		t.Fatalf("physical remaining = %d, want 2 (5-3)", st.PhysicalRemaining)
	}
	if st.ActiveOccupied != 0 || st.Committable != 2 {
		t.Fatalf("occupied=%d committable=%d, want 0/2", st.ActiveOccupied, st.Committable)
	}
	if len(st.Details) != 1 {
		t.Fatalf("details = %d, want 1", len(st.Details))
	}
	d := st.Details[0]
	if d.OriginalQuantity != 5 || d.UsedQuantity != 3 || d.RemainingQuantity != 2 || !d.Expiry.Equal(noon) {
		t.Fatalf("detail after expiry: %+v", d)
	}
}

// TestCanceledStaysCanceledAtEarlierTime 验证取消是持久状态：到期时刻之后
// 取消的承诺，回退时刻仍显示 canceled，不恢复占用；重复取消不再释放数量。
func TestCanceledStaysCanceledAtEarlierTime(t *testing.T) {
	s := expiryStore(t, 5)
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	beforeNoon := noon.Add(-time.Minute)
	afterNoon := noon.Add(time.Minute)

	if _, err := s.Reserve("a", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 到期时刻之后取消：取消是持久状态，回退时刻仍显示 canceled。
	if _, err := s.Cancel("a", afterNoon); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c, _ := s.Commitment("a")
	if c.Status(beforeNoon) != CommitmentCanceled {
		t.Fatalf("status at earlier time = %q, want canceled", c.Status(beforeNoon))
	}
	// 取消不占用。
	st, _ := s.PartStatus("part1", beforeNoon)
	if st.ActiveOccupied != 0 || st.Committable != 5 {
		t.Fatalf("canceled: occupied=%d committable=%d, want 0/5", st.ActiveOccupied, st.Committable)
	}
	// 重复取消不释放数量（也不报错）。
	if _, err := s.Cancel("a", beforeNoon); err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	st2, _ := s.PartStatus("part1", beforeNoon)
	if st2.ActiveOccupied != 0 || st2.Committable != 5 {
		t.Fatalf("after repeat cancel: occupied=%d committable=%d", st2.ActiveOccupied, st2.Committable)
	}
}

// TestRequestViewConfirmsExpiryAcrossParts 验证明细查询按请求范围确认：
// 同一请求在不同备件上的到期承诺都会被确认，且回退时刻查询任一备件都保持到期。
func TestRequestViewConfirmsExpiryAcrossParts(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 5); err != nil {
		t.Fatalf("register part1: %v", err)
	}
	if err := s.RegisterPart("part2", 5); err != nil {
		t.Fatalf("register part2: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "F"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	morning := t0.Add(10 * day)
	noon := morning.Add(2 * time.Hour)
	afterNoon := noon.Add(time.Minute)

	// 同一请求在两个备件上各有一笔到期承诺。
	if _, err := s.Reserve("a1", "r1", "part1", 5, noon, morning); err != nil {
		t.Fatalf("reserve a1: %v", err)
	}
	if _, err := s.Reserve("a2", "r1", "part2", 5, noon, morning); err != nil {
		t.Fatalf("reserve a2: %v", err)
	}
	// 请求明细查询（到期时刻之后）确认该请求的全部到期承诺，跨备件生效。
	if _, err := s.RequestView("r1", afterNoon); err != nil {
		t.Fatalf("request view: %v", err)
	}
	for _, id := range []string{"a1", "a2"} {
		c, _ := s.Commitment(id)
		if !c.Expired {
			t.Fatalf("%s was not confirmed expired by request view", id)
		}
	}
	// 回退时刻查询两个备件：占用均为零。
	for _, partID := range []string{"part1", "part2"} {
		st, _ := s.PartStatus(partID, morning)
		if st.ActiveOccupied != 0 || st.Committable != 5 {
			t.Fatalf("%s after rollback: occupied=%d committable=%d, want 0/5", partID, st.ActiveOccupied, st.Committable)
		}
	}
}
