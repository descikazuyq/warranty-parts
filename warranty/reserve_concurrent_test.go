package warranty

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障同一承诺编号在首次成功预留前被两组不同内容并发抢占时，编号
// 只能绑定一份内容：两笔已登记、在保且不命中除外规则的请求，向同一备件提交
// 预留，数量与到期时刻各自合法，却共用同一个尚未成功预留的承诺编号，每组内容
// 都可能同时被提交多次。无论哪组先成功，与其完全相同的提交全部成功并返回同一
// 份完整的首次承诺，另一组提交全部 ErrConflict：不能两组都成功，也不能把后来
// 的内容写进已成功的承诺。库存只承担获胜内容一份占用（实物库存不变），承诺只
// 归属于获胜请求；历史上获胜请求只留一条首次成功记录（保存当次资格与新增占用
// 前的库存依据），落败请求每次提交各留一条冲突失败记录（数量、到期时刻与当前
// 时刻保留，资格与库存依据均为空），序号按实际处理次序递增。这些测试只沿用
// 登记、资格、预留与查询的既有公开行为。

// concurrentReserveCall 是一次并发预留提交的入参。
type concurrentReserveCall struct {
	commitID  string
	requestID string
	partID    string
	quantity  int
	expiry    time.Time
	now       time.Time
}

// concurrentReserveResult 是一次并发预留提交的结果。
type concurrentReserveResult struct {
	commitment Commitment
	err        error
}

// reserveContent 是一组相同预留提交的内容（当前时刻不属于提交内容）。
type reserveContent struct {
	requestID string
	quantity  int
	expiry    time.Time
}

// fanOutConcurrentReserve 让全部调用在同一道闸机释放后同时进入 Reserve，尽量
// 制造交叉；返回与调用一一对应的结果，调用方按下标分组核对首次承诺与冲突。
func fanOutConcurrentReserve(s *Store, calls []concurrentReserveCall) []concurrentReserveResult {
	results := make([]concurrentReserveResult, len(calls))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c concurrentReserveCall) {
			defer wg.Done()
			<-start
			results[i].commitment, results[i].err =
				s.Reserve(c.commitID, c.requestID, c.partID, c.quantity, c.expiry, c.now)
		}(i, c)
	}
	close(start)
	wg.Wait()
	return results
}

// reserveContentionStore 构造同一产品下两笔在保、不命中除外规则的请求，以及
// 一个指定初始实物库存的备件；两者共用备件 part1。
func reserveContentionStore(t *testing.T, initialStock int) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", initialStock); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, id := range []string{"r1", "r2"} {
		if err := s.SubmitRequest(id, "p1", "FAULTY"); err != nil {
			t.Fatalf("submit request %s: %v", id, err)
		}
	}
	return s
}

// contentionCalls 构造两组各 n 个并发预留调用，交替排列以尽量交叉。两组使用
// 同一承诺编号与同一备件，仅请求、数量与到期时刻（即提交内容）不同；每个调用
// 给一个互不相同且都在保修期内、早于两笔到期时刻的当前时刻，便于核对落败记录
// 逐次保留当次当前时刻。
func contentionCalls(commitID, partID string, a, b reserveContent, n int) []concurrentReserveCall {
	calls := make([]concurrentReserveCall, 0, 2*n)
	for i := 0; i < n; i++ {
		calls = append(calls, concurrentReserveCall{
			commitID:  commitID,
			requestID: a.requestID,
			partID:    partID,
			quantity:  a.quantity,
			expiry:    a.expiry,
			now:       nowOK.Add(time.Duration(i) * time.Minute),
		})
		calls = append(calls, concurrentReserveCall{
			commitID:  commitID,
			requestID: b.requestID,
			partID:    partID,
			quantity:  b.quantity,
			expiry:    b.expiry,
			now:       nowOK.Add(time.Duration(n+i) * time.Minute),
		})
	}
	return calls
}

// contentMatches 判断某次调用是否与指定内容一致（到期时刻按实际时刻比较）。
func contentMatches(c concurrentReserveCall, ct reserveContent) bool {
	return c.requestID == ct.requestID && c.quantity == ct.quantity && c.expiry.Equal(ct.expiry)
}

// assertReserveContentionSingleWinner 核对两组不同内容并发抢占同一承诺编号后的
// 全部不变量，并在争用结束后再次按两组内容各提交两次：获胜内容继续取回首次
// 承诺且不追加历史，落败内容继续冲突且每次各追加一条同样规则的冲突记录，库存
// 与归属保持争用结束时的结果。physical 是备件始终不变的实物库存。
func assertReserveContentionSingleWinner(t *testing.T, s *Store, commitID string,
	calls []concurrentReserveCall, results []concurrentReserveResult, physical int, a, b reserveContent) {
	t.Helper()
	// 每组数量由调用长度推导。
	groupN := len(calls) / 2
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if len(st.Details) != 1 {
		t.Fatalf("details = %d, want exactly 1 commitment", len(st.Details))
	}
	d := st.Details[0]
	var win, lose reserveContent
	switch d.RequestID {
	case a.requestID:
		win, lose = a, b
	case b.requestID:
		win, lose = b, a
	default:
		t.Fatalf("winning detail request = %q, want %q or %q", d.RequestID, a.requestID, b.requestID)
	}
	wantFirst := Commitment{
		ID:        commitID,
		RequestID: win.requestID,
		PartID:    "part1",
		Quantity:  win.quantity,
		Expiry:    win.expiry,
	}

	// 明细字段与成功内容一致：原定数量、到期时刻对应胜者，已用为零、状态有效。
	if d.CommitmentID != commitID || d.PartID != "part1" ||
		d.OriginalQuantity != win.quantity || d.UsedQuantity != 0 ||
		d.RemainingQuantity != win.quantity || !d.Expiry.Equal(win.expiry) ||
		d.Status != CommitmentActive {
		t.Fatalf("winning detail = %+v, want %+v active/used=0", d, wantFirst)
	}
	// 实物库存不被预留扣减；有效占用只对应获胜数量，即使库存充足到能容纳两笔。
	if st.PhysicalRemaining != physical || st.ActiveOccupied != win.quantity ||
		st.Committable != physical-win.quantity {
		t.Fatalf("stock = %+v, want phys=%d occupied=%d committable=%d (winner=%s)",
			st, physical, win.quantity, physical-win.quantity, win.requestID)
	}
	// 活动承诺与首次承诺快照内容一致，已用为零、未取消、未到期。
	cur, err := s.Commitment(commitID)
	if err != nil {
		t.Fatalf("commitment: %v", err)
	}
	if cur != wantFirst {
		t.Fatalf("stored commitment = %+v, want %+v", cur, wantFirst)
	}

	// 按请求查看：承诺只归获胜请求，落败请求没有任何关联承诺。
	winView, err := s.RequestView(win.requestID, nowOK)
	if err != nil {
		t.Fatalf("winner view: %v", err)
	}
	if len(winView.Commitments) != 1 || winView.Commitments[0].CommitmentID != commitID {
		t.Fatalf("winner view commitments = %+v, want only %q", winView.Commitments, commitID)
	}
	loseView, err := s.RequestView(lose.requestID, nowOK)
	if err != nil {
		t.Fatalf("loser view: %v", err)
	}
	if len(loseView.Commitments) != 0 {
		t.Fatalf("loser view commitments = %+v, want none", loseView.Commitments)
	}

	// 逐调用核对：与获胜内容相同的全部成功并返回同一份完整首次承诺；落败内容
	// 全部 ErrConflict 且返回空承诺。即使获胜后可承诺量已为零，同内容的其余
	// 并发提交也取回首次结果，不得报库存不足。
	winnerNows := make(map[time.Time]struct{}, groupN)
	loserNows := make(map[time.Time]struct{}, groupN)
	success, conflict := 0, 0
	for i, c := range calls {
		r := results[i]
		if contentMatches(c, win) {
			winnerNows[c.now] = struct{}{}
			if r.err != nil || r.commitment != wantFirst {
				t.Fatalf("winner-group call %d = %+v, err %v; want first %+v",
					i, r.commitment, r.err, wantFirst)
			}
			if r.commitment.Used != 0 || r.commitment.Canceled || r.commitment.Expired {
				t.Fatalf("winner-group call %d returned non-first state: %+v", i, r.commitment)
			}
			success++
		} else if contentMatches(c, lose) {
			loserNows[c.now] = struct{}{}
			// 落败只可能是编号冲突：不能报库存不足等其他错误，也不能落库。
			if !errors.Is(r.err, ErrConflict) || r.commitment != (Commitment{}) {
				t.Fatalf("loser-group call %d = %+v, err %v; want ErrConflict with empty result",
					i, r.commitment, r.err)
			}
			conflict++
		} else {
			t.Fatalf("call %d matches neither content: %+v", i, c)
		}
	}
	if success != groupN || conflict != groupN {
		t.Fatalf("success=%d conflict=%d, want exactly %d/%d", success, conflict, groupN, groupN)
	}

	// 获胜请求只留一条首次成功记录；其余相同内容的成功返回不追加。
	winHist, err := s.RequestHistory(win.requestID)
	if err != nil {
		t.Fatalf("winner history: %v", err)
	}
	if len(winHist) != 1 {
		t.Fatalf("winner history len = %d, want 1", len(winHist))
	}
	wr := winHist[0]
	if wr.Seq != 1 || !wr.Success || wr.Error != "" ||
		wr.CommitID != commitID || wr.PartID != "part1" ||
		wr.Quantity != win.quantity || !wr.Expiry.Equal(win.expiry) {
		t.Fatalf("winner success record = %+v", wr)
	}
	if _, ok := winnerNows[wr.Now]; !ok {
		t.Fatalf("winner record now %v not among winning submissions %v", wr.Now, winnerNows)
	}
	// 成功记录保存当次资格依据：请求合格、不命中除外。
	if wr.Eligibility == nil || !wr.Eligibility.Eligible || wr.Eligibility.Excluded ||
		wr.Eligibility.RequestID != win.requestID || len(wr.Eligibility.Reasons) != 0 {
		t.Fatalf("winner eligibility snapshot = %+v", wr.Eligibility)
	}
	// 成功记录保存新增占用前的库存依据：实物 %d、占用 0、可承诺 %d。
	if wr.StockBasis == nil ||
		wr.StockBasis.PhysicalRemaining != physical || wr.StockBasis.ActiveOccupied != 0 ||
		wr.StockBasis.Committable != physical {
		t.Fatalf("winner stock basis = %+v, want phys=%d occupied=0 committable=%d",
			wr.StockBasis, physical, physical)
	}

	// 落败请求每次提交各留一条冲突失败记录，序号按处理次序连续递增，不挂到
	// 获胜请求下；数量、到期时刻与当前时刻逐次保留，资格与库存依据均为空。
	loseHist, err := s.RequestHistory(lose.requestID)
	if err != nil {
		t.Fatalf("loser history: %v", err)
	}
	if len(loseHist) != groupN {
		t.Fatalf("loser history len = %d, want %d (one conflict per submission)", len(loseHist), groupN)
	}
	seenNows := make(map[time.Time]struct{}, groupN)
	for i, rec := range loseHist {
		if rec.Seq != i+1 || rec.Success || rec.Error != HistoryErrorConflict {
			t.Fatalf("loser record %d = %+v, want seq=%d conflict failure", i, rec, i+1)
		}
		if rec.CommitID != commitID || rec.PartID != "part1" ||
			rec.Quantity != lose.quantity || !rec.Expiry.Equal(lose.expiry) {
			t.Fatalf("loser record %d submission fields = %+v", i, rec)
		}
		if _, ok := loserNows[rec.Now]; !ok {
			t.Fatalf("loser record %d now %v not among losing submissions", i, rec.Now)
		}
		if _, dup := seenNows[rec.Now]; dup {
			t.Fatalf("loser record %d duplicated now %v", i, rec.Now)
		}
		seenNows[rec.Now] = struct{}{}
		if rec.Eligibility != nil || rec.StockBasis != nil {
			t.Fatalf("loser record %d basis must be nil: elig=%+v stock=%+v",
				i, rec.Eligibility, rec.StockBasis)
		}
	}

	// 争用结束后：获胜内容再次提交继续返回首次承诺（即使此刻可承诺量为零也
	// 不报库存不足），不追加历史；落败内容再次提交仍冲突，每次各追加一条同样
	// 规则的冲突记录；库存与归属保持原结果。
	for k := 0; k < 2; k++ {
		postNow := nowOK.Add(time.Duration(2*groupN+k) * time.Hour)
		again, err := s.Reserve(commitID, win.requestID, "part1", win.quantity, win.expiry, postNow)
		if err != nil || again != wantFirst {
			t.Fatalf("post-hoc winner submit %d = %+v, err %v; want first %+v",
				k, again, err, wantFirst)
		}
		h, _ := s.RequestHistory(win.requestID)
		if len(h) != 1 {
			t.Fatalf("post-hoc winner submit %d appended history: len=%d", k, len(h))
		}

		loseNow := postNow.Add(30 * time.Minute)
		got, err := s.Reserve(commitID, lose.requestID, "part1", lose.quantity, lose.expiry, loseNow)
		if !errors.Is(err, ErrConflict) || got != (Commitment{}) {
			t.Fatalf("post-hoc loser submit %d = %+v, err %v; want ErrConflict", k, got, err)
		}
		lh, _ := s.RequestHistory(lose.requestID)
		wantLen := groupN + k + 1
		if len(lh) != wantLen {
			t.Fatalf("post-hoc loser history len = %d, want %d", len(lh), wantLen)
		}
		lr := lh[wantLen-1]
		if lr.Seq != wantLen || lr.Success || lr.Error != HistoryErrorConflict ||
			lr.Quantity != lose.quantity || !lr.Expiry.Equal(lose.expiry) ||
			!lr.Now.Equal(loseNow) || lr.Eligibility != nil || lr.StockBasis != nil {
			t.Fatalf("post-hoc loser record = %+v, want seq=%d conflict with nil basis", lr, wantLen)
		}
	}

	// 赛后库存与归属不变。
	final, _ := s.PartStatus("part1", nowOK)
	if final.PhysicalRemaining != physical || final.ActiveOccupied != win.quantity ||
		final.Committable != physical-win.quantity || len(final.Details) != 1 ||
		final.Details[0].RequestID != win.requestID {
		t.Fatalf("post-hoc state changed: %+v", final)
	}
	cur2, _ := s.Commitment(commitID)
	if cur2 != wantFirst {
		t.Fatalf("post-hoc commitment changed: %+v, want %+v", cur2, wantFirst)
	}
}

// TestConcurrentReserveSameIDDifferentContentSingleWinner 对应用户给出的主例：
// 初始实物库存十件，两笔在保、不命中除外的请求分别要三件和四件（到期时刻也
// 不同），各组同时多次提交同一个尚未成功预留的承诺编号。只能有一份内容成为
// 首次承诺：同内容提交全部成功并返回同一份承诺，另一笔全部 ErrConflict；处理
// 完实物仍为十件，有效占用与可承诺量只对应获胜数量，承诺与历史只归获胜请求。
func TestConcurrentReserveSameIDDifferentContentSingleWinner(t *testing.T) {
	const commitID = "cX"
	contentA := reserveContent{requestID: "r1", quantity: 3, expiry: expiryOK}
	contentB := reserveContent{requestID: "r2", quantity: 4, expiry: expiryOK.Add(10 * day)}

	const n = 12
	for iter := 0; iter < 5; iter++ {
		t.Run("iteration", func(t *testing.T) {
			s := reserveContentionStore(t, 10)
			calls := contentionCalls(commitID, "part1", contentA, contentB, n)
			results := fanOutConcurrentReserve(s, calls)
			assertReserveContentionSingleWinner(t, s, commitID, calls, results, 10, contentA, contentB)
		})
	}
}

// TestConcurrentReserveSameIDExactStockWinnerTakesAll 保障库存恰好被首次成功
// 预留用完的情况：初始库存五件，两笔请求各要五件（仅请求不同）。同内容的其余
// 提交仍取回首次结果，不得因可承诺量归零而报库存不足；另一笔仍报编号冲突而非
// 库存不足。赛后获胜内容再次提交继续返回首次承诺，落败内容再次提交仍冲突，
// 库存五件全部由获胜承诺占用、归属保持不变。
func TestConcurrentReserveSameIDExactStockWinnerTakesAll(t *testing.T) {
	const commitID = "cFull"
	contentA := reserveContent{requestID: "r1", quantity: 5, expiry: expiryOK}
	contentB := reserveContent{requestID: "r2", quantity: 5, expiry: expiryOK}

	const n = 12
	for iter := 0; iter < 5; iter++ {
		t.Run("iteration", func(t *testing.T) {
			s := reserveContentionStore(t, 5)
			calls := contentionCalls(commitID, "part1", contentA, contentB, n)
			results := fanOutConcurrentReserve(s, calls)
			assertReserveContentionSingleWinner(t, s, commitID, calls, results, 5, contentA, contentB)
		})
	}
}
