package warranty

import (
	"errors"
	"testing"
	"time"
)

// useConflictStore 构造两笔承诺的场景：备件十件，c1 承诺五件、c2 承诺三件，
// 两笔承诺均未取消、未确认到期。另建一笔已取消与一笔已确认到期的承诺，
// 用于验证冲突优先于“承诺已关闭”。
func useConflictStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	return s
}

// assertUseConflictStateLocked 核对冲突失败后的完整状态：实物八件、有效占用
// 六件（c1 未用三 + c2 未用三）、可再承诺两件，c1 仍已用两件，c2 未使用。
func assertUseConflictState(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	st, err := s.PartStatus("part1", now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 6 || st.Committable != 2 {
		t.Fatalf("stock after conflict = %+v, want phys=8 occupied=6 committable=2", st)
	}
	d1, ok := detailByID(st, "c1")
	if !ok || d1.UsedQuantity != 2 || d1.RemainingQuantity != 3 {
		t.Fatalf("c1 detail after conflict = %+v ok=%v, want used=2 remaining=3", d1, ok)
	}
	d2, ok := detailByID(st, "c2")
	if !ok || d2.UsedQuantity != 0 || d2.RemainingQuantity != 3 {
		t.Fatalf("c2 detail after conflict = %+v ok=%v, want used=0 remaining=3", d2, ok)
	}
	c1, _ := s.Commitment("c1")
	c2, _ := s.Commitment("c2")
	if c1.Used != 2 || c1.Canceled || c1.Expired {
		t.Fatalf("c1 mutated by conflict: %+v", c1)
	}
	if c2.Used != 0 || c2.Canceled || c2.Expired {
		t.Fatalf("c2 mutated by conflict: %+v", c2)
	}
}

// TestUseConflictPrecedenceOverInvalidContent 验证成功使用编号被改内容再次
// 提交时一律按 ErrConflict 处理：即使新内容本身无效（承诺编号为空或不存在、
// 数量为零/负数/超过承诺余量），或新承诺已取消、已确认到期，也不能返回普通
// 参数错误、对象不存在、使用超量或承诺已关闭；任何失败都不得用新内容替换
// 原来的成功使用记录，也不得改变库存与承诺数量。
func TestUseConflictPrecedenceOverInvalidContent(t *testing.T) {
	s := useConflictStore(t)
	first, err := s.Use("u1", "c1", 2, nowOK)
	if err != nil {
		t.Fatalf("use u1: %v", err)
	}
	if first != (Usage{ID: "u1", CommitmentID: "c1", Quantity: 2}) {
		t.Fatalf("first usage = %+v", first)
	}

	// 另建一笔已取消承诺与一笔已确认到期的承诺作为改指向目标。
	if _, err := s.Reserve("cCanceled", "r1", "part1", 1, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cCanceled: %v", err)
	}
	if _, err := s.Cancel("cCanceled", nowOK); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	earlyExpiry := t0.Add(15 * day)
	if _, err := s.Reserve("cExpired", "r1", "part1", 1, earlyExpiry, nowOK); err != nil {
		t.Fatalf("reserve cExpired: %v", err)
	}
	// 借库存查询确认 cExpired 到期；该时刻早于 expiryOK，不影响 c1/c2。
	if _, err := s.PartStatus("part1", earlyExpiry); err != nil {
		t.Fatalf("part status: %v", err)
	}

	cases := []struct {
		name     string
		commitID string
		quantity int
	}{
		{"empty commitment id", "", 2},
		{"unknown commitment", "cMissing", 2},
		{"zero quantity", "c1", 0},
		{"negative quantity", "c1", -1},
		{"quantity exceeds remaining", "c1", 4}, // c1 未用仅三件
		{"quantity beyond original", "c1", 99},
		{"point at canceled commitment", "cCanceled", 1},
		{"point at expired commitment", "cExpired", 1},
		{"point at second commitment", "c2", 2},
		{"zero quantity on second commitment", "c2", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 同时变换本次提交时刻，证明当前时刻不属于绑定内容，冲突与时刻无关。
			_, err := s.Use("u1", tc.commitID, tc.quantity, expiryOK.Add(10*day))
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("Use(u1, %q, %d) = %v, want ErrConflict", tc.commitID, tc.quantity, err)
			}
		})
	}

	// 全部冲突后：成功记录仍是首次内容，库存与承诺数量保持原值。
	got, err := s.Use("u1", "c1", 2, nowOK.Add(day))
	if err != nil {
		t.Fatalf("original retry: %v", err)
	}
	if got != first {
		t.Fatalf("stored usage replaced: got %+v, want first %+v", got, first)
	}
	assertUseConflictState(t, s, nowOK)

	// 改指向的目标承诺也没有被牵动。
	cc, _ := s.Commitment("cCanceled")
	if !cc.Canceled || cc.Used != 0 {
		t.Fatalf("canceled commitment touched by conflict: %+v", cc)
	}
	ce, _ := s.Commitment("cExpired")
	if !ce.Expired || ce.Used != 0 {
		t.Fatalf("expired commitment touched by conflict: %+v", ce)
	}
}

// TestUseConflictCausesNoDeductionOrRelease 对应用户给出的数量例子：初始十件，
// 承诺五件与三件，第一笔已成功使用两件，实物剩八件、有效占用六件、可再承诺
// 两件。复用该使用编号改数量或改指向第二笔承诺均失败，失败后这些数量保持
// 原值；冲突期间也没有任何扣减或释放，可再承诺量仍是两件。
func TestUseConflictCausesNoDeductionOrRelease(t *testing.T) {
	s := useConflictStore(t)
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use u1: %v", err)
	}

	// 改数量（零、负、超过余量、合法但不同的数量）。
	for _, q := range []int{0, -3, 3, 4} {
		if _, err := s.Use("u1", "c1", q, nowOK); !errors.Is(err, ErrConflict) {
			t.Fatalf("Use(u1, c1, %d) = %v, want ErrConflict", q, err)
		}
	}
	// 改指向第二笔承诺（数量合法与否都一样）。
	for _, q := range []int{2, 0, 4} {
		if _, err := s.Use("u1", "c2", q, nowOK); !errors.Is(err, ErrConflict) {
			t.Fatalf("Use(u1, c2, %d) = %v, want ErrConflict", q, err)
		}
	}
	assertUseConflictState(t, s, nowOK)

	// 可再承诺量确为两件：新承诺两件成功，三件则库存不足。
	if _, err := s.Reserve("c3", "r1", "part1", 2, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve remaining 2: %v", err)
	}
	if _, err := s.Reserve("c4", "r1", "part1", 3, expiryOK.Add(time.Hour), nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve 3 after fill: got %v, want ErrInsufficientStock", err)
	}
}

// TestUseConflictAtOrAfterExpiriesDoesNotConfirm 保护重要边界：两笔承诺此前
// 都未确认到期，冲突提交的当前时刻却已达到两笔承诺的到期时刻。冲突处理不得
// 借该时刻确认任一笔承诺到期；之后按到期前的时刻查看，两笔仍显示有效、占用
// 保持上述数量，按较早时刻提交合法的新使用也仍可成功。
func TestUseConflictAtOrAfterExpiriesDoesNotConfirm(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	// 时刻都取第 10~25 天，资格合格。
	expiry1 := t0.Add(20 * day)
	expiry2 := t0.Add(21 * day)
	now := t0.Add(10 * day)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiry1, now); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r1", "part1", 3, expiry2, now); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, now); err != nil {
		t.Fatalf("use u1: %v", err)
	}

	// 当前时刻已达到两笔承诺的到期时刻：改数量与改指向的提交都只报冲突。
	conflictNow := t0.Add(22 * day)
	if _, err := s.Use("u1", "c1", 3, conflictNow); !errors.Is(err, ErrConflict) {
		t.Fatalf("quantity conflict at/after expiries: got %v, want ErrConflict", err)
	}
	if _, err := s.Use("u1", "c2", 2, conflictNow); !errors.Is(err, ErrConflict) {
		t.Fatalf("commitment conflict at/after expiries: got %v, want ErrConflict", err)
	}

	// 两笔承诺均未被确认到期。
	c1, _ := s.Commitment("c1")
	c2, _ := s.Commitment("c2")
	if c1.Expired || c2.Expired || c1.Canceled || c2.Canceled {
		t.Fatalf("conflict confirmed expiry: c1=%+v c2=%+v", c1, c2)
	}

	// 回到两笔到期时刻之前查看：仍有效，实物八件、占用六件、可承诺两件。
	before := expiry1.Add(-time.Second)
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status before expiries: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 6 || st.Committable != 2 {
		t.Fatalf("stock = %+v, want phys=8 occupied=6 committable=2", st)
	}
	d1, _ := detailByID(st, "c1")
	d2, _ := detailByID(st, "c2")
	if d1.Status != CommitmentActive || d1.UsedQuantity != 2 || d1.RemainingQuantity != 3 {
		t.Fatalf("c1 = %+v, want active used=2 remaining=3", d1)
	}
	if d2.Status != CommitmentActive || d2.UsedQuantity != 0 || d2.RemainingQuantity != 3 {
		t.Fatalf("c2 = %+v, want active used=0 remaining=3", d2)
	}

	// 按较早时刻提交合法的新使用仍成功：c1 再用一件，实物七件、占用五件。
	if _, err := s.Use("u2", "c1", 1, before); err != nil {
		t.Fatalf("legal use before expiries after conflict: %v", err)
	}
	st2, _ := s.PartStatus("part1", before)
	if st2.PhysicalRemaining != 7 || st2.ActiveOccupied != 5 || st2.Committable != 2 {
		t.Fatalf("stock after legal use = %+v, want phys=7 occupied=5 committable=2", st2)
	}
}

// TestUseOriginalRetryAfterConflictsReturnsFirstResult 验证冲突之后再以首次
// 成功时的承诺与数量提交原使用编号，取回最初的完整使用结果，不再次扣库存；
// 仅本次提交时刻不同不构成冲突。
func TestUseOriginalRetryAfterConflictsReturnsFirstResult(t *testing.T) {
	s := useConflictStore(t)
	first, err := s.Use("u1", "c1", 2, nowOK)
	if err != nil {
		t.Fatalf("use u1: %v", err)
	}
	// 若干改内容的冲突提交。
	if _, err := s.Use("u1", "c1", 3, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("quantity conflict: %v", err)
	}
	if _, err := s.Use("u1", "c2", 2, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("commitment conflict: %v", err)
	}
	if _, err := s.Use("u1", "", 0, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid-content conflict: %v", err)
	}

	// 原承诺、原数量、不同当前时刻：取回首次结果。
	got, err := s.Use("u1", "c1", 2, nowOK.Add(5*day))
	if err != nil {
		t.Fatalf("original retry: %v", err)
	}
	if got != first {
		t.Fatalf("retry = %+v, want first %+v", got, first)
	}
	// 没有再次扣减：c1 仍已用两件，实物仍八件、占用六件。
	assertUseConflictState(t, s, nowOK)
}

// TestUseOriginalRetryDoesNotReopenClosedCommitment 验证承诺已被正常取消或
// 确认到期后，原样取回旧使用结果也不能重新开放承诺：不恢复占用、不补回库存，
// 新使用编号仍一律被拒。
func TestUseOriginalRetryDoesNotReopenClosedCommitment(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		s := useConflictStore(t)
		first, err := s.Use("u1", "c1", 2, nowOK)
		if err != nil {
			t.Fatalf("use u1: %v", err)
		}
		if _, err := s.Cancel("c1", nowOK); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		// 原样取回旧结果。
		got, err := s.Use("u1", "c1", 2, nowOK.Add(day))
		if err != nil || got != first {
			t.Fatalf("retry on canceled: %+v err %v", got, err)
		}
		c, _ := s.Commitment("c1")
		if !c.Canceled || c.Expired || c.Used != 2 {
			t.Fatalf("commitment reopened by retry: %+v", c)
		}
		st, _ := s.PartStatus("part1", nowOK)
		if st.PhysicalRemaining != 8 || st.ActiveOccupied != 3 || st.Committable != 5 {
			t.Fatalf("stock after retry = %+v, want phys=8 occupied=3(c2) committable=5", st)
		}
		// 承诺没有重新开放。
		if _, err := s.Use("uNew", "c1", 1, nowOK); !errors.Is(err, ErrCommitmentClosed) {
			t.Fatalf("new use on reopened commitment: got %v, want ErrCommitmentClosed", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		s := useConflictStore(t)
		first, err := s.Use("u1", "c1", 2, nowOK)
		if err != nil {
			t.Fatalf("use u1: %v", err)
		}
		// 借库存查询确认两笔承诺到期，余量永久释放。
		after := expiryOK.Add(10 * day)
		if _, err := s.PartStatus("part1", after); err != nil {
			t.Fatalf("part status: %v", err)
		}
		// 即使按到期前的时刻原样取回，旧结果不变，承诺也不重新开放。
		got, err := s.Use("u1", "c1", 2, expiryOK.Add(-time.Second))
		if err != nil || got != first {
			t.Fatalf("retry on expired: %+v err %v", got, err)
		}
		c, _ := s.Commitment("c1")
		if !c.Expired || c.Canceled || c.Used != 2 {
			t.Fatalf("expired commitment reopened by retry: %+v", c)
		}
		st, _ := s.PartStatus("part1", expiryOK.Add(-time.Second))
		if st.PhysicalRemaining != 8 || st.ActiveOccupied != 0 || st.Committable != 8 {
			t.Fatalf("stock after retry = %+v, want phys=8 occupied=0 committable=8", st)
		}
		if _, err := s.Use("uNew", "c1", 1, expiryOK.Add(-time.Second)); !errors.Is(err, ErrCommitmentClosed) {
			t.Fatalf("new use on reopened expired commitment: got %v, want ErrCommitmentClosed", err)
		}
	})
}

// TestUnsuccessfulUsageIDZeroThenValid 是对照：尚未成功使用的新编号提交零数量
// 仍返回 ErrInvalidParam（而不是冲突），编号不被占用，修正为合法数量后可以
// 正常使用且只扣减一次。
func TestUnsuccessfulUsageIDZeroThenValid(t *testing.T) {
	s := useConflictStore(t)
	if _, err := s.Use("uNew", "c1", 0, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero quantity on fresh id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Use("uNew", "c1", -2, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("negative quantity on fresh id: got %v, want ErrInvalidParam", err)
	}
	// 失败不占用编号、不扣减。
	c0, _ := s.Commitment("c1")
	if c0.Used != 0 {
		t.Fatalf("invalid use deducted: used=%d", c0.Used)
	}
	u, err := s.Use("uNew", "c1", 2, nowOK)
	if err != nil {
		t.Fatalf("valid use after invalid: %v", err)
	}
	if u != (Usage{ID: "uNew", CommitmentID: "c1", Quantity: 2}) {
		t.Fatalf("usage = %+v", u)
	}
	c1, _ := s.Commitment("c1")
	if c1.Used != 2 {
		t.Fatalf("commitment used = %d, want 2", c1.Used)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 8 {
		t.Fatalf("physical = %d, want 8", st.PhysicalRemaining)
	}
	// 成功后该编号再改内容即按冲突处理。
	if _, err := s.Use("uNew", "c1", 3, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict after success: got %v, want ErrConflict", err)
	}
}
