package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障成功使用编号的复用规则：一次成功使用把使用编号与当时的承诺
// 编号、数量绑定（当前时刻不属于绑定内容）；之后复用该编号，只要承诺或数量
// 有一项改变即返回 ErrConflict——即使新内容本身非法（空承诺编号、承诺不存在、
// 数量为零/负数/超过余量），或新指向的承诺已取消、已确认到期。冲突不扣减
// 实物库存、不改变任何承诺的已用数量、不释放占用、不借本次时刻确认原承诺或
// 新承诺到期，也不会用新内容覆盖原成功记录。冲突之后以首次成功时的承诺与
// 数量原样重试，始终取回最初的完整使用结果；承诺即使已被正常取消或确认到期，
// 取回旧结果也不重新开放。这些测试只沿用登记、预留、使用、取消和查询的既有
// 公开行为。

// usedTwiceStore 构造用户给定的主例：备件 part1 初始十件，c1 承诺五件、c2
// 承诺三件（同一到期时刻 expiryOK），u1 已在 c1 成功使用两件。此时实物剩余
// 八件，有效占用六件（c1 未用三件 + c2 未用三件），可再承诺两件。返回首次
// 使用结果供比对。
func usedTwiceStore(t *testing.T) (*Store, Usage) {
	t.Helper()
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	first, err := s.Use("u1", "c1", 2, nowOK)
	if err != nil {
		t.Fatalf("use u1: %v", err)
	}
	return s, first
}

// assertTwiceState 断言主例数量保持不变：实物八件、有效占用六件、可再承诺
// 两件；c1 已用两件、未用三件且有效，c2 已用零件、未用三件且有效。查询时刻
// 必须早于两笔承诺的到期时刻（调用方需保证），以免查询本身确认到期。
func assertTwiceState(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	st, err := s.PartStatus("part1", now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 6 || st.Committable != 2 {
		t.Fatalf("stock = phys %d / occupied %d / committable %d, want 8/6/2",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	d1, ok1 := detailByID(st, "c1")
	d2, ok2 := detailByID(st, "c2")
	if !ok1 || !ok2 {
		t.Fatalf("commitment detail missing: c1=%v c2=%v", ok1, ok2)
	}
	if d1.Status != CommitmentActive || d1.UsedQuantity != 2 || d1.RemainingQuantity != 3 {
		t.Fatalf("c1 detail = %+v, want active/used=2/remaining=3", d1)
	}
	if d2.Status != CommitmentActive || d2.UsedQuantity != 0 || d2.RemainingQuantity != 3 {
		t.Fatalf("c2 detail = %+v, want active/used=0/remaining=3", d2)
	}
}

// TestUsageReuseBindsCommitAndQuantityNotTime 验证成功使用只与承诺编号、数量
// 绑定：仅本次提交时刻改变不构成冲突，原样取回首次结果；改数量或改承诺编号
// 任一项即冲突，且冲突后原记录与全部数量不变。
func TestUsageReuseBindsCommitAndQuantityNotTime(t *testing.T) {
	s, first := usedTwiceStore(t)

	// 仅本次当前时刻不同：不是冲突，原样取回首次使用结果，不再次扣减。
	later := nowOK.Add(5 * day)
	got, err := s.Use("u1", "c1", 2, later)
	if err != nil || got != first {
		t.Fatalf("same content at later time = %+v, err %v; want first result %+v", got, err, first)
	}
	assertTwiceState(t, s, later)

	// 改数量：冲突。
	if u, err := s.Use("u1", "c1", 1, later); !errors.Is(err, ErrConflict) || u != (Usage{}) {
		t.Fatalf("changed quantity: usage=%+v err=%v, want ErrConflict with empty result", u, err)
	}
	// 改承诺编号：冲突。
	if u, err := s.Use("u1", "c2", 2, later); !errors.Is(err, ErrConflict) || u != (Usage{}) {
		t.Fatalf("changed commitment: usage=%+v err=%v, want ErrConflict with empty result", u, err)
	}

	// 冲突不扣减、不释放，原成功使用记录不被新内容替换。
	assertTwiceState(t, s, later)
	if again, err := s.Use("u1", "c1", 2, later); err != nil || again != first {
		t.Fatalf("original content after conflicts = %+v, err %v; want first result %+v", again, err, first)
	}
}

// TestUsageConflictPrecedenceOverInvalidContent 验证复用成功编号时，编号冲突
// 优先于一切新内容自身的校验：空承诺编号、承诺不存在、数量为零/负数/超过
// 承诺余量，都返回 ErrConflict，而不是普通参数错误、对象不存在或使用超量；
// 一笔本身完全合法的新使用内容（数量恰为余量、或指向 c2 的合法数量）同样按
// 冲突处理。所有失败都不得改变数量或替换原成功记录。
func TestUsageConflictPrecedenceOverInvalidContent(t *testing.T) {
	s, first := usedTwiceStore(t)
	submitAt := nowOK.Add(day)

	cases := []struct {
		name     string
		commitID string
		quantity int
	}{
		{"empty commitment id", "", 2},
		{"unknown commitment", "cMissing", 2},
		{"zero quantity", "c1", 0},
		{"negative quantity", "c1", -4},
		{"quantity over c1 unused", "c1", 4}, // c1 未用仅三件
		{"quantity equal to c1 unused is still a change", "c1", 3},
		{"point to c2 with same quantity", "c2", 2},
		{"point to c2 with quantity over its unused", "c2", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := s.Use("u1", tc.commitID, tc.quantity, submitAt)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("got %v, want ErrConflict", err)
			}
			if u != (Usage{}) {
				t.Fatalf("conflict returned non-empty usage: %+v", u)
			}
		})
	}

	// 全部冲突后：实物八、占用六、可承诺二，两笔承诺的已用数量与状态不变。
	assertTwiceState(t, s, submitAt)
	c1, _ := s.Commitment("c1")
	c2, _ := s.Commitment("c2")
	if c1.Used != 2 || c2.Used != 0 || c1.Canceled || c2.Canceled || c1.Expired || c2.Expired {
		t.Fatalf("conflicts mutated commitments: c1=%+v c2=%+v", c1, c2)
	}
	// 原记录未被任何新内容替换：仍取回首次的 {u1 c1 2}。
	if got, err := s.Use("u1", "c1", 2, nowOK.Add(2*day)); err != nil || got != first {
		t.Fatalf("original record replaced: got %+v err %v, want %+v", got, err, first)
	}
	// 可再承诺两件仍真实可用。
	if _, err := s.Reserve("c3", "r1", "part1", 2, expiryOK, submitAt); err != nil {
		t.Fatalf("reserve remaining committable 2 after conflicts: %v", err)
	}
}

// TestUsageConflictPrecedenceOverClosedTarget 验证复用成功编号改指向另一笔
// 已取消或已确认到期的承诺时，仍按编号冲突处理，而不是 ErrCommitmentClosed；
// 冲突不改变取消/到期结果、不确认到期、不扣减或释放任何数量。
func TestUsageConflictPrecedenceOverClosedTarget(t *testing.T) {
	t.Run("target canceled", func(t *testing.T) {
		s, _ := usedTwiceStore(t)
		if _, err := s.Cancel("c2", nowOK); err != nil {
			t.Fatalf("cancel c2: %v", err)
		}
		// 复用 u1 改指向已取消的 c2：编号冲突优先。
		if _, err := s.Use("u1", "c2", 2, nowOK.Add(day)); !errors.Is(err, ErrConflict) {
			t.Fatalf("point to canceled commitment: got %v, want ErrConflict", err)
		}
		c2, _ := s.Commitment("c2")
		c1, _ := s.Commitment("c1")
		if !c2.Canceled || c2.Expired || c2.Used != 0 {
			t.Fatalf("canceled c2 changed by conflict: %+v", c2)
		}
		if c1.Used != 2 || c1.Canceled || c1.Expired {
			t.Fatalf("c1 changed by conflict: %+v", c1)
		}
		// 取消释放 c2 的三件占用：实物仍八件，有效占用仅 c1 的三件。
		st, _ := s.PartStatus("part1", nowOK.Add(day))
		if st.PhysicalRemaining != 8 || st.ActiveOccupied != 3 || st.Committable != 5 {
			t.Fatalf("stock after conflict = %+v, want phys=8 occupied=3 committable=5", st)
		}
		if d, _ := detailByID(st, "c2"); d.Status != CommitmentCanceled {
			t.Fatalf("c2 status = %q, want canceled", d.Status)
		}
	})

	t.Run("target confirmed expired", func(t *testing.T) {
		s := newStore(t)
		// c2 使用更早的到期时刻，先于 c1 到期并已被正常确认。
		c2Expiry := nowOK.Add(2 * day)
		if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
			t.Fatalf("reserve c1: %v", err)
		}
		if _, err := s.Reserve("c2", "r1", "part1", 3, c2Expiry, nowOK); err != nil {
			t.Fatalf("reserve c2: %v", err)
		}
		first, err := s.Use("u1", "c1", 2, nowOK)
		if err != nil {
			t.Fatalf("use u1: %v", err)
		}
		// 由库存查询正常确认 c2 到期，释放三件占用。
		if _, err := s.PartStatus("part1", c2Expiry); err != nil {
			t.Fatalf("confirm c2 expiry via query: %v", err)
		}
		// 复用 u1 改指向已到期的 c2（提交当前时刻也达到其到期时刻）：仍是冲突。
		if u, err := s.Use("u1", "c2", 2, c2Expiry); !errors.Is(err, ErrConflict) || u != (Usage{}) {
			t.Fatalf("point to expired commitment: usage=%+v err=%v, want ErrConflict", u, err)
		}
		c2, _ := s.Commitment("c2")
		c1, _ := s.Commitment("c1")
		if !c2.Expired || c2.Canceled || c2.Used != 0 {
			t.Fatalf("expired c2 changed by conflict: %+v", c2)
		}
		if c1.Used != 2 || c1.Expired || c1.Canceled {
			t.Fatalf("c1 changed by conflict: %+v", c1)
		}
		// 原内容仍取回首次结果，不再次扣减；c1 到期时刻更晚，冲突未波及它。
		if got, err := s.Use("u1", "c1", 2, c2Expiry); err != nil || got != first {
			t.Fatalf("retry original content: got %+v err %v, want %+v", got, err, first)
		}
		// 回到 c2 到期前时刻查看：c2 到期确认不可逆，c1 仍有效、占用三件。
		st, _ := s.PartStatus("part1", c2Expiry.Add(-time.Second))
		if st.PhysicalRemaining != 8 || st.ActiveOccupied != 3 || st.Committable != 5 {
			t.Fatalf("stock = %+v, want phys=8 occupied=3 committable=5", st)
		}
		d1, _ := detailByID(st, "c1")
		d2, _ := detailByID(st, "c2")
		if d1.Status != CommitmentActive || d1.RemainingQuantity != 3 {
			t.Fatalf("c1 = %+v, want active/remaining=3", d1)
		}
		if d2.Status != CommitmentExpired || d2.RemainingQuantity != 3 {
			t.Fatalf("c2 = %+v, want expired/remaining=3", d2)
		}
	})
}

// TestUsageConflictAtExpiryConfirmsNeitherCommitment 保护重要边界：两笔承诺
// 此前都未确认到期，冲突提交的当前时刻却已达到两笔承诺的到期时刻——该提交
// 不能确认任一笔到期。之后按到期前时刻查看仍显示有效与原有占用，按较早时刻
// 提交合法的新使用也仍可成功。
func TestUsageConflictAtExpiryConfirmsNeitherCommitment(t *testing.T) {
	s, _ := usedTwiceStore(t)
	before := expiryOK.Add(-time.Minute)

	// 当前时刻恰好达到两笔承诺共同的到期时刻：改数量、改承诺各冲突一次。
	if _, err := s.Use("u1", "c1", 1, expiryOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed quantity at expiry: got %v, want ErrConflict", err)
	}
	if _, err := s.Use("u1", "c2", 2, expiryOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed commitment at expiry: got %v, want ErrConflict", err)
	}

	// 两笔承诺均未被确认到期，已用数量不变。
	c1, _ := s.Commitment("c1")
	c2, _ := s.Commitment("c2")
	if c1.Expired || c1.Used != 2 || c2.Expired || c2.Used != 0 {
		t.Fatalf("conflict at expiry confirmed/changed commitments: c1=%+v c2=%+v", c1, c2)
	}
	// 回到到期前时刻：库存视图仍是有效状态和八/六/二的数量。
	assertTwiceState(t, s, before)
	// 请求视图同样显示两笔均有效、原已用数量不变。
	view, err := s.RequestView("r1", before)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	want := map[string]struct {
		status             CommitmentStatus
		used, remainingQty int
	}{
		"c1": {CommitmentActive, 2, 3},
		"c2": {CommitmentActive, 0, 3},
	}
	if len(view.Commitments) != len(want) {
		t.Fatalf("commitments in view = %d, want 2", len(view.Commitments))
	}
	for _, d := range view.Commitments {
		w, ok := want[d.CommitmentID]
		if !ok {
			t.Fatalf("unexpected detail: %+v", d)
		}
		if d.Status != w.status || d.UsedQuantity != w.used || d.RemainingQuantity != w.remainingQty {
			t.Fatalf("%s detail = %+v, want status=%s used=%d remaining=%d",
				d.CommitmentID, d, w.status, w.used, w.remainingQty)
		}
	}

	// 到期前时刻提交一笔合法的新使用：仍成功并正常扣减（c1 已用三件、
	// 未用两件；实物七件、有效占用五件、可再承诺两件）。
	u, err := s.Use("u2", "c1", 1, before)
	if err != nil {
		t.Fatalf("new legal use at earlier time after conflict: %v", err)
	}
	if u.ID != "u2" || u.CommitmentID != "c1" || u.Quantity != 1 {
		t.Fatalf("new usage = %+v, want {u2 c1 1}", u)
	}
	c1b, _ := s.Commitment("c1")
	if c1b.Used != 3 || c1b.Expired {
		t.Fatalf("c1 after new use = %+v, want used=3 not expired", c1b)
	}
	st, _ := s.PartStatus("part1", before)
	if st.PhysicalRemaining != 7 || st.ActiveOccupied != 5 || st.Committable != 2 {
		t.Fatalf("stock after new use = %+v, want phys=7 occupied=5 committable=2", st)
	}
}

// TestUsageRetryAfterConflictReturnsFirstAndNeverReopens 验证冲突之后再以首次
// 成功时的承诺与数量提交，取回最初的完整使用结果、不再次扣库存；承诺此前已
// 被正常取消或确认到期的，取回旧结果也不重新开放承诺。
func TestUsageRetryAfterConflictReturnsFirstAndNeverReopens(t *testing.T) {
	t.Run("commitment still active", func(t *testing.T) {
		s, first := usedTwiceStore(t)
		// 先制造若干冲突（含零数量、指向 c2）。
		if _, err := s.Use("u1", "c1", 0, nowOK.Add(day)); !errors.Is(err, ErrConflict) {
			t.Fatalf("zero quantity conflict: %v", err)
		}
		if _, err := s.Use("u1", "c2", 2, nowOK.Add(day)); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed commitment conflict: %v", err)
		}
		got, err := s.Use("u1", "c1", 2, nowOK.Add(3*day))
		if err != nil || got != first {
			t.Fatalf("original retry = %+v, err %v; want first result %+v", got, err, first)
		}
		// 不再次扣库存、不释放，c1 仍是已用两件未用三件。
		assertTwiceState(t, s, nowOK.Add(3*day))
	})

	t.Run("commitment canceled beforehand", func(t *testing.T) {
		s, first := usedTwiceStore(t)
		if _, err := s.Cancel("c1", nowOK); err != nil {
			t.Fatalf("cancel c1: %v", err)
		}
		// 冲突与原样重试交错提交。
		if _, err := s.Use("u1", "c2", 5, nowOK.Add(day)); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflict pointing at c2: %v", err)
		}
		got, err := s.Use("u1", "c1", 2, nowOK.Add(2*day))
		if err != nil || got != first {
			t.Fatalf("retry after cancel = %+v, err %v; want first result %+v", got, err, first)
		}
		c1, _ := s.Commitment("c1")
		if !c1.Canceled || c1.Expired || c1.Used != 2 {
			t.Fatalf("c1 reopened or changed by retry: %+v", c1)
		}
		// 取回旧结果不重新开放承诺：新使用仍被拒。
		if _, err := s.Use("u9", "c1", 1, nowOK.Add(2*day)); !errors.Is(err, ErrCommitmentClosed) {
			t.Fatalf("new use after old-result retrieval: got %v, want ErrCommitmentClosed", err)
		}
	})

	t.Run("commitment confirmed expired beforehand", func(t *testing.T) {
		s, first := usedTwiceStore(t)
		before := expiryOK.Add(-time.Minute)
		// 由库存查询正常确认两笔承诺到期（c1、c2 同一到期时刻）。
		if _, err := s.PartStatus("part1", expiryOK); err != nil {
			t.Fatalf("confirm expiry via query: %v", err)
		}
		// 到期时刻的冲突提交。
		if _, err := s.Use("u1", "c1", 1, expiryOK); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflict at expiry: %v", err)
		}
		// 即使回退到到期前时刻原样取回，也只是返回首次结果。
		got, err := s.Use("u1", "c1", 2, before)
		if err != nil || got != first {
			t.Fatalf("retry at earlier time after expiry = %+v, err %v; want %+v", got, err, first)
		}
		c1, _ := s.Commitment("c1")
		if !c1.Expired || c1.Canceled || c1.Used != 2 {
			t.Fatalf("c1 reopened or changed by retry: %+v", c1)
		}
		// 请求视图在较早时刻仍显示到期，占用为零；旧结果不重新开放承诺。
		view, _ := s.RequestView("r1", before)
		for _, d := range view.Commitments {
			if d.Status != CommitmentExpired {
				t.Fatalf("%s status = %q, want expired", d.CommitmentID, d.Status)
			}
		}
		if _, err := s.Use("u9", "c1", 1, before); !errors.Is(err, ErrCommitmentClosed) {
			t.Fatalf("new use after old-result retrieval: got %v, want ErrCommitmentClosed", err)
		}
		// 到期只释放未用占用、不补实物：实物八件、有效占用零、可承诺八件。
		st, _ := s.PartStatus("part1", before)
		if st.PhysicalRemaining != 8 || st.ActiveOccupied != 0 || st.Committable != 8 {
			t.Fatalf("stock after expiry+retry = %+v, want phys=8 occupied=0 committable=8", st)
		}
		d1, _ := detailByID(st, "c1")
		if d1.UsedQuantity != 2 || d1.RemainingQuantity != 3 {
			t.Fatalf("c1 detail = %+v, want used=2 remaining=3", d1)
		}
	})
}

// TestFreshUsageIDRejectsInvalidThenSucceeds 是对照测试：尚未成功使用的新
// 编号提交零数量等非法内容仍按普通 ErrInvalidParam（未知承诺为 ErrNotFound）
// 处理，提前失败不占用编号，修正为合法数量后同一编号可以正常使用。
func TestFreshUsageIDRejectsInvalidThenSucceeds(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if _, err := s.Use("uFresh", "c1", 0, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero quantity for fresh id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Use("uFresh", "c1", -1, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("negative quantity for fresh id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Use("uEmpty", "", 1, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty commitment id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Use("uMissing", "cMissing", 1, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown commitment: got %v, want ErrNotFound", err)
	}

	// 非法/缺失引用的提前失败不占用编号：修正内容后同一编号成功使用。
	u, err := s.Use("uFresh", "c1", 2, nowOK)
	if err != nil {
		t.Fatalf("fresh id succeeds after correction: %v", err)
	}
	if u.ID != "uFresh" || u.CommitmentID != "c1" || u.Quantity != 2 {
		t.Fatalf("usage = %+v, want {uFresh c1 2}", u)
	}
	u2, err := s.Use("uMissing", "c1", 1, nowOK)
	if err != nil {
		t.Fatalf("previously-not-found id succeeds with valid commitment: %v", err)
	}
	if u2.ID != "uMissing" || u2.Quantity != 1 {
		t.Fatalf("usage = %+v, want {uMissing c1 1}", u2)
	}
	c1, _ := s.Commitment("c1")
	if c1.Used != 3 {
		t.Fatalf("c1 used = %d, want 3", c1.Used)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 7 {
		t.Fatalf("physical remaining = %d, want 7", st.PhysicalRemaining)
	}

	// 新编号一旦成功，再改内容提交即为冲突，与上面的普通参数错误形成对照。
	if _, err := s.Use("uFresh", "c1", 0, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("successful id with zero quantity after success: got %v, want ErrConflict", err)
	}
}
