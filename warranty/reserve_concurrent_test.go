package warranty

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障同一承诺编号在首次成功前被两笔不同内容并发抢占时的编号规则：
// 两笔已登记、在保且不命中除外规则的请求，各自把数量与到期时刻都合法的内容
// 重复提交多次，却共用同一个尚未成功预留的承诺编号。无论哪笔先成功，该编号
// 最终只能对应一份预留内容：与首次成功完全相同的提交全部成功并返回同一份
// 完整的首次承诺，另一笔请求的提交全部 ErrConflict；不能两笔都成功，也不能
// 把后来的内容写进已经成功的承诺。即使库存充足到能同时容纳两笔数量，有效占用
// 也只能是获胜内容的数量。落败请求每次提交各留一条冲突失败记录（资格与库存
// 依据均为空），获胜请求只留一条首次成功记录（保存当次资格与新增占用前的
// 库存依据），同内容重试不追加；记录各归各的请求，请求内序号按处理次序严格
// 递增。这些测试只沿用登记、资格、预留和查询的既有公开行为。

// concurrentReserveCall 是一次并发预留提交的入参；承诺编号由 fan-out 统一给定。
type concurrentReserveCall struct {
	requestID string
	quantity  int
	expiry    time.Time
	now       time.Time
}

// concurrentReserveResult 是一次并发预留提交的结果。
type concurrentReserveResult struct {
	commitment Commitment
	err        error
}

// reserveContentionStore 构造两笔已登记、在保、不命中除外规则的请求（r1、r2），
// 备件 part1 的初始库存由调用方给定。
func reserveContentionStore(t *testing.T, stock int) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", stock); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r1: %v", err)
	}
	if err := s.SubmitRequest("r2", "p1", "LEAK"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}
	return s
}

// fanOutConcurrentReserve 让全部调用在同一道闸机释放后同时进入 Reserve，尽量
// 制造交叉；返回与调用一一对应的结果。
func fanOutConcurrentReserve(s *Store, commitID string, calls []concurrentReserveCall) []concurrentReserveResult {
	results := make([]concurrentReserveResult, len(calls))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c concurrentReserveCall) {
			defer wg.Done()
			<-start
			results[i].commitment, results[i].err = s.Reserve(
				commitID, c.requestID, "part1", c.quantity, c.expiry, c.now)
		}(i, c)
	}
	close(start)
	wg.Wait()
	return results
}

// contentionContents 给出两组不同内容：r1 要 qtyA、到期 expiryOK；r2 要
// qtyB、到期 expiryOK 之后一天。两组各 n 次交错排列，每次提交给不同的当前
// 时刻（当前时刻不属于提交内容，也便于把历史记录与具体提交一一对应）。
func contentionCalls(qtyA, qtyB, n int) ([]concurrentReserveCall, map[string]time.Time, map[string]time.Time) {
	expiryA := expiryOK
	expiryB := expiryOK.Add(day)
	calls := make([]concurrentReserveCall, 0, 2*n)
	nowA := make(map[string]time.Time, n)
	nowB := make(map[string]time.Time, n)
	for i := 0; i < 2*n; i++ {
		now := nowOK.Add(time.Duration(i) * time.Minute)
		if i%2 == 0 {
			calls = append(calls, concurrentReserveCall{"r1", qtyA, expiryA, now})
			nowA[now.String()] = now
		} else {
			calls = append(calls, concurrentReserveCall{"r2", qtyB, expiryB, now})
			nowB[now.String()] = now
		}
	}
	return calls, nowA, nowB
}

// assertContentionHistory 核对双方历史：获胜请求只有一条首次成功记录（序号 1、
// 当次资格合格、库存依据是新增占用前的账目）；落败请求每次提交各一条冲突
// 失败记录，序号按处理次序 1..n（赛后另提交时为 n+1），数量与到期时刻属于
// 落败内容、当前时刻与每次提交一一对应，资格与库存依据均为空。
func assertContentionHistory(
	t *testing.T,
	s *Store,
	winnerReq, loserReq string,
	winnerQty, loserQty int,
	winnerExpiry, loserExpiry time.Time,
	loserNows map[string]time.Time,
	loserRecords int,
) {
	t.Helper()

	wh, err := s.RequestHistory(winnerReq)
	if err != nil {
		t.Fatalf("winner history: %v", err)
	}
	if len(wh) != 1 {
		t.Fatalf("winner history len = %d, want 1 (first success only)", len(wh))
	}
	w := wh[0]
	if w.Seq != 1 || !w.Success || w.Error != "" {
		t.Fatalf("winner record marker = seq %d success %v error %q, want seq 1 success", w.Seq, w.Success, w.Error)
	}
	if w.CommitID != "cX" || w.PartID != "part1" || w.Quantity != winnerQty ||
		!w.Expiry.Equal(winnerExpiry) {
		t.Fatalf("winner record content = %+v, want qty %d expiry %v under %s", w, winnerQty, winnerExpiry, winnerReq)
	}
	if w.Eligibility == nil || !w.Eligibility.Eligible || w.Eligibility.RequestID != winnerReq ||
		len(w.Eligibility.Reasons) != 0 {
		t.Fatalf("winner eligibility snapshot = %+v, want eligible snapshot for %s", w.Eligibility, winnerReq)
	}
	// 首次成功处理前没有任何承诺：实物十件、占用零、可承诺十件。
	if w.StockBasis == nil || w.StockBasis.PhysicalRemaining != 10 ||
		w.StockBasis.ActiveOccupied != 0 || w.StockBasis.Committable != 10 {
		t.Fatalf("winner stock basis = %+v, want 10/0/10 before new occupancy", w.StockBasis)
	}

	lh, err := s.RequestHistory(loserReq)
	if err != nil {
		t.Fatalf("loser history: %v", err)
	}
	if len(lh) != loserRecords {
		t.Fatalf("loser history len = %d, want %d (one conflict per submission)", len(lh), loserRecords)
	}
	seenNows := make(map[string]struct{}, loserRecords)
	for i, rec := range lh {
		if rec.Seq != i+1 || rec.Success || rec.Error != HistoryErrorConflict {
			t.Fatalf("loser record %d = seq %d success %v error %q, want seq %d conflict",
				i, rec.Seq, rec.Success, rec.Error, i+1)
		}
		if rec.CommitID != "cX" || rec.PartID != "part1" || rec.Quantity != loserQty ||
			!rec.Expiry.Equal(loserExpiry) {
			t.Fatalf("loser record %d content = %+v, want qty %d expiry %v", i, rec, loserQty, loserExpiry)
		}
		// 冲突失败不做资格判断与库存核算：依据明确为空。
		if rec.Eligibility != nil || rec.StockBasis != nil {
			t.Fatalf("loser record %d basis = %+v / %+v, want nil/nil", i, rec.Eligibility, rec.StockBasis)
		}
		key := rec.Now.String()
		if _, ok := loserNows[key]; !ok {
			t.Fatalf("loser record %d now %v is not one of the loser submissions", i, rec.Now)
		}
		if _, dup := seenNows[key]; dup {
			t.Fatalf("loser record %d duplicates a submission now %v", i, rec.Now)
		}
		seenNows[key] = struct{}{}
	}
}

// TestConcurrentReserveSameIDDifferentRequestSingleWinner 对应用户给出的主例：
// 初始实物库存十件，两笔在保请求分别以三件与四件（到期时刻也不同）的合法内容
// 各并发提交多次，共用同一个尚未成功预留的承诺编号。库存本可同时容纳两笔，
// 但该编号最终只能对应一份内容：获胜组全部成功并返回同一份完整首次承诺，
// 落败组全部 ErrConflict；实物仍为十件，有效占用只能是三件或四件。
func TestConcurrentReserveSameIDDifferentRequestSingleWinner(t *testing.T) {
	const n = 16
	s := reserveContentionStore(t, 10)
	calls, nowA, nowB := contentionCalls(3, 4, n)
	results := fanOutConcurrentReserve(s, "cX", calls)

	// 获胜者只能由实际落库的承诺判定：编号下恰有一份内容。
	stored, err := s.Commitment("cX")
	if err != nil {
		t.Fatalf("no commitment bound to cX: %v", err)
	}
	winnerReq, loserReq := stored.RequestID, ""
	winnerQty, loserQty := stored.Quantity, 0
	winnerExpiry, loserExpiry := stored.Expiry, time.Time{}
	loserNows := nowB
	switch winnerReq {
	case "r1":
		loserReq, loserQty, loserExpiry, loserNows = "r2", 4, expiryOK.Add(day), nowB
	case "r2":
		loserReq, loserQty, loserExpiry, loserNows = "r1", 3, expiryOK, nowA
	default:
		t.Fatalf("bound commitment under unexpected request %q", winnerReq)
	}

	// 与获胜内容相同的调用全部成功、返回同一份完整首次承诺；落败组全部冲突，
	// 且失败返回值为空承诺。
	var first Commitment
	success, conflict := 0, 0
	for i, c := range calls {
		r := results[i]
		if c.requestID == winnerReq {
			if r.err != nil {
				t.Fatalf("winner-group call %d: %v", i, r.err)
			}
			if r.commitment != stored {
				t.Fatalf("winner-group call %d = %+v, want first commitment %+v", i, r.commitment, stored)
			}
			if r.commitment.Used != 0 || r.commitment.Canceled || r.commitment.Expired ||
				r.commitment.PartID != "part1" || r.commitment.RequestID != winnerReq ||
				r.commitment.Quantity != winnerQty || !r.commitment.Expiry.Equal(winnerExpiry) {
				t.Fatalf("winner-group call %d returned incomplete/altered first commitment: %+v", i, r.commitment)
			}
			first = r.commitment
			success++
		} else {
			if !errors.Is(r.err, ErrConflict) || r.commitment != (Commitment{}) {
				t.Fatalf("loser-group call %d = %+v, err %v; want ErrConflict with empty result",
					i, r.commitment, r.err)
			}
			conflict++
		}
	}
	if success != n || conflict != n {
		t.Fatalf("success=%d conflict=%d, want exactly %d/%d", success, conflict, n, n)
	}

	// 库存本可同时容纳两笔（3+4<=10），但有效占用只能是获胜数量，绝不能是七件。
	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != winnerQty ||
		st.Committable != 10-winnerQty || st.ActiveOccupied == 3+4 {
		t.Fatalf("stock = %+v, want phys=10 occupied=%d committable=%d (winner=%s qty %d)",
			st, st.ActiveOccupied, 10-winnerQty, winnerReq, winnerQty)
	}

	// 按备件查看只出现这一条承诺，内容与获胜内容一致、已用为零、状态有效。
	if len(st.Details) != 1 {
		t.Fatalf("part details = %d, want exactly 1", len(st.Details))
	}
	d := st.Details[0]
	if d.CommitmentID != "cX" || d.RequestID != winnerReq || d.PartID != "part1" ||
		d.OriginalQuantity != winnerQty || d.UsedQuantity != 0 ||
		d.RemainingQuantity != winnerQty || !d.Expiry.Equal(winnerExpiry) ||
		d.Status != CommitmentActive {
		t.Fatalf("sole detail = %+v, want winner %s qty %d used 0 active", d, winnerReq, winnerQty)
	}

	// 按请求查看：承诺只归属获胜请求；落败请求没有任何关联承诺。
	wv, err := s.RequestView(winnerReq, nowOK)
	if err != nil {
		t.Fatalf("winner view: %v", err)
	}
	if len(wv.Commitments) != 1 || wv.Commitments[0].CommitmentID != "cX" {
		t.Fatalf("winner view commitments = %+v, want only cX", wv.Commitments)
	}
	lv, err := s.RequestView(loserReq, nowOK)
	if err != nil {
		t.Fatalf("loser view: %v", err)
	}
	if len(lv.Commitments) != 0 {
		t.Fatalf("loser view commitments = %+v, want none", lv.Commitments)
	}

	// 历史与返回结果相互解释：获胜一条首次成功，落败每次提交各一条冲突。
	assertContentionHistory(t, s, winnerReq, loserReq, winnerQty, loserQty,
		winnerExpiry, loserExpiry, loserNows, n)

	// 争用结束后：获胜内容再次提交继续取回首次承诺（即使当前时刻更晚）；
	// 落败内容再次提交仍冲突，库存与归属保持原结果。
	later := nowOK.Add(2 * day)
	retryWinner, err := s.Reserve("cX", winnerReq, "part1", winnerQty, winnerExpiry, later)
	if err != nil || retryWinner != first {
		t.Fatalf("winner content retry = %+v, err %v; want first commitment %+v", retryWinner, err, first)
	}
	if _, err := s.Reserve("cX", loserReq, "part1", loserQty, loserExpiry, later); !errors.Is(err, ErrConflict) {
		t.Fatalf("loser content retry: got %v, want ErrConflict", err)
	}
	st2, _ := s.PartStatus("part1", later)
	if st2.PhysicalRemaining != 10 || st2.ActiveOccupied != winnerQty || st2.Committable != 10-winnerQty {
		t.Fatalf("post-hoc retries changed stock: %+v", st2)
	}
	if len(st2.Details) != 1 || st2.Details[0].RequestID != winnerReq {
		t.Fatalf("post-hoc details = %+v, want sole winner commitment", st2.Details)
	}
	// 获胜重试不追加历史；落败重试再留一条序号衔接的冲突记录。
	extendedLoserNows := make(map[string]time.Time, len(loserNows)+1)
	for k, v := range loserNows {
		extendedLoserNows[k] = v
	}
	extendedLoserNows[later.String()] = later
	assertContentionHistory(t, s, winnerReq, loserReq, winnerQty, loserQty,
		winnerExpiry, loserExpiry, extendedLoserNows, n+1)
	if lh, _ := s.RequestHistory(loserReq); lh[n].Now != later {
		t.Fatalf("latest loser record now = %v, want %v", lh[n].Now, later)
	}
}

// TestConcurrentReserveSameIDExactStockExhaustion 保障库存恰好被首次成功预留
// 用完的边界：初始库存五件，两笔请求各要五件。同内容的其余提交仍取回首次
// 结果，不得报库存不足；另一笔提交仍报编号冲突（而不是库存不足）。赛后
// 双方再次提交行为不变，库存、归属与历史沿用同一规则。
func TestConcurrentReserveSameIDExactStockExhaustion(t *testing.T) {
	const n = 12
	s := reserveContentionStore(t, 5)
	calls, nowA, nowB := contentionCalls(5, 5, n)
	results := fanOutConcurrentReserve(s, "cX", calls)

	stored, err := s.Commitment("cX")
	if err != nil {
		t.Fatalf("no commitment bound to cX: %v", err)
	}
	var winnerReq, loserReq string
	var winnerExpiry, loserExpiry time.Time
	var loserNows map[string]time.Time
	switch stored.RequestID {
	case "r1":
		winnerReq, loserReq = "r1", "r2"
		winnerExpiry, loserExpiry, loserNows = expiryOK, expiryOK.Add(day), nowB
	case "r2":
		winnerReq, loserReq = "r2", "r1"
		winnerExpiry, loserExpiry, loserNows = expiryOK.Add(day), expiryOK, nowA
	default:
		t.Fatalf("bound commitment under unexpected request %q", stored.RequestID)
	}

	for i, c := range calls {
		r := results[i]
		if c.requestID == winnerReq {
			// 库存恰好被首次预留用完：其余同内容提交取回首次结果，不能报库存不足。
			if r.err != nil || r.commitment != stored {
				t.Fatalf("winner-group call %d = %+v, err %v; want first commitment %+v, not stock error",
					i, r.commitment, r.err, stored)
			}
		} else {
			// 落败组即使在可承诺数量已为零之后处理，也只能得到编号冲突。
			if !errors.Is(r.err, ErrConflict) {
				t.Fatalf("loser-group call %d: got %v, want ErrConflict (not insufficient stock)", i, r.err)
			}
		}
	}

	st, _ := s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != 5 || st.ActiveOccupied != 5 || st.Committable != 0 {
		t.Fatalf("stock = %+v, want phys=5 occupied=5 committable=0", st)
	}
	if len(st.Details) != 1 {
		t.Fatalf("details = %d, want 1", len(st.Details))
	}
	d := st.Details[0]
	if d.CommitmentID != "cX" || d.RequestID != winnerReq || d.OriginalQuantity != 5 ||
		d.UsedQuantity != 0 || d.RemainingQuantity != 5 || !d.Expiry.Equal(winnerExpiry) ||
		d.Status != CommitmentActive {
		t.Fatalf("sole detail = %+v, want cX under %s qty 5 used 0 active", d, winnerReq)
	}
	wv, _ := s.RequestView(winnerReq, nowOK)
	lv, _ := s.RequestView(loserReq, nowOK)
	if len(wv.Commitments) != 1 || wv.Commitments[0].CommitmentID != "cX" {
		t.Fatalf("winner view = %+v, want only cX", wv.Commitments)
	}
	if len(lv.Commitments) != 0 {
		t.Fatalf("loser view = %+v, want no commitments", lv.Commitments)
	}

	// 首次成功的库存依据是占用前的五件可承诺；落败记录依据为空。
	wh, _ := s.RequestHistory(winnerReq)
	if len(wh) != 1 || !wh[0].Success || wh[0].StockBasis == nil ||
		wh[0].StockBasis.PhysicalRemaining != 5 || wh[0].StockBasis.ActiveOccupied != 0 ||
		wh[0].StockBasis.Committable != 5 {
		t.Fatalf("winner history = %+v, want single success with basis 5/0/5", wh)
	}
	lh, _ := s.RequestHistory(loserReq)
	if len(lh) != n {
		t.Fatalf("loser history len = %d, want %d", len(lh), n)
	}
	for i, rec := range lh {
		if rec.Seq != i+1 || rec.Error != HistoryErrorConflict || rec.Success ||
			rec.Eligibility != nil || rec.StockBasis != nil {
			t.Fatalf("loser record %d = %+v, want seq %d conflict with nil basis", i, rec, i+1)
		}
		if _, ok := loserNows[rec.Now.String()]; !ok {
			t.Fatalf("loser record %d now %v not from a loser submission", i, rec.Now)
		}
	}

	// 赛后：获胜内容再提交取回首次承诺；落败内容再提交仍冲突；零可承诺量不改变结果。
	later := nowOK.Add(2 * day)
	if got, err := s.Reserve("cX", winnerReq, "part1", 5, winnerExpiry, later); err != nil || got != stored {
		t.Fatalf("winner retry = %+v, err %v; want first commitment %+v", got, err, stored)
	}
	if _, err := s.Reserve("cX", loserReq, "part1", 5, loserExpiry, later); !errors.Is(err, ErrConflict) {
		t.Fatalf("loser retry: got %v, want ErrConflict", err)
	}
	st2, _ := s.PartStatus("part1", later)
	if st2.PhysicalRemaining != 5 || st2.ActiveOccupied != 5 || st2.Committable != 0 || len(st2.Details) != 1 {
		t.Fatalf("state after post-hoc retries = %+v, want unchanged 5/5/0 with one detail", st2)
	}
	wh2, _ := s.RequestHistory(winnerReq)
	lh2, _ := s.RequestHistory(loserReq)
	if len(wh2) != 1 {
		t.Fatalf("winner history grew to %d, want still 1", len(wh2))
	}
	if len(lh2) != n+1 || lh2[n].Seq != n+1 || lh2[n].Error != HistoryErrorConflict ||
		lh2[n].Now != later || lh2[n].Eligibility != nil || lh2[n].StockBasis != nil {
		t.Fatalf("loser post-hoc history = %+v, want appended seq %d conflict at %v", lh2, n+1, later)
	}
}
