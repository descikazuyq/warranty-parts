package warranty

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障同一使用编号在并发提交下只产生一次真实扣减：首次成功把使用
// 编号与承诺编号、使用数量绑定（当前时刻不属于绑定内容）。此后相同内容的提交
// 都取回同一份完整使用记录，不同内容的提交一律 ErrConflict；无论成功返回多少
// 次，承诺累计已用数量与备件实物库存都只变化一次，有效占用只减少实际使用的
// 数量，可承诺数量保持原值。这些测试只沿用登记、预留、使用与查询的既有公开
// 行为。

// concurrentUseCall 是一次并发使用提交的入参。
type concurrentUseCall struct {
	usageID  string
	commitID string
	quantity int
	now      time.Time
}

// concurrentUseResult 是一次并发使用提交的结果。
type concurrentUseResult struct {
	usage Usage
	err   error
}

// fanOutConcurrentUse 让全部调用在同一道闸机释放后同时进入 Use，尽量制造交叉；
// 返回与调用一一对应的结果，调用方按下标分组核对成功记录与冲突。
func fanOutConcurrentUse(s *Store, calls []concurrentUseCall) []concurrentUseResult {
	results := make([]concurrentUseResult, len(calls))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c concurrentUseCall) {
			defer wg.Done()
			<-start
			results[i].usage, results[i].err = s.Use(c.usageID, c.commitID, c.quantity, c.now)
		}(i, c)
	}
	close(start)
	wg.Wait()
	return results
}

// identicalUseCalls 构造 n 个内容完全相同的使用调用；每个调用给不同但都处于
// 承诺有效期内的当前时刻，以证明当前时刻不属于绑定内容。
func identicalUseCalls(usageID, commitID string, quantity, n int) []concurrentUseCall {
	calls := make([]concurrentUseCall, n)
	for i := range calls {
		calls[i] = concurrentUseCall{
			usageID:  usageID,
			commitID: commitID,
			quantity: quantity,
			now:      nowOK.Add(time.Duration(i) * time.Hour),
		}
	}
	return calls
}

// TestConcurrentUseSameIDDeductsOnce 验证多个调用同时提交相同的使用编号、承诺
// 编号和数量时全部成功、返回同一份完整使用记录，但承诺已用数量与实物库存只
// 变化一次。含“这一次恰好耗尽承诺余量”的边界：其余相同内容提交仍取回首次
// 结果，不能被当作新的超量使用拒绝。
func TestConcurrentUseSameIDDeductsOnce(t *testing.T) {
	t.Run("partial use", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		const n = 16
		results := fanOutConcurrentUse(s, identicalUseCalls("u1", "c1", 2, n))

		want := Usage{ID: "u1", CommitmentID: "c1", Quantity: 2}
		for i, r := range results {
			if r.err != nil || r.usage != want {
				t.Fatalf("call %d = %+v, err %v; want first record %+v", i, r.usage, r.err, want)
			}
		}
		// 成功返回 n 次，真实扣减只有一次：实物八件、有效占用三件（c1 未用三）。
		st, _ := s.PartStatus("part1", nowOK)
		if st.PhysicalRemaining != 8 || st.ActiveOccupied != 3 || st.Committable != 5 {
			t.Fatalf("stock = %+v, want phys=8 occupied=3 committable=5", st)
		}
		c1, _ := s.Commitment("c1")
		if c1.Used != 2 || c1.Unused() != 3 {
			t.Fatalf("c1 = used %d / unused %d, want 2/3", c1.Used, c1.Unused())
		}
	})

	t.Run("exactly exhausts remaining", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		const n = 16
		results := fanOutConcurrentUse(s, identicalUseCalls("u1", "c1", 5, n))

		want := Usage{ID: "u1", CommitmentID: "c1", Quantity: 5}
		for i, r := range results {
			// 恰好耗尽余量：相同内容的并发提交仍取回首次结果，不按超量使用拒绝。
			if r.err != nil || r.usage != want {
				t.Fatalf("call %d = %+v, err %v; want first record %+v", i, r.usage, r.err, want)
			}
		}
		st, _ := s.PartStatus("part1", nowOK)
		if st.PhysicalRemaining != 5 || st.ActiveOccupied != 0 || st.Committable != 5 {
			t.Fatalf("stock = %+v, want phys=5 occupied=0 committable=5", st)
		}
		c1, _ := s.Commitment("c1")
		if c1.Used != 5 || c1.Unused() != 0 {
			t.Fatalf("c1 = used %d / unused %d, want 5/0", c1.Used, c1.Unused())
		}
		// 对照：耗尽之后新编号的新使用确实被当作超量使用拒绝；同编号原样提交
		// （即使在更晚的当前时刻）仍只取回首次记录，不再次扣减。
		if _, err := s.Use("uNew", "c1", 1, nowOK); !errors.Is(err, ErrUsageExceeded) {
			t.Fatalf("fresh use after exhaustion: got %v, want ErrUsageExceeded", err)
		}
		if got, err := s.Use("u1", "c1", 5, nowOK.Add(day)); err != nil || got != want {
			t.Fatalf("same content after exhaustion = %+v, err %v; want %+v", got, err, want)
		}
		if again, _ := s.PartStatus("part1", nowOK); again.PhysicalRemaining != 5 {
			t.Fatalf("physical remaining = %d, want still 5", again.PhysicalRemaining)
		}
	})
}

// TestConcurrentUseSameIDDifferentCommitmentSingleWinner 对应用户给出的主例：
// 两笔承诺各预留同一备件五件和三件，两组调用同时用同一个尚未成功占用的使用
// 编号分别使用两件。哪组先成功不作保证，但只能有一份内容成为成功记录：胜出
// 组全部成功并返回同一结果，落败组全部 ErrConflict。结束后实物剩余八件、有效
// 占用六件、可承诺两件；只有胜出承诺已用两件，落败承诺保持未使用。
func TestConcurrentUseSameIDDifferentCommitmentSingleWinner(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}

	const n = 12
	calls := make([]concurrentUseCall, 0, 2*n)
	for i := 0; i < n; i++ {
		calls = append(calls, concurrentUseCall{"uX", "c1", 2, nowOK.Add(time.Duration(i) * time.Minute)})
	}
	for i := 0; i < n; i++ {
		calls = append(calls, concurrentUseCall{"uX", "c2", 2, nowOK.Add(time.Duration(i) * time.Minute)})
	}
	results := fanOutConcurrentUse(s, calls)

	// 胜出者由实际数量判定（调用结果必须与实际扣减对应）：恰有一笔承诺已用两件。
	c1c, _ := s.Commitment("c1")
	c2c, _ := s.Commitment("c2")
	var winner, loser string
	switch {
	case c1c.Used == 2 && c2c.Used == 0:
		winner, loser = "c1", "c2"
	case c1c.Used == 0 && c2c.Used == 2:
		winner, loser = "c2", "c1"
	default:
		t.Fatalf("ambiguous deductions: c1 used=%d c2 used=%d, want exactly one at 2", c1c.Used, c2c.Used)
	}
	winningUsage := Usage{ID: "uX", CommitmentID: winner, Quantity: 2}

	// 与胜出内容相同的调用全部成功且返回同一记录，另一组全部 ErrConflict。
	success, conflict := 0, 0
	for i, c := range calls {
		r := results[i]
		if c.commitID == winner {
			if r.err != nil || r.usage != winningUsage {
				t.Fatalf("winner-group call %d = %+v, err %v; want %+v", i, r.usage, r.err, winningUsage)
			}
			success++
		} else {
			if !errors.Is(r.err, ErrConflict) || r.usage != (Usage{}) {
				t.Fatalf("loser-group call %d = %+v, err %v; want ErrConflict with empty result", i, r.usage, r.err)
			}
			conflict++
		}
	}
	if success != n || conflict != n {
		t.Fatalf("success=%d conflict=%d, want exactly %d/%d", success, conflict, n, n)
	}

	// 结束数量：实物八件、有效占用六件（c1 与 c2 未用之和）、可承诺两件。
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 6 || st.Committable != 2 {
		t.Fatalf("stock = %+v, want phys=8 occupied=6 committable=2 (winner=%s)", st, winner)
	}

	// 赛后按胜出内容提交取回原记录；按落败内容提交仍冲突，均不再次扣减。
	if got, err := s.Use("uX", winner, 2, nowOK.Add(2*day)); err != nil || got != winningUsage {
		t.Fatalf("winner content retry = %+v, err %v; want %+v", got, err, winningUsage)
	}
	if _, err := s.Use("uX", loser, 2, nowOK.Add(2*day)); !errors.Is(err, ErrConflict) {
		t.Fatalf("loser content retry: got %v, want ErrConflict", err)
	}
	st2, _ := s.PartStatus("part1", nowOK)
	if st2.PhysicalRemaining != 8 || st2.ActiveOccupied != 6 {
		t.Fatalf("post-hoc retries changed quantities: %+v", st2)
	}

	// 落败承诺保持未使用：换新编号仍可正常使用；可承诺两件也真实可再预留。
	if u, err := s.Use("uY", loser, 1, nowOK); err != nil {
		t.Fatalf("loser commitment %s should remain usable: %v", loser, err)
	} else if u.CommitmentID != loser || u.Quantity != 1 {
		t.Fatalf("use on loser = %+v, want commitment %s qty 1", u, loser)
	}
	if _, err := s.Reserve("c3", "r1", "part1", 2, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve remaining committable 2: %v", err)
	}
}

// TestConcurrentUseSameIDDifferentQuantitySingleWinner 验证同一承诺下仅使用
// 数量不同也属于不同内容竞争同一编号：成功记录与最终扣减量必须对应同一份
// 提交（两件或四件），胜出组全部返回同一记录，落败组全部 ErrConflict。赛后
// 按胜出内容提交取回原记录，按另一份内容提交仍冲突，冲突不覆盖记录、不额外
// 扣减。
func TestConcurrentUseSameIDDifferentQuantitySingleWinner(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	const n = 12
	calls := make([]concurrentUseCall, 0, 2*n)
	for i := 0; i < n; i++ {
		calls = append(calls, concurrentUseCall{"uQ", "c1", 2, nowOK.Add(time.Duration(i) * time.Minute)})
	}
	for i := 0; i < n; i++ {
		calls = append(calls, concurrentUseCall{"uQ", "c1", 4, nowOK.Add(time.Duration(i) * time.Minute)})
	}
	results := fanOutConcurrentUse(s, calls)

	// 胜出数量由承诺实际已用数量判定：只能是两份内容之一，不能是两者叠加。
	c1c, _ := s.Commitment("c1")
	var winnerQty, loserQty int
	switch c1c.Used {
	case 2:
		winnerQty, loserQty = 2, 4
	case 4:
		winnerQty, loserQty = 4, 2
	default:
		t.Fatalf("c1 used = %d, want exactly 2 or 4 (single winning content)", c1c.Used)
	}
	winningUsage := Usage{ID: "uQ", CommitmentID: "c1", Quantity: winnerQty}

	success, conflict := 0, 0
	for i, c := range calls {
		r := results[i]
		if c.quantity == winnerQty {
			if r.err != nil || r.usage != winningUsage {
				t.Fatalf("winner-group call %d = %+v, err %v; want %+v", i, r.usage, r.err, winningUsage)
			}
			success++
		} else {
			// 落败组即使其数量此刻会超过被首次使用后压缩的余量，也只能得到
			// 编号冲突，不能报超量使用，更不能扣减。
			if !errors.Is(r.err, ErrConflict) || r.usage != (Usage{}) {
				t.Fatalf("loser-group call %d = %+v, err %v; want ErrConflict with empty result", i, r.usage, r.err)
			}
			conflict++
		}
	}
	if success != n || conflict != n {
		t.Fatalf("success=%d conflict=%d, want exactly %d/%d", success, conflict, n, n)
	}

	// 实物与有效占用只随胜出数量变化一次；可承诺数量恒为初始的五件。
	st, _ := s.PartStatus("part1", nowOK)
	wantPhys, wantOccupied := 10-winnerQty, 5-winnerQty
	if st.PhysicalRemaining != wantPhys || st.ActiveOccupied != wantOccupied || st.Committable != 5 {
		t.Fatalf("winner qty %d: stock = %+v, want phys=%d occupied=%d committable=5",
			winnerQty, st, wantPhys, wantOccupied)
	}

	// 赛后按胜出内容提交取回原记录；按落败内容提交仍冲突，数量不再变化。
	if got, err := s.Use("uQ", "c1", winnerQty, nowOK.Add(day)); err != nil || got != winningUsage {
		t.Fatalf("winner content retry = %+v, err %v; want %+v", got, err, winningUsage)
	}
	if _, err := s.Use("uQ", "c1", loserQty, nowOK.Add(day)); !errors.Is(err, ErrConflict) {
		t.Fatalf("loser content retry: got %v, want ErrConflict", err)
	}
	c1b, _ := s.Commitment("c1")
	if c1b.Used != winnerQty {
		t.Fatalf("c1 used after retries = %d, want still %d", c1b.Used, winnerQty)
	}
}
