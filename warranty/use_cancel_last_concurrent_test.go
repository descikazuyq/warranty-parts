package warranty

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障“同一笔备件承诺的最后一次领取与取消同时提交”：两项操作沿用
// 既有的领取（Use）、取消（Cancel）与查询（Commitment/RequestView/
// PartStatus/CommitmentUsages）公开行为，取消只释放当时尚未领取的占用。
//
// 业务主例（场景限定在产品仍在保、故障未被除外、所有承诺都未到期的时刻）：
//
//	备件 ucPart 初始库存十件；
//	ucSix ：请求 ucReq1 的承诺预留六件；
//	ucFour：请求 ucReq2 的承诺预留四件；
//	ucSix 已成功领取两件（ucFirstUse）。
//	此时实物剩八件，两笔承诺合计占用八件（ucSix 未用四件 + ucFour 四件），
//	可承诺数量为零。
//
// 用一个尚未成功使用过的编号 ucLastUse，从 ucSix 领取剩下的四件，同时取消
// ucSix；两次操作使用同一个早于到期的当前时刻。两种先后关系都必须自洽：
//
//   - 四件领取先成功、取消也成功：ucSix 最终 canceled，原定六件、已用六件、
//     未用零件；取消返回的承诺同样保留六件已用事实。实物剩四件，ucFour 仍占用
//     四件，可承诺数量为零。成功使用明细保留此前两件与本次四件。
//   - 取消先完成：本次四件领取返回 ErrCommitmentClosed；ucSix canceled，原定
//     六件、已用两件、未用四件；取消返回的承诺保留两件已用事实。实物仍八件，
//     ucFour 占用四件，可承诺数量变为四件。被拒绝的领取不出现在成功使用明细中，
//     明细数量之和仍为两件；取消记录里的四件未用数量继续作为原承诺的事实展示，
//     但不再计入有效占用。
//
// 无论哪种先后结果，ucFour 的请求归属、原定数量、已用数量和到期时刻都保持原值，
// 不能借取消 ucSix 释放它的占用。并发主测试接受其中任意一种完整结果，不依赖某
// 一种执行顺序总会获胜；另两个测试分别确定地覆盖两种先后关系。

const (
	ucPart     = "ucPart1"
	ucReq1     = "ucReq1"
	ucReq2     = "ucReq2"
	ucSix      = "ucSix"
	ucFour     = "ucFour"
	ucFirstUse = "ucUsed2"
	ucLastUse  = "ucUsed4"
	ucExtraUse = "ucUsedAfter"
)

var (
	ucExpSix  = t0.Add(40 * day)
	ucExpFour = t0.Add(30 * day)
	// ucRaceNow 是领取四件与取消并发提交使用的同一个当前时刻，早于全部承诺
	// 到期时刻，也在保修期内；查询沿用稍后但仍早于最早到期时刻的 ucCheckNow。
	ucRaceNow  = nowOK.Add(time.Hour)
	ucCheckNow = ucRaceNow.Add(time.Hour)
)

// ucRaceStore 构造上述业务主例并断言并发前的初始账目：实物八件、有效占用
// 八件（ucSix 未用四件 + ucFour 四件）、可承诺数量为零。
func ucRaceStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("ucP1", t0, 60, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(ucPart, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, id := range []string{ucReq1, ucReq2} {
		if err := s.SubmitRequest(id, "ucP1", "FAULTY"); err != nil {
			t.Fatalf("submit request %s: %v", id, err)
		}
	}
	if _, err := s.Reserve(ucSix, ucReq1, ucPart, 6, ucExpSix, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", ucSix, err)
	}
	if _, err := s.Reserve(ucFour, ucReq2, ucPart, 4, ucExpFour, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", ucFour, err)
	}
	if _, err := s.Use(ucFirstUse, ucSix, 2, nowOK); err != nil {
		t.Fatalf("seed use 2 from %s: %v", ucSix, err)
	}
	st, err := s.PartStatus(ucPart, ucCheckNow)
	if err != nil {
		t.Fatalf("part status baseline: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 8 || st.Committable != 0 {
		t.Fatalf("baseline account: phys=%d occupied=%d committable=%d, want 8/8/0",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	return s
}

// ucRaceResult 收集一次并发“领取四件 + 取消”的两个返回值。
type ucRaceResult struct {
	used      Usage
	useErr    error
	canceled  Commitment
	cancelErr error
}

// fanOutUseCancel 让从 ucSix 领取四件与取消 ucSix 在同一道闸机释放后同时进入
// 仓库，尽量制造交叉；两者的实际先后不由测试预设。
func fanOutUseCancel(t *testing.T, s *Store, now time.Time) ucRaceResult {
	t.Helper()
	var rr ucRaceResult
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		rr.used, rr.useErr = s.Use(ucLastUse, ucSix, 4, now)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		rr.canceled, rr.cancelErr = s.Cancel(ucSix, now)
	}()
	close(start)
	wg.Wait()
	return rr
}

// assertUCSecondCommitmentUntouched 核对 ucFour 与本次并发结果完全无关：请求
// 归属、原定数量、已用数量、未用数量、备件归属、到期时刻与有效状态保持原值。
func assertUCSecondCommitmentUntouched(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	got, err := s.Commitment(ucFour)
	if err != nil {
		t.Fatalf("commitment %s: %v", ucFour, err)
	}
	if got.Canceled || got.Expired || got.Quantity != 4 || got.Used != 0 ||
		got.Unused() != 4 || got.RequestID != ucReq2 || got.PartID != ucPart ||
		!got.Expiry.Equal(ucExpFour) {
		t.Fatalf("second commitment disturbed: %+v, want active 4/0/4 owned by %s expiry %v",
			got, ucReq2, ucExpFour)
	}
	// 按请求查看：ucReq2 只看到自己这一笔保持原值的承诺。
	view, err := s.RequestView(ucReq2, now)
	if err != nil {
		t.Fatalf("request view %s: %v", ucReq2, err)
	}
	if len(view.Commitments) != 1 {
		t.Fatalf("%s commitments = %d, want 1", ucReq2, len(view.Commitments))
	}
	d := view.Commitments[0]
	if d.CommitmentID != ucFour || d.Status != CommitmentActive ||
		d.OriginalQuantity != 4 || d.UsedQuantity != 0 || d.RemainingQuantity != 4 ||
		d.RequestID != ucReq2 || d.PartID != ucPart || !d.Expiry.Equal(ucExpFour) {
		t.Fatalf("second request view changed after use/cancel race: %+v", d)
	}
	// 第二笔承诺没有任何成功使用记录。
	if usages, err := s.CommitmentUsages(ucFour); err != nil || len(usages) != 0 {
		t.Fatalf("usages of second commitment = %+v err %v, want empty", usages, err)
	}
}

// assertUCAccountConsistent 交叉核对账目自洽性：可承诺 = 实物 − 有效占用；
// 明细中 active 记录的未用数量之和必须恰好等于有效占用，已取消记录的未用数量
// 只作为事实展示、不参与求和。
func assertUCAccountConsistent(t *testing.T, st *PartStatus) {
	t.Helper()
	if st.PhysicalRemaining-st.ActiveOccupied != st.Committable {
		t.Fatalf("committable %d != physical %d - occupied %d",
			st.Committable, st.PhysicalRemaining, st.ActiveOccupied)
	}
	activeUnused := 0
	for _, d := range st.Details {
		if d.Status == CommitmentActive {
			activeUnused += d.RemainingQuantity
		}
	}
	if activeUnused != st.ActiveOccupied {
		t.Fatalf("active details unused sum = %d, want ActiveOccupied %d", activeUnused, st.ActiveOccupied)
	}
}

// TestConcurrentLastUseAndCancelEitherOrderValid 主并发回归：从 ucSix 领取剩余
// 四件与取消 ucSix 同时提交，不预设处理先后。每次迭代只接受两种完整结果之一：
// 领取先成功（4/4/0 账目、两条成功使用明细）或取消先完成（领取被
// ErrCommitmentClosed 拒绝、8/4/4 账目、明细仍只有两件），且两种结果下取消
// 返回的承诺、查询记录与成功使用明细都必须与实际结果对应同一种事实。
func TestConcurrentLastUseAndCancelEitherOrderValid(t *testing.T) {
	const iterations = 50
	var sawUseFirst, sawCancelFirst int
	for i := 0; i < iterations; i++ {
		t.Run("iteration", func(t *testing.T) {
			s := ucRaceStore(t)
			rr := fanOutUseCancel(t, s, ucRaceNow)
			useFirst := rr.useErr == nil
			if useFirst {
				sawUseFirst++
			} else if errors.Is(rr.useErr, ErrCommitmentClosed) {
				sawCancelFirst++
			} else {
				t.Fatalf("concurrent four-piece use returned unexpected error: %v (usage %+v)",
					rr.useErr, rr.used)
			}
			assertUCOutcome(t, s, rr, useFirst, ucCheckNow)
		})
	}
	// 仅作信息记录：两种先后关系在足量迭代下通常都会真实发生；即使某次运行
	// 调度只给出一侧，assertUCOutcome 也已覆盖该侧的全部不变量。
	t.Logf("across %d iterations: use-before-cancel=%d, cancel-before-use=%d",
		iterations, sawUseFirst, sawCancelFirst)
}

// assertUCOutcome 在并发结束后按实际发生的先后关系核对全部不变量。
func assertUCOutcome(t *testing.T, s *Store, rr ucRaceResult, useFirst bool, now time.Time) {
	t.Helper()

	// 取消无论先后都成功。
	if rr.cancelErr != nil {
		t.Fatalf("concurrent cancel: %v", rr.cancelErr)
	}

	st, err := s.PartStatus(ucPart, now)
	if err != nil {
		t.Fatalf("part status after race: %v", err)
	}
	if len(st.Details) != 2 {
		t.Fatalf("part details len = %d, want 2 (ucSix canceled + ucFour active)", len(st.Details))
	}
	dSix := mustDetailByID(t, st.Details, ucSix)
	if dSix.Status != CommitmentCanceled || dSix.RequestID != ucReq1 ||
		dSix.PartID != ucPart || dSix.OriginalQuantity != 6 || !dSix.Expiry.Equal(ucExpSix) {
		t.Fatalf("ucSix detail = %+v, want canceled original 6 owned by %s", dSix, ucReq1)
	}

	stored, err := s.Commitment(ucSix)
	if err != nil {
		t.Fatalf("commitment %s: %v", ucSix, err)
	}
	if !stored.Canceled || stored.Expired || stored.Quantity != 6 ||
		stored.RequestID != ucReq1 || stored.PartID != ucPart ||
		!stored.Expiry.Equal(ucExpSix) {
		t.Fatalf("stored ucSix = %+v, want canceled qty=6", stored)
	}

	switch {
	case useFirst:
		// 四件领取先成功：返回完整使用记录；随后取消也成功。
		wantUsage := Usage{ID: ucLastUse, CommitmentID: ucSix, Quantity: 4}
		if rr.used != wantUsage {
			t.Fatalf("four-piece use result = %+v, want %+v", rr.used, wantUsage)
		}
		// 取消返回的承诺必须保留这六件已用事实，未用为零。
		if !rr.canceled.Canceled || rr.canceled.Quantity != 6 ||
			rr.canceled.Used != 6 || rr.canceled.Unused() != 0 ||
			rr.canceled.RequestID != ucReq1 || !rr.canceled.Expiry.Equal(ucExpSix) {
			t.Fatalf("cancel result on use-first branch = %+v, want canceled 6/6/0", rr.canceled)
		}
		if stored.Used != 6 || stored.Unused() != 0 {
			t.Fatalf("stored ucSix on use-first branch = %+v, want used=6 unused=0", stored)
		}
		if dSix.UsedQuantity != 6 || dSix.RemainingQuantity != 0 {
			t.Fatalf("ucSix detail on use-first branch = %+v, want 6/0", dSix)
		}
		// 实物剩四件（再扣四件），有效占用只剩 ucFour 的四件，可承诺为零。
		if st.PhysicalRemaining != 4 || st.ActiveOccupied != 4 || st.Committable != 0 {
			t.Fatalf("account on use-first branch: phys=%d occupied=%d committable=%d, want 4/4/0",
				st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
		}
		// 成功使用明细保留之前两件与本次四件，按使用编号升序，合计六件。
		assertUCUsages(t, s, []Usage{
			{ID: ucFirstUse, CommitmentID: ucSix, Quantity: 2},
			{ID: ucLastUse, CommitmentID: ucSix, Quantity: 4},
		})

	case errors.Is(rr.useErr, ErrCommitmentClosed):
		// 取消先完成：四件领取被拒绝，返回空使用记录，不产生成功使用明细。
		if rr.used != (Usage{}) {
			t.Fatalf("rejected four-piece use returned record %+v, want empty", rr.used)
		}
		// 取消返回的承诺保留两件已用事实，未用四件只是展示事实。
		if !rr.canceled.Canceled || rr.canceled.Quantity != 6 ||
			rr.canceled.Used != 2 || rr.canceled.Unused() != 4 ||
			rr.canceled.RequestID != ucReq1 || !rr.canceled.Expiry.Equal(ucExpSix) {
			t.Fatalf("cancel result on cancel-first branch = %+v, want canceled 6/2/4", rr.canceled)
		}
		if stored.Used != 2 || stored.Unused() != 4 {
			t.Fatalf("stored ucSix on cancel-first branch = %+v, want used=2 unused=4", stored)
		}
		if dSix.UsedQuantity != 2 || dSix.RemainingQuantity != 4 {
			t.Fatalf("ucSix detail on cancel-first branch = %+v, want 6/2/4 canceled", dSix)
		}
		// 实物仍八件，有效占用四件（只剩 ucFour），可承诺变为四件。
		if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
			t.Fatalf("account on cancel-first branch: phys=%d occupied=%d committable=%d, want 8/4/4",
				st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
		}
		// 被拒绝的领取不出现在成功使用明细中，明细仍只有此前两件，合计两件。
		assertUCUsages(t, s, []Usage{
			{ID: ucFirstUse, CommitmentID: ucSix, Quantity: 2},
		})

	default:
		t.Fatalf("unreachable: non-closed use error %v", rr.useErr)
	}

	assertUCAccountConsistent(t, st)

	// 按请求查看 ucReq1：只看到这一笔已取消承诺，数量与实际结果一致。
	view1, err := s.RequestView(ucReq1, now)
	if err != nil {
		t.Fatalf("request view %s: %v", ucReq1, err)
	}
	if view1.Eligibility == nil || !view1.Eligibility.Eligible {
		t.Fatalf("%s eligibility = %+v, want still eligible", ucReq1, view1.Eligibility)
	}
	if len(view1.Commitments) != 1 {
		t.Fatalf("%s commitments = %d, want 1", ucReq1, len(view1.Commitments))
	}
	d1 := view1.Commitments[0]
	wantUsed, wantRemaining := 2, 4
	if useFirst {
		wantUsed, wantRemaining = 6, 0
	}
	if d1.CommitmentID != ucSix || d1.Status != CommitmentCanceled ||
		d1.OriginalQuantity != 6 || d1.UsedQuantity != wantUsed ||
		d1.RemainingQuantity != wantRemaining || d1.RequestID != ucReq1 ||
		!d1.Expiry.Equal(ucExpSix) {
		t.Fatalf("ucReq1 view = %+v, want canceled 6/%d/%d", d1, wantUsed, wantRemaining)
	}

	// 第二笔承诺的请求归属、原数量、已用数量和到期时刻保持原值。
	assertUCSecondCommitmentUntouched(t, s, now)

	// 赛后再用新编号向已取消的 ucSix 领取，无论此前哪一侧获胜，都一律关闭且
	// 不改变任何数量与明细。
	if _, err := s.Use(ucExtraUse, ucSix, 1, now); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new use after race: got %v, want ErrCommitmentClosed", err)
	}
	st2, err := s.PartStatus(ucPart, now)
	if err != nil {
		t.Fatalf("part status after post-race rejected use: %v", err)
	}
	if st2.PhysicalRemaining != st.PhysicalRemaining ||
		st2.ActiveOccupied != st.ActiveOccupied ||
		st2.Committable != st.Committable {
		t.Fatalf("post-race rejected use changed account: before %+v after %+v", st, st2)
	}
	post, _ := s.Commitment(ucSix)
	if post.Used != stored.Used || !post.Canceled {
		t.Fatalf("post-race rejected use changed ucSix: %+v, want used=%d canceled", post, stored.Used)
	}

	// 两条既有成功使用（取消先完成一侧只有第一件）原样重试仍取回首次记录，
	// 不再次扣减、不改变账目。
	wantPhys, wantOcc, wantCommit := st.PhysicalRemaining, st.ActiveOccupied, st.Committable
	if first, err := s.Use(ucFirstUse, ucSix, 2, now); err != nil ||
		first != (Usage{ID: ucFirstUse, CommitmentID: ucSix, Quantity: 2}) {
		t.Fatalf("retry original 2-piece usage = %+v err %v", first, err)
	}
	if useFirst {
		if last, err := s.Use(ucLastUse, ucSix, 4, now); err != nil ||
			last != (Usage{ID: ucLastUse, CommitmentID: ucSix, Quantity: 4}) {
			t.Fatalf("retry four-piece usage = %+v err %v", last, err)
		}
	}
	st3, _ := s.PartStatus(ucPart, now)
	if st3.PhysicalRemaining != wantPhys || st3.ActiveOccupied != wantOcc ||
		st3.Committable != wantCommit {
		t.Fatalf("idempotent retries changed account: want %d/%d/%d, got %d/%d/%d",
			wantPhys, wantOcc, wantCommit,
			st3.PhysicalRemaining, st3.ActiveOccupied, st3.Committable)
	}
}

// assertUCUsages 核对 ucSix 的成功使用明细与期望记录完全一致：按使用编号升序，
// 每条编号、承诺与数量相符，数量之和即承诺已用数量。
func assertUCUsages(t *testing.T, s *Store, want []Usage) {
	t.Helper()
	got, err := s.CommitmentUsages(ucSix)
	if err != nil {
		t.Fatalf("usages of %s: %v", ucSix, err)
	}
	if len(got) != len(want) {
		t.Fatalf("usages len = %d, want %d (%+v)", len(got), len(want), got)
	}
	total := 0
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("usage[%d] = %+v, want %+v; full list %+v", i, got[i], w, got)
		}
		total += w.Quantity
	}
	c, err := s.Commitment(ucSix)
	if err != nil {
		t.Fatalf("commitment %s: %v", ucSix, err)
	}
	if c.Used != total {
		t.Fatalf("usage total %d != commitment used %d", total, c.Used)
	}
}

// TestLastUseSucceedsThenCancelReleasesZero 确定地覆盖“四件领取先成功”的先后
// 关系：四件领取成功后取消，取消只释放当时尚未领取的占用——此时未用已为零，
// 故实物四件、有效占用四件（ucFour）、可承诺为零；取消返回的承诺保留六件已用
// 事实，成功使用明细保留两件加四件。
func TestLastUseSucceedsThenCancelReleasesZero(t *testing.T) {
	s := ucRaceStore(t)

	u, err := s.Use(ucLastUse, ucSix, 4, ucRaceNow)
	if err != nil {
		t.Fatalf("final four-piece use: %v", err)
	}
	if u != (Usage{ID: ucLastUse, CommitmentID: ucSix, Quantity: 4}) {
		t.Fatalf("use result = %+v", u)
	}
	// 领取后、取消前：ucSix 已用六件、未用零件，但仍 active，故未用零不构成
	// 占用；实物四件、有效占用四件（ucFour）、可承诺为零。
	mid, _ := s.PartStatus(ucPart, ucCheckNow)
	if mid.PhysicalRemaining != 4 || mid.ActiveOccupied != 4 || mid.Committable != 0 {
		t.Fatalf("account after final use before cancel: %+v, want 4/4/0", mid)
	}
	midSix := mustCommitment(t, s, ucSix)
	if midSix.Canceled || midSix.Used != 6 || midSix.Unused() != 0 {
		t.Fatalf("ucSix between use and cancel = %+v, want active 6/6/0", midSix)
	}

	canceled, err := s.Cancel(ucSix, ucRaceNow)
	if err != nil {
		t.Fatalf("cancel after final use: %v", err)
	}
	// 取消返回的承诺保留六件已用事实，释放的未用数量为零。
	if !canceled.Canceled || canceled.Quantity != 6 || canceled.Used != 6 ||
		canceled.Unused() != 0 || !canceled.Expiry.Equal(ucExpSix) {
		t.Fatalf("cancel result = %+v, want canceled 6/6/0", canceled)
	}

	// 重复取消不再释放任何数量，返回仍是 6/6/0 的取消事实。
	again, err := s.Cancel(ucSix, ucCheckNow)
	if err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	if !again.Canceled || again.Used != 6 || again.Unused() != 0 {
		t.Fatalf("repeat cancel result = %+v, want canceled 6/6/0", again)
	}

	st, _ := s.PartStatus(ucPart, ucCheckNow)
	if st.PhysicalRemaining != 4 || st.ActiveOccupied != 4 || st.Committable != 0 {
		t.Fatalf("final account: %+v, want phys=4 occupied=4 committable=0", st)
	}
	assertUCAccountConsistent(t, st)
	dSix := mustDetailByID(t, st.Details, ucSix)
	if dSix.Status != CommitmentCanceled || dSix.OriginalQuantity != 6 ||
		dSix.UsedQuantity != 6 || dSix.RemainingQuantity != 0 {
		t.Fatalf("ucSix detail = %+v, want canceled 6/6/0", dSix)
	}
	assertUCUsages(t, s, []Usage{
		{ID: ucFirstUse, CommitmentID: ucSix, Quantity: 2},
		{ID: ucLastUse, CommitmentID: ucSix, Quantity: 4},
	})
	assertUCSecondCommitmentUntouched(t, s, ucCheckNow)

	// 取消后该使用编号原样重试仍取回首次四件记录，不再次扣减。
	retry, err := s.Use(ucLastUse, ucSix, 4, ucCheckNow)
	if err != nil || retry != u {
		t.Fatalf("retry final usage = %+v err %v, want %+v", retry, err, u)
	}
	st2, _ := s.PartStatus(ucPart, ucCheckNow)
	if st2.PhysicalRemaining != 4 || st2.ActiveOccupied != 4 || st2.Committable != 0 {
		t.Fatalf("retry changed account: %+v", st2)
	}
}

// TestCancelFirstThenLastUseRejectedClosed 确定地覆盖“取消先完成”的先后关系：
// 取消只释放当时尚未领取的四件占用，随后四件领取返回 ErrCommitmentClosed 且不
// 产生明细；ucSix 保留原定六件、已用两件、未用四件的取消事实；实物八件、有效
// 占用四件（ucFour）、可承诺四件。被拒绝的编号从未成功，改指向仍有效的
// ucFour 可以绑定成功——但为保持 ucFour 原值不变，该交叉验证放在独立副本上。
func TestCancelFirstThenLastUseRejectedClosed(t *testing.T) {
	s := ucRaceStore(t)

	canceled, err := s.Cancel(ucSix, ucRaceNow)
	if err != nil {
		t.Fatalf("cancel before final use: %v", err)
	}
	// 取消返回的承诺保留两件已用事实，释放的是尚未领取的四件。
	if !canceled.Canceled || canceled.Quantity != 6 || canceled.Used != 2 ||
		canceled.Unused() != 4 || canceled.RequestID != ucReq1 ||
		!canceled.Expiry.Equal(ucExpSix) {
		t.Fatalf("cancel result = %+v, want canceled 6/2/4", canceled)
	}
	st, _ := s.PartStatus(ucPart, ucCheckNow)
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("account after cancel: %+v, want 8/4/4", st)
	}

	// 四件领取被关闭拒绝：返回空使用记录，不扣实物、不增明细、不改变已用。
	u, err := s.Use(ucLastUse, ucSix, 4, ucRaceNow)
	if !errors.Is(err, ErrCommitmentClosed) || u != (Usage{}) {
		t.Fatalf("final use after cancel = %+v err %v, want ErrCommitmentClosed empty", u, err)
	}
	st2, _ := s.PartStatus(ucPart, ucCheckNow)
	if st2.PhysicalRemaining != 8 || st2.ActiveOccupied != 4 || st2.Committable != 4 {
		t.Fatalf("rejected use changed account: %+v, want 8/4/4", st2)
	}
	six := mustCommitment(t, s, ucSix)
	if !six.Canceled || six.Quantity != 6 || six.Used != 2 || six.Unused() != 4 {
		t.Fatalf("ucSix after rejected use = %+v, want canceled 6/2/4", six)
	}
	assertUCAccountConsistent(t, st2)
	assertUCUsages(t, s, []Usage{
		{ID: ucFirstUse, CommitmentID: ucSix, Quantity: 2},
	})

	// 被拒绝的编号未被占用：沿用同一编号改指向仍有效的 ucFour 属于首次成功
	// 提交，正常绑定并扣减（ucFour 未用四件恰好容纳）。
	rebound, err := s.Use(ucLastUse, ucFour, 4, ucCheckNow)
	if err != nil {
		t.Fatalf("reuse rejected id on active commitment: %v", err)
	}
	if rebound != (Usage{ID: ucLastUse, CommitmentID: ucFour, Quantity: 4}) {
		t.Fatalf("rebound usage = %+v", rebound)
	}
	// ucFour 的原定数量、归属与到期时刻仍是原值，只是已用数量随它自己的成功
	// 领取变为四件（这是它自己的使用，不是取消 ucSix 释放了它的占用）。
	four, _ := s.Commitment(ucFour)
	if four.Canceled || four.Expired || four.Quantity != 4 || four.Used != 4 ||
		four.RequestID != ucReq2 || !four.Expiry.Equal(ucExpFour) {
		t.Fatalf("ucFour after its own use = %+v, want active 4/4 owned by %s", four, ucReq2)
	}
	st3, _ := s.PartStatus(ucPart, ucCheckNow)
	// 实物再扣四件剩四件，两笔承诺均无未用占用，可承诺四件。
	if st3.PhysicalRemaining != 4 || st3.ActiveOccupied != 0 || st3.Committable != 4 {
		t.Fatalf("account after reuse on second commitment: %+v, want 4/0/4", st3)
	}
	// ucSix 的明细仍只有两件：被拒绝的领取从未写入，改绑后的记录挂在 ucFour 下。
	assertUCUsages(t, s, []Usage{
		{ID: ucFirstUse, CommitmentID: ucSix, Quantity: 2},
	})
	usages4, err := s.CommitmentUsages(ucFour)
	if err != nil || len(usages4) != 1 ||
		usages4[0] != (Usage{ID: ucLastUse, CommitmentID: ucFour, Quantity: 4}) {
		t.Fatalf("ucFour usages = %+v err %v, want single rebound usage", usages4, err)
	}

	// 按请求查看与上述实际结果一致。
	view1, err := s.RequestView(ucReq1, ucCheckNow)
	if err != nil {
		t.Fatalf("request view %s: %v", ucReq1, err)
	}
	d1 := view1.Commitments[0]
	if d1.CommitmentID != ucSix || d1.Status != CommitmentCanceled ||
		d1.OriginalQuantity != 6 || d1.UsedQuantity != 2 || d1.RemainingQuantity != 4 {
		t.Fatalf("ucReq1 view = %+v, want canceled 6/2/4", d1)
	}

	// 对照：全新仓库复现“取消先完成”，但不触碰 ucFour——它必须仍是 4/0/4
	// active，证明取消 ucSix 绝不会借释放之名改动第二笔承诺。
	s2 := ucRaceStore(t)
	if _, err := s2.Cancel(ucSix, ucRaceNow); err != nil {
		t.Fatalf("cancel on fresh store: %v", err)
	}
	untouched := mustCommitment(t, s2, ucFour)
	if untouched.Canceled || untouched.Expired || untouched.Quantity != 4 ||
		untouched.Used != 0 || untouched.Unused() != 4 ||
		untouched.RequestID != ucReq2 || !untouched.Expiry.Equal(ucExpFour) {
		t.Fatalf("ucFour on untouched store = %+v, want active 4/0/4 original", untouched)
	}
	fresh, _ := s2.PartStatus(ucPart, ucCheckNow)
	if fresh.PhysicalRemaining != 8 || fresh.ActiveOccupied != 4 || fresh.Committable != 4 {
		t.Fatalf("untouched store account = %+v, want 8/4/4", fresh)
	}
}
