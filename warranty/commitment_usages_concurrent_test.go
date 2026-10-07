package warranty

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障“按承诺查询成功领取明细”（CommitmentUsages）在分批领取与明细
// 查询同时发生时的正确性。业务主例：备件 part1 初始库存十件，请求 r1 的承诺
// cMain 预留六件，产品在保、故障未被除外，承诺未取消、未到期。三个不同使用编号
// 分别领取两件、一件和三件可以与明细查询同时提交：
//
//   - 每次查询只展示已经成功保存的领取事实：每条记录的使用编号、承诺编号和数量
//     完整对应一次成功领取，不出现空编号、错误数量或同一编号的重复条目；列表
//     始终按使用编号字符串升序，与提交或完成的先后无关。
//   - 领取与查询重叠时，列表可以尚未包含正在处理的领取，也可以已经包含它，但
//     不能把一次领取显示成部分数量。
//   - 一次领取已经成功返回之后才开始的查询必须包含该记录；同一观察者连续进行
//     的不重叠查询不能丢失先前已经看到的成功记录。
//   - 全部成功后明细恰好是这三次领取、合计六件，与承诺已用数量一致；实物剩余
//     四件、有效占用为零、可承诺四件。查询期间保存下来的旧列表保持取得时的
//     内容，后续领取不让它自动增加记录或改变已有数量，重新查询才反映新事实。
//
// 另覆盖两个直接相关的边界：同一使用编号、同一承诺、同一数量的重复领取即使与
// 查询同时发生也只对应一条成功明细和一次实际扣减；尚未成功的编号申请七件（超过
// 这笔承诺最多六件的未用数量）整次返回 ErrUsageExceeded，任何查询都不能将它列
// 为成功领取或把允许的部分记进去。这些测试只沿用登记、预留、使用与查询的既有
// 公开入口与编号重试规则。

// cuCommit 是主例承诺编号；cuAllowed 是主例三次领取各自绑定的完整数量。
const cuCommit = "cMain"

var cuAllowed = map[string]int{"U-A": 1, "U-B": 2, "U-C": 3}

// usagesRaceStore 构造主例：part1 初始十件，r1 的承诺 cMain 预留六件，承诺有效
// 且尚未使用。newStore 已登记在保产品（除外故障不含本请求故障代码）与请求 r1。
func usagesRaceStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	if _, err := s.Reserve(cuCommit, "r1", "part1", 6, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", cuCommit, err)
	}
	return s
}

// snapshotProblem 校验单份明细快照的完整性规则：每条记录编号非空、属于本承诺、
// 数量等于该编号绑定的完整数量（绝不出现部分数量，也绝不出现不属于成功集合的
// 编号）；列表严格按使用编号升序，因而也不含重复条目。合法快照可以是成功集合
// 的任意子集（进行中的领取可以尚未出现），所以缺失不算问题。
func snapshotProblem(us []Usage, commitID string, allowed map[string]int) error {
	for i, u := range us {
		if u.ID == "" {
			return fmt.Errorf("snapshot contains empty usage id: %+v", us)
		}
		if u.CommitmentID != commitID {
			return fmt.Errorf("snapshot contains record of commitment %q, want only %q: %+v",
				u.CommitmentID, commitID, us)
		}
		want, ok := allowed[u.ID]
		if !ok {
			return fmt.Errorf("snapshot contains usage %q that never succeeded: %+v", u.ID, us)
		}
		if u.Quantity != want {
			return fmt.Errorf("usage %q shows quantity %d, want its full bound quantity %d: %+v",
				u.ID, u.Quantity, want, us)
		}
		if i > 0 && us[i-1].ID >= u.ID {
			return fmt.Errorf("snapshot not strictly ascending by usage id (duplicate or unsorted): %+v", us)
		}
	}
	return nil
}

// usagesWatch 收集并发查询观察者发现的问题；queries 记录已执行的查询次数。
type usagesWatch struct {
	mu      sync.Mutex
	errs    []error
	queries int
}

func (w *usagesWatch) fail(err error) {
	w.mu.Lock()
	w.errs = append(w.errs, err)
	w.mu.Unlock()
}

// watchCommitmentUsages 在 stop 关闭前持续查询 commitID 的成功明细：每份快照都
// 校验完整性规则；同一观察者的连续查询互不重叠，后一份快照不得丢失前一份已经
// 看到的任何成功记录（记录只增不改）。查询错误与快照问题都记入 w，由主协程在
// 观察者结束后统一断言，避免在子协程里直接 FailNow。
func watchCommitmentUsages(s *Store, commitID string, allowed map[string]int,
	stop <-chan struct{}, w *usagesWatch, wg *sync.WaitGroup) {
	defer wg.Done()
	prev := map[string]int{}
	for {
		select {
		case <-stop:
			return
		default:
		}
		us, err := s.CommitmentUsages(commitID)
		w.mu.Lock()
		w.queries++
		w.mu.Unlock()
		if err != nil {
			w.fail(fmt.Errorf("query during concurrent use: %w", err))
			continue
		}
		if p := snapshotProblem(us, commitID, allowed); p != nil {
			w.fail(p)
		}
		cur := make(map[string]int, len(us))
		for _, u := range us {
			cur[u.ID] = u.Quantity
		}
		for id, q := range prev {
			if got, ok := cur[id]; !ok || got != q {
				w.fail(fmt.Errorf("sequential query lost previously seen record %s=%d: snapshot %+v", id, q, us))
			}
		}
		prev = cur
	}
}

// stopWatch 关闭观察者并返回它发现的问题；同时断言观察期间确实执行过查询，
// 否则“重叠时规则成立”无从谈起。
func stopWatch(t *testing.T, stop chan<- struct{}, w *usagesWatch, wg *sync.WaitGroup) {
	t.Helper()
	close(stop)
	wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queries == 0 {
		t.Fatal("observer never queried during the race window")
	}
	for _, err := range w.errs {
		t.Errorf("observed during concurrent use/query: %v", err)
	}
}

// TestConcurrentUseAndQueryMainExample 是主例的并发回归：三个不同使用编号分别
// 领取两件、一件和三件，与持续的明细查询同时发生。重叠期间每份快照都必须满足
// 完整性、排序与单调性规则；全部成功后明细恰好是这三次领取（按编号升序）、合计
// 六件，与承诺已用数量一致，实物四件、有效占用零、可承诺四件。
func TestConcurrentUseAndQueryMainExample(t *testing.T) {
	s := usagesRaceStore(t)

	w := &usagesWatch{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go watchCommitmentUsages(s, cuCommit, cuAllowed, stop, w, &wg)

	// 提交顺序（U-C、U-B、U-A）与编号升序无关，核对排序只按编号字符串。
	calls := []concurrentUseCall{
		{usageID: "U-C", commitID: cuCommit, quantity: 3, now: nowOK},
		{usageID: "U-B", commitID: cuCommit, quantity: 2, now: nowOK.Add(time.Minute)},
		{usageID: "U-A", commitID: cuCommit, quantity: 1, now: nowOK.Add(2 * time.Minute)},
	}
	results := fanOutConcurrentUse(s, calls)
	stopWatch(t, stop, w, &wg)

	for i, r := range results {
		c := calls[i]
		want := Usage{ID: c.usageID, CommitmentID: cuCommit, Quantity: c.quantity}
		if r.err != nil || r.usage != want {
			t.Fatalf("use %v = %+v, err %v; want %+v", c, r.usage, r.err, want)
		}
	}

	// 全部成功返回之后才开始的查询必须包含全部三条记录，按编号升序。
	us, err := s.CommitmentUsages(cuCommit)
	if err != nil {
		t.Fatalf("final usages: %v", err)
	}
	want := []Usage{
		{ID: "U-A", CommitmentID: cuCommit, Quantity: 1},
		{ID: "U-B", CommitmentID: cuCommit, Quantity: 2},
		{ID: "U-C", CommitmentID: cuCommit, Quantity: 3},
	}
	if len(us) != len(want) {
		t.Fatalf("final usages = %+v, want %+v", us, want)
	}
	sum := 0
	for i, u := range us {
		if u != want[i] {
			t.Fatalf("final usages[%d] = %+v, want %+v", i, u, want[i])
		}
		sum += u.Quantity
	}
	// 明细合计六件，与承诺已用数量一致；实物四件、有效占用零、可承诺四件。
	if c := mustCommitment(t, s, cuCommit); sum != 6 || c.Used != 6 {
		t.Fatalf("usages sum = %d, cMain used = %d, want both 6", sum, c.Used)
	}
	assertPartAccount(t, s, 4, 0, 4)
}

// TestSequentialQueriesKeepSeenRecordsAndSnapshotsFrozen 确定地锁定两条顺序规则：
// 一次领取成功返回之后才开始的查询必须包含该记录（连续查询不丢失先前见过的记录）；
// 查询期间保存下来的旧列表保持取得时的内容，后续领取不让它自动增加记录或改变
// 已有数量，只有重新查询才反映新的成功事实。
func TestSequentialQueriesKeepSeenRecordsAndSnapshotsFrozen(t *testing.T) {
	s := usagesRaceStore(t)

	if _, err := s.Use("U-B", cuCommit, 2, nowOK); err != nil {
		t.Fatalf("use U-B: %v", err)
	}
	// U-B 已成功返回：此刻开始的查询必须包含它，且只有它。
	q1, err := s.CommitmentUsages(cuCommit)
	if err != nil {
		t.Fatalf("query after U-B: %v", err)
	}
	if len(q1) != 1 || q1[0] != (Usage{ID: "U-B", CommitmentID: cuCommit, Quantity: 2}) {
		t.Fatalf("q1 = %+v, want only {U-B cMain 2}", q1)
	}

	if _, err := s.Use("U-A", cuCommit, 1, nowOK); err != nil {
		t.Fatalf("use U-A: %v", err)
	}
	q2, err := s.CommitmentUsages(cuCommit)
	if err != nil {
		t.Fatalf("query after U-A: %v", err)
	}
	wantQ2 := []Usage{
		{ID: "U-A", CommitmentID: cuCommit, Quantity: 1},
		{ID: "U-B", CommitmentID: cuCommit, Quantity: 2},
	}
	if len(q2) != len(wantQ2) {
		t.Fatalf("q2 = %+v, want %+v", q2, wantQ2)
	}
	for i, u := range q2 {
		if u != wantQ2[i] {
			t.Fatalf("q2[%d] = %+v, want %+v", i, u, wantQ2[i])
		}
	}
	// 保存下来的 q1 不受后续领取影响：不自动增加记录、不改变已有数量。
	if len(q1) != 1 || q1[0] != (Usage{ID: "U-B", CommitmentID: cuCommit, Quantity: 2}) {
		t.Fatalf("saved q1 changed after later use: %+v, want still only {U-B cMain 2}", q1)
	}

	if _, err := s.Use("U-C", cuCommit, 3, nowOK); err != nil {
		t.Fatalf("use U-C: %v", err)
	}
	// 重新查询才反映新的成功事实：恰好三次领取，合计六件。
	q3, err := s.CommitmentUsages(cuCommit)
	if err != nil {
		t.Fatalf("query after U-C: %v", err)
	}
	wantQ3 := []Usage{
		{ID: "U-A", CommitmentID: cuCommit, Quantity: 1},
		{ID: "U-B", CommitmentID: cuCommit, Quantity: 2},
		{ID: "U-C", CommitmentID: cuCommit, Quantity: 3},
	}
	if len(q3) != len(wantQ3) {
		t.Fatalf("q3 = %+v, want %+v", q3, wantQ3)
	}
	sum := 0
	for i, u := range q3 {
		if u != wantQ3[i] {
			t.Fatalf("q3[%d] = %+v, want %+v", i, u, wantQ3[i])
		}
		sum += u.Quantity
	}
	if sum != 6 {
		t.Fatalf("q3 quantity sum = %d, want 6", sum)
	}
	// 旧列表 q1、q2 仍保持各自取得时的内容。
	if len(q1) != 1 || q1[0] != (Usage{ID: "U-B", CommitmentID: cuCommit, Quantity: 2}) {
		t.Fatalf("saved q1 changed after all uses: %+v", q1)
	}
	if len(q2) != len(wantQ2) {
		t.Fatalf("saved q2 grew after later use: %+v, want %+v", q2, wantQ2)
	}
	for i, u := range q2 {
		if u != wantQ2[i] {
			t.Fatalf("saved q2[%d] = %+v, want %+v", i, u, wantQ2[i])
		}
	}

	if c := mustCommitment(t, s, cuCommit); c.Used != 6 {
		t.Fatalf("cMain used = %d, want 6", c.Used)
	}
	assertPartAccount(t, s, 4, 0, 4)
}

// TestConcurrentDuplicateUseWithQuerySingleRecord 边界一：同一使用编号、同一承诺、
// 同一数量的重复领取即使与查询同时发生，也只对应一条成功明细和一次实际扣减。
// 重叠期间的每份快照要么还没有该编号、要么恰好有一条完整数量的记录，绝不能出现
// 重复条目；结束后明细只有一条，承诺已用与实物库存只变化一次。
func TestConcurrentDuplicateUseWithQuerySingleRecord(t *testing.T) {
	s := usagesRaceStore(t)

	allowed := map[string]int{"U-DUP": 2}
	w := &usagesWatch{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go watchCommitmentUsages(s, cuCommit, allowed, stop, w, &wg)

	const n = 16
	results := fanOutConcurrentUse(s, identicalUseCalls("U-DUP", cuCommit, 2, n))
	stopWatch(t, stop, w, &wg)

	want := Usage{ID: "U-DUP", CommitmentID: cuCommit, Quantity: 2}
	for i, r := range results {
		if r.err != nil || r.usage != want {
			t.Fatalf("duplicate call %d = %+v, err %v; want %+v", i, r.usage, r.err, want)
		}
	}

	// 成功返回 n 次，成功明细只有一条，实际扣减只有一次。
	us, err := s.CommitmentUsages(cuCommit)
	if err != nil {
		t.Fatalf("usages after duplicate race: %v", err)
	}
	if len(us) != 1 || us[0] != want {
		t.Fatalf("usages = %+v, want single %+v", us, want)
	}
	if c := mustCommitment(t, s, cuCommit); c.Used != 2 {
		t.Fatalf("cMain used = %d, want 2 (deducted once)", c.Used)
	}
	assertPartAccount(t, s, 8, 4, 4)
}

// TestConcurrentExceededUseNeverListedAsSuccess 边界二：尚未成功的使用编号申请
// 七件，超过这笔承诺最多六件的未用数量，即使与查询同时发生也整次返回
// ErrUsageExceeded；任何查询都不能将它列为成功领取，也不能把允许的部分（六件）
// 记进去。失败不扣减、不占用编号：事后沿用该编号以合法数量领取应当成功并出现在
// 明细中。
func TestConcurrentExceededUseNeverListedAsSuccess(t *testing.T) {
	s := usagesRaceStore(t)

	// 尚无任何成功领取：观察期间出现的任何记录都是问题。
	w := &usagesWatch{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go watchCommitmentUsages(s, cuCommit, map[string]int{}, stop, w, &wg)

	const n = 12
	calls := make([]concurrentUseCall, n)
	for i := range calls {
		calls[i] = concurrentUseCall{
			usageID:  "U-BIG",
			commitID: cuCommit,
			quantity: 7,
			now:      nowOK.Add(time.Duration(i) * time.Minute),
		}
	}
	results := fanOutConcurrentUse(s, calls)
	stopWatch(t, stop, w, &wg)

	for i, r := range results {
		if !errors.Is(r.err, ErrUsageExceeded) || r.usage != (Usage{}) {
			t.Fatalf("exceeded call %d = %+v, err %v; want ErrUsageExceeded with empty result",
				i, r.usage, r.err)
		}
	}

	// 整次失败：明细为空（既没有 U-BIG 的成功记录，也没有六件的部分记录），
	// 承诺已用与实物库存分毫未动。
	us, err := s.CommitmentUsages(cuCommit)
	if err != nil {
		t.Fatalf("usages after exceeded race: %v", err)
	}
	if len(us) != 0 {
		t.Fatalf("usages = %+v, want empty (exceeded use leaves no record, not even partial)", us)
	}
	if c := mustCommitment(t, s, cuCommit); c.Used != 0 {
		t.Fatalf("cMain used = %d, want 0 after rejected seven-piece uses", c.Used)
	}
	assertPartAccount(t, s, 10, 6, 4)

	// 失败不占用编号：沿用 U-BIG 以恰好六件（承诺最大未用数量）领取应当成功，
	// 并作为唯一一条完整记录出现在明细中。
	u, err := s.Use("U-BIG", cuCommit, 6, nowOK)
	if err != nil {
		t.Fatalf("rebind U-BIG with legal quantity: %v", err)
	}
	if u != (Usage{ID: "U-BIG", CommitmentID: cuCommit, Quantity: 6}) {
		t.Fatalf("rebound U-BIG = %+v, want {U-BIG cMain 6}", u)
	}
	us, err = s.CommitmentUsages(cuCommit)
	if err != nil {
		t.Fatalf("usages after legal rebind: %v", err)
	}
	if len(us) != 1 || us[0] != (Usage{ID: "U-BIG", CommitmentID: cuCommit, Quantity: 6}) {
		t.Fatalf("usages = %+v, want single {U-BIG cMain 6}", us)
	}
	if c := mustCommitment(t, s, cuCommit); c.Used != 6 {
		t.Fatalf("cMain used = %d, want 6 after legal rebind", c.Used)
	}
	assertPartAccount(t, s, 4, 0, 4)
}
