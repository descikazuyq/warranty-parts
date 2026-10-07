package warranty

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 本文件回归保障分批领取与 CommitmentUsages 明细查询同时发生时的规则。现有
// 明细检查主要发生在领取全部结束之后，这里补齐“领取期间持续核对”的使用条件：
// 查询只展示已经成功保存的领取事实，每条记录的使用编号、承诺编号和数量都完整
// 对应一次成功领取（无空编号、无错误数量、无同编号重复），列表始终按使用编号
// 字符串升序，与提交或完成先后无关；领取与查询重叠时，正在处理的领取可以尚未
// 出现也可以已经出现，但不能显示成部分数量。一次领取成功返回之后才开始的查询
// 必含该记录；连续不重叠查询不丢先前已见记录。查询返回的是取得时的独立快照，
// 后续领取不会让旧列表自动增长或改数。这些测试只沿用登记、预留、使用与查询的
// 既有公开行为。

// usagesOverlapStore 构造题设主例：part1 初始 10 件，请求 r1 的承诺 cSix 预留
// 6 件，承诺未取消、未到期。三个不同使用编号将分别领取 2、1、3 件。
func usagesOverlapStore(t *testing.T) *Store {
	t.Helper()
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
	if _, err := s.Reserve("cSix", "r1", "part1", 6, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cSix: %v", err)
	}
	return s
}

// validateUsagesSnapshot 校验一份查询快照在任何时刻都必须成立的不变量：
//   - 只含绑定到 commitID 的成功记录，使用编号非空、数量为正整数；
//   - 同一使用编号不重复，且编号 -> 数量与该编号的成功领取一一对应
//     （expected 给出本次场景全部可能成功的编号及其数量，快照只能是其前缀式
//     子集，数量不得有出入）；
//   - 按使用编号字符串升序排列；
//   - 列表数量之和不超过承诺已用数量（查询可能早于部分领取落库），且在领取
//     全部结束后与已用数量相等。
//
// 返回快照中各编号数量，便于调用方做跨查询的累积核对。
func validateUsagesSnapshot(t *testing.T, usages []Usage, commitID string,
	expected map[string]int) map[string]int {
	t.Helper()
	seen := make(map[string]int, len(usages))
	prev := ""
	sum := 0
	for i, u := range usages {
		if u.ID == "" {
			t.Fatalf("usages[%d] has empty usage id: %+v", i, u)
		}
		if u.CommitmentID != commitID {
			t.Fatalf("usages[%d] commitment = %q, want %q (other commitments must not mix in)",
				i, u.CommitmentID, commitID)
		}
		if u.Quantity <= 0 {
			t.Fatalf("usages[%d] quantity = %d, want a positive integer (no partial/zero record)",
				i, u.Quantity)
		}
		wantQty, ok := expected[u.ID]
		if !ok {
			t.Fatalf("usages[%d] contains unexpected usage id %q: %+v", i, u.ID, u)
		}
		if u.Quantity != wantQty {
			t.Fatalf("usages[%d] %q quantity = %d, want %d (must match the whole successful usage)",
				i, u.ID, u.Quantity, wantQty)
		}
		if _, dup := seen[u.ID]; dup {
			t.Fatalf("usages[%d] duplicated usage id %q in %+v", i, u.ID, usages)
		}
		seen[u.ID] = u.Quantity
		if i > 0 && !(prev < u.ID) {
			t.Fatalf("usages not sorted by id string: %q before %q in %+v", prev, u.ID, usages)
		}
		prev = u.ID
		sum += u.Quantity
	}
	return seen
}

// TestCommitmentUsagesOverlappingBatchedUses 是核心重叠主例：三个不同使用编号
// （U-Two 2 件、U-One 1 件、U-Three 3 件，编号大小次序与数量/提交次序都不同）
// 在领取进行期间被多个 goroutine 反复查询。任何一份中途快照都必须完整、有序、
// 无重复且只含整条成功记录；领取全部成功后明细恰好为这三次领取、合计六件，与
// 承诺已用数量一致，实物剩余四件、有效占用为零、可承诺四件。
func TestCommitmentUsagesOverlappingBatchedUses(t *testing.T) {
	s := usagesOverlapStore(t)

	uses := []struct {
		id   string
		qty  int
	}{
		{"U-Two", 2},
		{"U-One", 1},
		{"U-Three", 3},
	}
	wantQty := map[string]int{"U-Two": 2, "U-One": 1, "U-Three": 3}
	wantFinal := []Usage{
		{ID: "U-One", CommitmentID: "cSix", Quantity: 1},
		{ID: "U-Three", CommitmentID: "cSix", Quantity: 3},
		{ID: "U-Two", CommitmentID: "cSix", Quantity: 2},
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	useResults := make(map[string]error, len(uses))
	var resMu sync.Mutex

	// 查询方：闸机释放后持续查询，直到看到三条成功记录为止。领取与查询重叠
	// 时，正在处理的领取可以尚未出现在快照中，也可以已经出现——因此这里不要求
	// 快照与“Use 是否已返回”严格对齐（可见时刻可能早于领取方拿到返回值），
	// 只要求出现的每条都是最终成功编号的整条记录，无空编号、无部分数量、无重复，
	// 且始终按使用编号字符串升序。
	const readers = 8
	var readerWG sync.WaitGroup
	for r := 0; r < readers; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			<-start
			for {
				us, err := s.CommitmentUsages("cSix")
				if err != nil {
					t.Errorf("concurrent commitment usages: %v", err)
					return
				}
				validateUsagesSnapshot(t, us, "cSix", wantQty)
				if len(us) == len(wantFinal) {
					return
				}
			}
		}()
	}

	// 领取方：三个不同编号同时提交，与查询交叉。全部必须成功。
	for _, u := range uses {
		wg.Add(1)
		go func(id string, qty int) {
			defer wg.Done()
			<-start
			_, err := s.Use(id, "cSix", qty, nowOK)
			resMu.Lock()
			useResults[id] = err
			resMu.Unlock()
		}(u.id, u.qty)
	}
	close(start)
	wg.Wait()
	readerWG.Wait()

	for _, u := range uses {
		if err := useResults[u.id]; err != nil {
			t.Fatalf("use %s %d: %v", u.id, u.qty, err)
		}
	}

	// 终态：明细恰好三条，按使用编号字符串升序（与提交次序、数量均无关），
	// 合计六件，与承诺已用数量一致。
	us, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("final commitment usages: %v", err)
	}
	if len(us) != len(wantFinal) {
		t.Fatalf("final usages = %+v, want %d records", us, len(wantFinal))
	}
	sum := 0
	for i, u := range us {
		if u != wantFinal[i] {
			t.Fatalf("final usages[%d] = %+v, want %+v", i, u, wantFinal[i])
		}
		sum += u.Quantity
	}
	if c := mustCommitment(t, s, "cSix"); sum != 6 || c.Used != 6 || c.Unused() != 0 {
		t.Fatalf("usages sum = %d, cSix used = %d, unused = %d; want 6/6/0", sum, c.Used, c.Unused())
	}
	// 实物剩余四件、有效占用为零、可承诺四件。
	assertPartAccount(t, s, 4, 0, 4)
}

// TestCommitmentUsagesQueryAfterSuccessContainsRecord 锁定“成功返回之后才开始
// 的查询必含该记录”，以及同一用户连续不重叠查询的单调性：先前已见的成功记录
// 不会丢失，数量不会改变；新成功的记录只可能在下一次查询出现。
func TestCommitmentUsagesQueryAfterSuccessContainsRecord(t *testing.T) {
	s := usagesOverlapStore(t)

	// 第一次领取成功返回之后才查询：必含 U-Two 两件。
	if _, err := s.Use("U-Two", "cSix", 2, nowOK); err != nil {
		t.Fatalf("use U-Two: %v", err)
	}
	first, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("query after first success: %v", err)
	}
	if len(first) != 1 || first[0] != (Usage{ID: "U-Two", CommitmentID: "cSix", Quantity: 2}) {
		t.Fatalf("first snapshot = %+v, want only {U-Two cSix 2}", first)
	}

	// 第二次领取尚未发生，连续不重叠查询不能凭空增长，也不能丢掉 U-Two。
	again, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("repeat query: %v", err)
	}
	if len(again) != 1 || again[0] != first[0] {
		t.Fatalf("repeat snapshot = %+v, want unchanged %+v", again, first)
	}

	// 第二次领取成功后查询：新增 U-One 一件，U-Two 保持两件且不重复。
	if _, err := s.Use("U-One", "cSix", 1, nowOK); err != nil {
		t.Fatalf("use U-One: %v", err)
	}
	second, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("query after second success: %v", err)
	}
	want := []Usage{
		{ID: "U-One", CommitmentID: "cSix", Quantity: 1},
		{ID: "U-Two", CommitmentID: "cSix", Quantity: 2},
	}
	if len(second) != 2 {
		t.Fatalf("second snapshot = %+v, want 2 records", second)
	}
	for i, u := range second {
		if u != want[i] {
			t.Fatalf("second snapshot[%d] = %+v, want %+v", i, u, want[i])
		}
	}

	// 第三次领取成功后查询：三条齐全，累计六件。
	if _, err := s.Use("U-Three", "cSix", 3, nowOK); err != nil {
		t.Fatalf("use U-Three: %v", err)
	}
	final, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("query after third success: %v", err)
	}
	seen := validateUsagesSnapshot(t, final, "cSix",
		map[string]int{"U-Two": 2, "U-One": 1, "U-Three": 3})
	if len(seen) != 3 {
		t.Fatalf("final snapshot = %+v, want all three successful usages", final)
	}
}

// TestCommitmentUsagesSnapshotStaysFrozen 锁定查询期间保存下来的旧列表是取得
// 时的独立副本：领取全部结束后再看旧列表，仍保持取得时的内容，不会自动增加
// 记录或改变已有数量；重新查询才反映新的成功事实。
func TestCommitmentUsagesSnapshotStaysFrozen(t *testing.T) {
	s := usagesOverlapStore(t)

	if _, err := s.Use("U-Two", "cSix", 2, nowOK); err != nil {
		t.Fatalf("use U-Two: %v", err)
	}
	saved, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("query saved snapshot: %v", err)
	}
	if len(saved) != 1 || saved[0] != (Usage{ID: "U-Two", CommitmentID: "cSix", Quantity: 2}) {
		t.Fatalf("saved snapshot = %+v, want only {U-Two cSix 2}", saved)
	}

	// 旧列表保存期间，其余两批领取完成。
	if _, err := s.Use("U-One", "cSix", 1, nowOK); err != nil {
		t.Fatalf("use U-One: %v", err)
	}
	if _, err := s.Use("U-Three", "cSix", 3, nowOK); err != nil {
		t.Fatalf("use U-Three: %v", err)
	}

	// 旧列表保持取得时的内容：没有自动增长，已有数量不变。
	if len(saved) != 1 || saved[0] != (Usage{ID: "U-Two", CommitmentID: "cSix", Quantity: 2}) {
		t.Fatalf("saved snapshot mutated by later uses: %+v, want frozen {U-Two cSix 2}", saved)
	}
	// 重新查询才反映新的成功事实：三条齐全、按编号升序、合计六件。
	fresh, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	wantFresh := []Usage{
		{ID: "U-One", CommitmentID: "cSix", Quantity: 1},
		{ID: "U-Three", CommitmentID: "cSix", Quantity: 3},
		{ID: "U-Two", CommitmentID: "cSix", Quantity: 2},
	}
	if len(fresh) != len(wantFresh) {
		t.Fatalf("fresh snapshot = %+v, want %d records", fresh, len(wantFresh))
	}
	sum := 0
	for i, u := range fresh {
		if u != wantFresh[i] {
			t.Fatalf("fresh snapshot[%d] = %+v, want %+v", i, u, wantFresh[i])
		}
		sum += u.Quantity
	}
	if sum != 6 {
		t.Fatalf("fresh snapshot sum = %d, want 6", sum)
	}
}

// TestCommitmentUsagesOverlappingDuplicateAndExceeded 同时保留两个直接相关边界，
// 且让它们与查询并发发生：
//   - 同一使用编号、同一承诺、同一数量的重复领取（含并发重复提交）只对应一条
//     成功明细和一次实际扣减；
//   - 另一未成功编号申请七件，超过承诺最多六件的未用数量，整次返回
//     ErrUsageExceeded，任何查询都不能把它列为成功领取，也不能只记入允许部分。
func TestCommitmentUsagesOverlappingDuplicateAndExceeded(t *testing.T) {
	s := usagesOverlapStore(t)

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, 12)

	// 前 8 个调用：同一编号 U-Dup、同一承诺 cSix、同一数量 2，全部同时提交。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = s.Use("U-Dup", "cSix", 2, nowOK)
		}(i)
	}
	// 后 4 个调用：未成功编号 U-Over 申请 7 件，超过六件未用数量，整次失败。
	for i := 8; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = s.Use("U-Over", "cSix", 7, nowOK)
		}(i)
	}

	// 与领取并发反复查询：任何一份快照都只能在 U-Dup 成功后出现恰好一条
	// {U-Dup cSix 2}，任何时刻都不能出现 U-Over，也不能出现部分数量。
	const readers = 6
	var readerWG sync.WaitGroup
	stopQuery := make(chan struct{})
	for r := 0; r < readers; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			<-start
			for {
				select {
				case <-stopQuery:
					return
				default:
				}
				us, err := s.CommitmentUsages("cSix")
				if err != nil {
					t.Errorf("concurrent query: %v", err)
					return
				}
				for _, u := range us {
					switch u.ID {
					case "U-Dup":
						if u != (Usage{ID: "U-Dup", CommitmentID: "cSix", Quantity: 2}) {
							t.Errorf("U-Dup snapshot record = %+v, want whole {U-Dup cSix 2}", u)
						}
					case "U-Over":
						t.Errorf("failed over-use U-Over must never appear in successful usages: %+v", us)
					default:
						t.Errorf("unexpected usage id %q in snapshot %+v", u.ID, us)
					}
				}
				if len(us) > 1 {
					t.Errorf("snapshot %+v has %d records, want at most the single U-Dup record", us, len(us))
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(stopQuery)
	readerWG.Wait()

	for i := 0; i < 8; i++ {
		if results[i] != nil {
			t.Fatalf("duplicate submit %d: got %v, want success returning first record", i, results[i])
		}
	}
	for i := 8; i < 12; i++ {
		if !errors.Is(results[i], ErrUsageExceeded) {
			t.Fatalf("over-use submit %d: got %v, want ErrUsageExceeded", i, results[i])
		}
	}

	// 终态明细：恰好一条 U-Dup 两件；U-Over 从未成功，绝不列入。
	us, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("final query: %v", err)
	}
	if len(us) != 1 || us[0] != (Usage{ID: "U-Dup", CommitmentID: "cSix", Quantity: 2}) {
		t.Fatalf("final usages = %+v, want only {U-Dup cSix 2}", us)
	}
	// 只实际扣减一次两件：承诺已用两件、未用四件；实物八件、有效占用四件、
	// 可承诺四件。超量申请整次失败，不允许部分扣减。
	if c := mustCommitment(t, s, "cSix"); c.Used != 2 || c.Unused() != 4 {
		t.Fatalf("cSix used = %d unused = %d, want 2/4 (single deduction)", c.Used, c.Unused())
	}
	assertPartAccount(t, s, 8, 4, 4)

	// 赛后原样重试 U-Dup 仍取回同一条记录、不再次扣减；U-Over 再查仍不存在。
	if got, err := s.Use("U-Dup", "cSix", 2, nowOK); err != nil ||
		got != (Usage{ID: "U-Dup", CommitmentID: "cSix", Quantity: 2}) {
		t.Fatalf("post-hoc U-Dup retry = %+v err %v, want the same record", got, err)
	}
	if c := mustCommitment(t, s, "cSix"); c.Used != 2 {
		t.Fatalf("post-hoc retry deducted again: used = %d, want 2", c.Used)
	}
	post, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("post-hoc query: %v", err)
	}
	if len(post) != 1 || post[0].ID == "U-Over" {
		t.Fatalf("post-hoc usages = %+v, want still only U-Dup", post)
	}

	// 余量仍真实可用：新编号领取剩余四件后成功，查询恰为两条（U-Dup、U-Rest），
	// 合计六件；这也证明失败的七件申请没有占走任何允许部分。
	if _, err := s.Use("U-Rest", "cSix", 4, nowOK); err != nil {
		t.Fatalf("use remaining 4 with fresh id: %v", err)
	}
	final, err := s.CommitmentUsages("cSix")
	if err != nil {
		t.Fatalf("final query after using remainder: %v", err)
	}
	wantFinal := []Usage{
		{ID: "U-Dup", CommitmentID: "cSix", Quantity: 2},
		{ID: "U-Rest", CommitmentID: "cSix", Quantity: 4},
	}
	if len(final) != len(wantFinal) {
		t.Fatalf("final usages = %+v, want %d records", final, len(wantFinal))
	}
	for i, u := range final {
		if u != wantFinal[i] {
			t.Fatalf("final usages[%d] = %+v, want %+v", i, u, wantFinal[i])
		}
	}
	if c := mustCommitment(t, s, "cSix"); c.Used != 6 {
		t.Fatalf("cSix used = %d, want 6", c.Used)
	}
}

// TestCommitmentUsagesOverlappingPartialVisibilityInvariant 用大量不同编号的小批
// 领取与持续查询反复交叉，统计每份快照：只能看到已成功编号的整条记录、无空编号、
// 无错误数量、无重复、按字符串升序，且任意前后两份快照之间成功记录集合不回退
// （先前看到的编号在后一份中仍在、数量不变）。运行多个迭代提高竞态覆盖。
func TestCommitmentUsagesOverlappingPartialVisibilityInvariant(t *testing.T) {
	const numUses = 6 // 每件承诺预留 6，恰好 6 个不同编号各领 1 件
	for iter := 0; iter < 5; iter++ {
		t.Run(fmt.Sprintf("iteration%d", iter), func(t *testing.T) {
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
			if _, err := s.Reserve("cSix", "r1", "part1", 6, expiryOK, nowOK); err != nil {
				t.Fatalf("reserve cSix: %v", err)
			}

			// 编号故意不按数值/提交次序排列字符串：u10 的字符串序在 u2 之前。
			ids := []string{"u03", "u01", "u10", "u02", "u05", "u04"}
			wantQty := make(map[string]int, numUses)
			for _, id := range ids {
				wantQty[id] = 1
			}

			var wg sync.WaitGroup
			start := make(chan struct{})
			for _, id := range ids {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					<-start
					if _, err := s.Use(id, "cSix", 1, nowOK); err != nil {
						t.Errorf("use %s: %v", id, err)
					}
				}(id)
			}

			const readers = 4
			var readerWG sync.WaitGroup
			stopQuery := make(chan struct{})
			for r := 0; r < readers; r++ {
				readerWG.Add(1)
				go func() {
					defer readerWG.Done()
					<-start
					var prev map[string]int
					for {
						select {
						case <-stopQuery:
							return
						default:
						}
						us, err := s.CommitmentUsages("cSix")
						if err != nil {
							t.Errorf("query: %v", err)
							return
						}
						seen := validateUsagesSnapshot(t, us, "cSix", wantQty)
						// 单调不回退：上一份快照中的每条记录在本份中仍在且数量不变。
						if prev != nil {
							for id, qty := range prev {
								if got, ok := seen[id]; !ok || got != qty {
									t.Errorf("success record %s(qty %d) lost or changed between non-overlapping reads: now %v",
										id, qty, seen)
								}
							}
							if len(seen) < len(prev) {
								t.Errorf("snapshot shrank from %d records to %d: %v", len(prev), len(seen), seen)
							}
						}
						prev = seen
					}
				}()
			}

			close(start)
			wg.Wait()
			close(stopQuery)
			readerWG.Wait()

			// 终态：六条各一件，按字符串升序（u01,u02,u03,u04,u05,u10），合计六件。
			final, err := s.CommitmentUsages("cSix")
			if err != nil {
				t.Fatalf("final query: %v", err)
			}
			if len(final) != numUses {
				t.Fatalf("final usages len = %d, want %d: %+v", len(final), numUses, final)
			}
			wantOrder := []string{"u01", "u02", "u03", "u04", "u05", "u10"}
			for i, u := range final {
				if u.ID != wantOrder[i] || u.CommitmentID != "cSix" || u.Quantity != 1 {
					t.Fatalf("final usages[%d] = %+v, want id %s qty 1", i, u, wantOrder[i])
				}
			}
			if c := mustCommitment(t, s, "cSix"); c.Used != 6 {
				t.Fatalf("cSix used = %d, want 6", c.Used)
			}
			assertPartAccount(t, s, 4, 0, 4)
		})
	}
}
