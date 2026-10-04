package warranty

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 本文件回归保障同一使用编号被多个调用同时提交时的行为：一个编号在整个仓库内
// 只能产生一次真实扣减。首次成功把编号与承诺编号、使用数量绑定（当前时刻不属于
// 绑定内容），内容相同的并发提交全部取回这同一份完整记录，内容不同的并发提交
// 全部得到 ErrConflict；承诺的累计已用数量与备件实物库存只随这一次成功变化，
// 不随成功返回的次数重复扣减。这些测试只沿用登记、预留、使用和查询的既有公开
// 行为，全部提交时刻都在承诺有效期内，且承诺与备件满足每份提交单独执行时的
// 数量要求。

// runConcurrentUses 用 start 屏障让 groups 组、每组 n 个调用尽量同时提交
// s.Use(usageID, commitID, quantity, nowOK)，返回每组每份调用的结果与错误。
func runConcurrentUses(s *Store, groups []struct{ usageID, commitID string; quantity int }, n int) ([][]Usage, [][]error) {
	start := make(chan struct{})
	usages := make([][]Usage, len(groups))
	errs := make([][]error, len(groups))
	var wg sync.WaitGroup
	for gi, g := range groups {
		usages[gi] = make([]Usage, n)
		errs[gi] = make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(gi, i int, usageID, commitID string, quantity int) {
				defer wg.Done()
				<-start
				usages[gi][i], errs[gi][i] = s.Use(usageID, commitID, quantity, nowOK)
			}(gi, i, g.usageID, g.commitID, g.quantity)
		}
	}
	close(start)
	wg.Wait()
	return usages, errs
}

// assertGroupAllSucceeded 断言一组并发调用全部成功且返回同一份完整使用记录。
func assertGroupAllSucceeded(t *testing.T, usages []Usage, errs []error, want Usage) {
	t.Helper()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("call %d failed: %v", i, errs[i])
		}
		if usages[i] != want {
			t.Fatalf("call %d returned %+v, want identical record %+v", i, usages[i], want)
		}
	}
}

// assertGroupAllConflict 断言一组并发调用全部返回 ErrConflict 且无使用记录。
func assertGroupAllConflict(t *testing.T, usages []Usage, errs []error) {
	t.Helper()
	for i := range errs {
		if !errors.Is(errs[i], ErrConflict) {
			t.Fatalf("call %d: got %v, want ErrConflict", i, errs[i])
		}
		if usages[i] != (Usage{}) {
			t.Fatalf("call %d: conflict returned non-empty usage %+v", i, usages[i])
		}
	}
}

// assertStock 断言备件在 nowOK 时刻的实物剩余、有效占用与可承诺数量。
func assertStock(t *testing.T, s *Store, partID string, phys, occupied, committable int) {
	t.Helper()
	st, err := s.PartStatus(partID, nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != phys || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, phys, occupied, committable)
	}
}

// TestConcurrentIdenticalUseDeductsOnce 多个调用同时提交相同的使用编号、承诺
// 编号和数量，承诺未取消、未到期且余量足够完成这一次使用：所有调用都成功并
// 返回相同的完整使用记录，承诺已用数量与实物库存只变化一次，不随成功返回的
// 次数重复扣减；有效占用只减少实际使用的数量，可承诺数量保持原值。
func TestConcurrentIdenticalUseDeductsOnce(t *testing.T) {
	t.Run("partial use", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
			t.Fatalf("reserve: %v", err)
		}

		groups := []struct{ usageID, commitID string; quantity int }{{"u1", "c1", 2}}
		usages, errs := runConcurrentUses(s, groups, 16)
		assertGroupAllSucceeded(t, usages[0], errs[0], Usage{ID: "u1", CommitmentID: "c1", Quantity: 2})

		// 十六次成功返回只对应一次真实扣减：已用两件、实物八件；
		// 有效占用减少两件，可承诺数量保持使用前的五件。
		c1, _ := s.Commitment("c1")
		if c1.Used != 2 {
			t.Fatalf("c1 used = %d, want exactly 2 after %d successes", c1.Used, len(errs[0]))
		}
		assertStock(t, s, "part1", 8, 3, 5)
	})

	t.Run("use exactly exhausts commitment", func(t *testing.T) {
		s := newStore(t)
		// 承诺两件，这一次使用恰好耗尽余量：除首次外的相同提交仍应取回首次
		// 结果，不能被当作新的超量使用（ErrUsageExceeded）拒绝。
		if _, err := s.Reserve("c1", "r1", "part1", 2, expiryOK, nowOK); err != nil {
			t.Fatalf("reserve: %v", err)
		}

		groups := []struct{ usageID, commitID string; quantity int }{{"u1", "c1", 2}}
		usages, errs := runConcurrentUses(s, groups, 16)
		assertGroupAllSucceeded(t, usages[0], errs[0], Usage{ID: "u1", CommitmentID: "c1", Quantity: 2})

		c1, _ := s.Commitment("c1")
		if c1.Used != 2 {
			t.Fatalf("c1 used = %d, want exactly 2", c1.Used)
		}
		// 实物八件；占用降为零，可承诺数量保持使用前的八件。
		assertStock(t, s, "part1", 8, 0, 8)
	})
}

// TestConcurrentCompetingCommitmentsSingleWinner 两组调用同时竞争一个尚未成功
// 占用的编号，分别指定不同承诺：哪组先成功不作保证，但最终只有一份内容成为
// 该编号的成功记录；内容相同的调用都成功并返回同一结果，另一组全部返回
// ErrConflict，不能两组各自成功一次。沿用用户给定的主例数量：两个请求各自
// 预留同一备件五件和三件，实物库存十件，两组都尝试使用两件；结束后实物剩余
// 八件、有效占用六件、可承诺两件，只有胜出承诺的已用数量增加两件。
func TestConcurrentCompetingCommitmentsSingleWinner(t *testing.T) {
	s := newStore(t)
	if err := s.SubmitRequest("r2", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request r2: %v", err)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r2", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}

	groups := []struct{ usageID, commitID string; quantity int }{
		{"u1", "c1", 2},
		{"u1", "c2", 2},
	}
	usages, errs := runConcurrentUses(s, groups, 8)

	// 判定胜出组：成功记录只能是 {u1 c1 2} 或 {u1 c2 2} 之一。
	winner := -1
	for gi := range groups {
		if errs[gi][0] == nil {
			winner = gi
			break
		}
	}
	if winner < 0 {
		t.Fatalf("neither group succeeded: errs = %v / %v", errs[0], errs[1])
	}
	loser := 1 - winner
	want := Usage{ID: "u1", CommitmentID: groups[winner].commitID, Quantity: 2}
	assertGroupAllSucceeded(t, usages[winner], errs[winner], want)
	assertGroupAllConflict(t, usages[loser], errs[loser])

	// 只扣减一次：实物八件、有效占用六件、可承诺两件；胜出承诺已用两件，
	// 另一笔保持未使用。
	assertStock(t, s, "part1", 8, 6, 2)
	c1, _ := s.Commitment("c1")
	c2, _ := s.Commitment("c2")
	wantUsed := map[string]int{"c1": 0, "c2": 0}
	wantUsed[groups[winner].commitID] = 2
	if c1.Used != wantUsed["c1"] || c2.Used != wantUsed["c2"] {
		t.Fatalf("used = c1:%d c2:%d, want c1:%d c2:%d (winner %s)",
			c1.Used, c2.Used, wantUsed["c1"], wantUsed["c2"], groups[winner].commitID)
	}

	// 竞争落幕后：按胜出内容再提交仍取回同一记录，按落败内容提交仍是冲突，
	// 冲突不覆盖记录也不额外扣减。
	got, err := s.Use("u1", groups[winner].commitID, 2, nowOK)
	if err != nil || got != want {
		t.Fatalf("retry winning content = %+v, err %v; want %+v", got, err, want)
	}
	if _, err := s.Use("u1", groups[loser].commitID, 2, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("losing content after race: got %v, want ErrConflict", err)
	}
	assertStock(t, s, "part1", 8, 6, 2)
}

// TestConcurrentSameCommitmentDifferentQuantity 同一承诺下仅使用数量不同，也
// 属于不同内容竞争同一编号：成功记录与最终扣减量必须对应同一份提交；之后按
// 胜出内容再次提交返回原记录，按另一份内容提交仍冲突，冲突不覆盖记录、不
// 额外扣减数量。两份数量单独执行时承诺余量都足够。
func TestConcurrentSameCommitmentDifferentQuantity(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	groups := []struct{ usageID, commitID string; quantity int }{
		{"u1", "c1", 2},
		{"u1", "c1", 3},
	}
	usages, errs := runConcurrentUses(s, groups, 8)

	winner := -1
	for gi := range groups {
		if errs[gi][0] == nil {
			winner = gi
			break
		}
	}
	if winner < 0 {
		t.Fatalf("neither group succeeded: errs = %v / %v", errs[0], errs[1])
	}
	loser := 1 - winner
	winQty := groups[winner].quantity
	want := Usage{ID: "u1", CommitmentID: "c1", Quantity: winQty}
	assertGroupAllSucceeded(t, usages[winner], errs[winner], want)
	assertGroupAllConflict(t, usages[loser], errs[loser])

	// 最终扣减量与胜出提交的数量一致，只扣减一次。
	c1, _ := s.Commitment("c1")
	if c1.Used != winQty {
		t.Fatalf("c1 used = %d, want winning quantity %d", c1.Used, winQty)
	}
	assertStock(t, s, "part1", 10-winQty, 5-winQty, 5)

	// 按胜出内容再次提交：取回原记录，不再次扣减。
	for i := 0; i < 3; i++ {
		got, err := s.Use("u1", "c1", winQty, nowOK)
		if err != nil || got != want {
			t.Fatalf("retry winning content = %+v, err %v; want %+v", got, err, want)
		}
	}
	// 按落败内容提交：仍是冲突，不覆盖记录、不额外扣减。
	loseQty := groups[loser].quantity
	if _, err := s.Use("u1", "c1", loseQty, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("losing content after race: got %v, want ErrConflict", err)
	}
	c1, _ = s.Commitment("c1")
	if c1.Used != winQty {
		t.Fatalf("conflict changed used to %d, want %d", c1.Used, winQty)
	}
	assertStock(t, s, "part1", 10-winQty, 5-winQty, 5)
}

// TestConcurrentIdenticalUseAcrossDistinctIDs 对照测试：不同使用编号各自并发
// 提交同一承诺时互不绑定，每个编号各自成功一次并各自扣减；与同一编号只扣减
// 一次的保障形成对照，确认去重键是使用编号而非承诺编号。
func TestConcurrentIdenticalUseAcrossDistinctIDs(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	const ids = 3
	const perID = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, ids*perID)
	for id := 0; id < ids; id++ {
		for i := 0; i < perID; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				<-start
				u, err := s.Use(fmt.Sprintf("u%d", id), "c1", 1, nowOK)
				if err != nil {
					errCh <- err
					return
				}
				if u.Quantity != 1 || u.CommitmentID != "c1" {
					errCh <- fmt.Errorf("unexpected usage %+v", u)
				}
			}(id)
		}
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent use across distinct ids: %v", err)
	}

	// 三个编号各扣减一件：已用三件、实物七件、占用两件、可承诺五件。
	c1, _ := s.Commitment("c1")
	if c1.Used != ids {
		t.Fatalf("c1 used = %d, want %d", c1.Used, ids)
	}
	assertStock(t, s, "part1", 7, 2, 5)
}
