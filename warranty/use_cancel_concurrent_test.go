package warranty

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障同一笔备件承诺“最后一次领取”与“取消”同时提交时的并发正确性，
// 只沿用登记、资格、预留、使用、取消与查询等既有公开入口、错误类别及编号规则。
//
// 业务主例（产品始终在保、故障未被除外、全部承诺均未到期）：
//
//	备件 part1 初始库存十件；
//	c1（请求 r1）预留六件，c2（请求 r2）预留四件；
//	c1 已成功使用两件（uPrev），因此实物剩八件，两笔承诺合计占用八件
//	（c1 未用四件 + c2 四件），可承诺数量为零。
//
// 以一个尚未成功使用过的新编号 uFinal，从 c1 领取它剩下的四件，同时取消 c1，
// 两次操作使用同一个早于到期的当前时刻，不预设谁先被处理。两种先后关系都是
// 合法的完整结果：
//
//   - 领取先成功、取消随后：四件领取成功，取消也成功。c1 最终 canceled，
//     原定六件、已用六件、未用零件；取消返回的承诺保留六件已用事实。实物剩
//     四件，c2 仍占用四件，可承诺仍为零。成功使用明细保留之前的两件和本次
//     四件，合计六件。
//   - 取消先完成：本次四件领取返回 ErrCommitmentClosed（整次失败、不扣减、
//     不占用使用编号）。c1 为 canceled，原定六件、已用两件、未用四件；取消
//     返回的承诺保留两件已用事实。实物仍剩八件，c2 占用四件，可承诺变为
//     四件。成功使用明细只有之前的两件，被拒绝的领取不出现，合计仍两件；
//     取消记录上的四件未用数量只作为原承诺的事实展示，不再计入有效占用。
//
// 两种结果下 c2 的请求归属、原数量、已用数量与到期时刻都必须保持原值，不能
// 借取消 c1 释放它的占用；按请求查看承诺、按备件查看库存、按承诺查看成功
// 使用明细，都必须与本次领取的成功或失败一致。

const (
	ucPart  = "part1"
	ucReq1  = "r1"
	ucReq2  = "r2"
	ucC1    = "c1"
	ucC2    = "c2"
	ucPrev  = "uPrev"
	ucFinal = "uFinal"
)

var (
	ucExpiry1 = t0.Add(40 * day)
	ucExpiry2 = t0.Add(50 * day)
	// ucRaceNow 是领取与取消并发提交的当前时刻：保修期内且早于两笔承诺到期；
	// 查询沿用稍后但仍早于最早到期时刻的 ucCheckNow，不引入到期确认。
	ucRaceNow  = nowOK.Add(time.Hour)
	ucCheckNow = ucRaceNow.Add(time.Hour)
)

// useCancelStore 构造上述业务主例并断言并发前的初始账目：实物八件、有效占用
// 八件、可承诺为零。
func useCancelStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(ucPart, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, id := range []string{ucReq1, ucReq2} {
		if err := s.SubmitRequest(id, "p1", "FAULTY"); err != nil {
			t.Fatalf("submit request %s: %v", id, err)
		}
	}
	if _, err := s.Reserve(ucC1, ucReq1, ucPart, 6, ucExpiry1, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", ucC1, err)
	}
	if _, err := s.Reserve(ucC2, ucReq2, ucPart, 4, ucExpiry2, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", ucC2, err)
	}
	if _, err := s.Use(ucPrev, ucC1, 2, nowOK); err != nil {
		t.Fatalf("seed use 2 from %s: %v", ucC1, err)
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

// fanOutUseCancel 让 c1 的四件领取与取消在同一道闸机释放后同时进入仓库，
// 尽量制造交叉；两者的实际先后不由测试预设。
func fanOutUseCancel(t *testing.T, s *Store, now time.Time) ucRaceResult {
	t.Helper()
	var rr ucRaceResult
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		rr.used, rr.useErr = s.Use(ucFinal, ucC1, 4, now)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		rr.canceled, rr.cancelErr = s.Cancel(ucC1, now)
	}()
	close(start)
	wg.Wait()
	return rr
}

// assertUseCancelFixedFacts 核对与本次领取成败无关的固定事实：
// c1 已取消（未被到期确认）、归属与原到期时刻保留；c2 完全不受影响；
// 领取与取消都不向两张请求追加预留历史。
func assertUseCancelFixedFacts(t *testing.T, s *Store) {
	t.Helper()
	c1, err := s.Commitment(ucC1)
	if err != nil {
		t.Fatalf("commitment %s: %v", ucC1, err)
	}
	if !c1.Canceled || c1.Expired || c1.RequestID != ucReq1 || c1.PartID != ucPart ||
		c1.Quantity != 6 || !c1.Expiry.Equal(ucExpiry1) {
		t.Fatalf("c1 after race: %+v, want canceled qty=6, request r1, kept expiry", c1)
	}
	c2, err := s.Commitment(ucC2)
	if err != nil {
		t.Fatalf("commitment %s: %v", ucC2, err)
	}
	if c2.Canceled || c2.Expired || c2.RequestID != ucReq2 || c2.PartID != ucPart ||
		c2.Quantity != 4 || c2.Used != 0 || c2.Unused() != 4 ||
		!c2.Expiry.Equal(ucExpiry2) {
		t.Fatalf("c2 disturbed: %+v, want active 4/0/4 owned by r2 with kept expiry", c2)
	}
	for _, rid := range []string{ucReq1, ucReq2} {
		hist, err := s.RequestHistory(rid)
		if err != nil {
			t.Fatalf("history %s: %v", rid, err)
		}
		if len(hist) != 1 || !hist[0].Success {
			t.Fatalf("history of %s = %+v, want single original reserve success", rid, hist)
		}
	}
}

// assertUseCancelOutcome 在并发结束后按实际发生的先后关系核对全部不变量。
// useSucceeded 只用于交叉校验调用方对 rr.useErr 的分支判断；账目、明细、查询
// 与使用明细的期望值全部从该分支推导，不预设固定结果。
func assertUseCancelOutcome(t *testing.T, s *Store, rr ucRaceResult, useSucceeded bool) {
	t.Helper()

	// 取消无论先后都成功。
	if rr.cancelErr != nil {
		t.Fatalf("concurrent cancel: %v", rr.cancelErr)
	}
	if !rr.canceled.Canceled || rr.canceled.Quantity != 6 ||
		rr.canceled.RequestID != ucReq1 || !rr.canceled.Expiry.Equal(ucExpiry1) {
		t.Fatalf("cancel result = %+v, want canceled qty=6 owned by r1", rr.canceled)
	}

	st, err := s.PartStatus(ucPart, ucCheckNow)
	if err != nil {
		t.Fatalf("part status after race: %v", err)
	}

	switch {
	case useSucceeded:
		// 领取先成功：返回完整使用记录；取消随后完成，取消返回的承诺必须保留
		// 六件已用事实（原定六、已用六、未用零）。
		if rr.useErr != nil {
			t.Fatalf("use before cancel: %v", rr.useErr)
		}
		if rr.used != (Usage{ID: ucFinal, CommitmentID: ucC1, Quantity: 4}) {
			t.Fatalf("use result = %+v, want {uFinal c1 4}", rr.used)
		}
		if rr.canceled.Used != 6 || rr.canceled.Unused() != 0 {
			t.Fatalf("cancel result = %+v, want used=6 unused=0", rr.canceled)
		}
		// 实物再扣四件剩四件；c1 未用清零后取消不释放任何占用，有效占用只剩
		// c2 的四件，可承诺仍为零。
		if st.PhysicalRemaining != 4 || st.ActiveOccupied != 4 || st.Committable != 0 {
			t.Fatalf("account on use-first branch: phys=%d occupied=%d committable=%d, want 4/4/0",
				st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
		}

		// c1 的承诺记录与两类查询都显示 canceled、6/6/0。
		got, err := s.Commitment(ucC1)
		if err != nil || got.Used != 6 || got.Unused() != 0 || !got.Canceled {
			t.Fatalf("stored c1 = %+v err %v, want canceled 6/6/0", got, err)
		}
		d1 := mustDetailByID(t, st.Details, ucC1)
		if d1.Status != CommitmentCanceled || d1.RequestID != ucReq1 ||
			d1.OriginalQuantity != 6 || d1.UsedQuantity != 6 || d1.RemainingQuantity != 0 ||
			!d1.Expiry.Equal(ucExpiry1) {
			t.Fatalf("c1 part detail = %+v, want canceled 6/6/0", d1)
		}
		view1, err := s.RequestView(ucReq1, ucCheckNow)
		if err != nil {
			t.Fatalf("request view r1: %v", err)
		}
		if len(view1.Commitments) != 1 {
			t.Fatalf("r1 commitments = %d, want 1", len(view1.Commitments))
		}
		if d := view1.Commitments[0]; d.CommitmentID != ucC1 || d.Status != CommitmentCanceled ||
			d.OriginalQuantity != 6 || d.UsedQuantity != 6 || d.RemainingQuantity != 0 ||
			!d.Expiry.Equal(ucExpiry1) {
			t.Fatalf("r1 view = %+v, want canceled 6/6/0", d)
		}

		// 成功使用明细保留之前的两件与本次四件，按使用编号升序，合计六件。
		usages, err := s.CommitmentUsages(ucC1)
		if err != nil {
			t.Fatalf("usages of c1: %v", err)
		}
		if len(usages) != 2 ||
			usages[0] != (Usage{ID: ucFinal, CommitmentID: ucC1, Quantity: 4}) ||
			usages[1] != (Usage{ID: ucPrev, CommitmentID: ucC1, Quantity: 2}) {
			t.Fatalf("usages = %+v, want [uFinal=4 uPrev=2]", usages)
		}
		if total := usages[0].Quantity + usages[1].Quantity; total != 6 {
			t.Fatalf("usages total = %d, want 6", total)
		}

	case errors.Is(rr.useErr, ErrCommitmentClosed):
		// 取消先完成：四件领取整次被拒，返回空使用记录、不扣减、不占用编号。
		if rr.used != (Usage{}) {
			t.Fatalf("rejected use returned record = %+v, want empty", rr.used)
		}
		// 取消返回的承诺保留两件已用事实：原定六、已用两、未用四。
		if rr.canceled.Used != 2 || rr.canceled.Unused() != 4 {
			t.Fatalf("cancel result = %+v, want used=2 unused=4", rr.canceled)
		}
		// 实物仍剩八件；c1 的四件未用占用被取消释放，有效占用只剩 c2 四件，
		// 可承诺变为四件。
		if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
			t.Fatalf("account on cancel-first branch: phys=%d occupied=%d committable=%d, want 8/4/4",
				st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
		}

		// c1 记录与两类查询都显示 canceled、6/2/4：四件未用只作为原承诺的
		// 数量事实展示，不再计入有效占用。
		got, err := s.Commitment(ucC1)
		if err != nil || got.Used != 2 || got.Unused() != 4 || !got.Canceled {
			t.Fatalf("stored c1 = %+v err %v, want canceled 6/2/4", got, err)
		}
		d1 := mustDetailByID(t, st.Details, ucC1)
		if d1.Status != CommitmentCanceled || d1.RequestID != ucReq1 ||
			d1.OriginalQuantity != 6 || d1.UsedQuantity != 2 || d1.RemainingQuantity != 4 ||
			!d1.Expiry.Equal(ucExpiry1) {
			t.Fatalf("c1 part detail = %+v, want canceled 6/2/4", d1)
		}
		view1, err := s.RequestView(ucReq1, ucCheckNow)
		if err != nil {
			t.Fatalf("request view r1: %v", err)
		}
		if len(view1.Commitments) != 1 {
			t.Fatalf("r1 commitments = %d, want 1", len(view1.Commitments))
		}
		if d := view1.Commitments[0]; d.CommitmentID != ucC1 || d.Status != CommitmentCanceled ||
			d.OriginalQuantity != 6 || d.UsedQuantity != 2 || d.RemainingQuantity != 4 ||
			!d.Expiry.Equal(ucExpiry1) {
			t.Fatalf("r1 view = %+v, want canceled 6/2/4", d)
		}

		// 被拒绝的领取不出现在成功使用明细中：只有之前的两件，合计两件。
		usages, err := s.CommitmentUsages(ucC1)
		if err != nil {
			t.Fatalf("usages of c1: %v", err)
		}
		if len(usages) != 1 || usages[0] != (Usage{ID: ucPrev, CommitmentID: ucC1, Quantity: 2}) {
			t.Fatalf("usages = %+v, want only [uPrev=2]", usages)
		}

	default:
		t.Fatalf("concurrent use returned unexpected error: %v (usage %+v)", rr.useErr, rr.used)
	}

	// c2 在两种结果下都保持 active、4/0/4、归属 r2 与原到期时刻。
	d2 := mustDetailByID(t, st.Details, ucC2)
	if d2.Status != CommitmentActive || d2.RequestID != ucReq2 ||
		d2.OriginalQuantity != 4 || d2.UsedQuantity != 0 || d2.RemainingQuantity != 4 ||
		!d2.Expiry.Equal(ucExpiry2) {
		t.Fatalf("c2 part detail = %+v, want active 4/0/4 owned by r2", d2)
	}
	view2, err := s.RequestView(ucReq2, ucCheckNow)
	if err != nil {
		t.Fatalf("request view r2: %v", err)
	}
	if len(view2.Commitments) != 1 {
		t.Fatalf("r2 commitments = %d, want 1", len(view2.Commitments))
	}
	if d := view2.Commitments[0]; d.CommitmentID != ucC2 || d.Status != CommitmentActive ||
		d.OriginalQuantity != 4 || d.UsedQuantity != 0 || d.RemainingQuantity != 4 ||
		d.RequestID != ucReq2 || !d.Expiry.Equal(ucExpiry2) {
		t.Fatalf("r2 view changed after use/cancel race: %+v", d)
	}

	// 明细中有效承诺的未用数量之和必须恰好等于有效占用；c1 的取消记录在
	// “取消先完成”分支保留四件未用事实，但绝不参与这个和。
	activeUnused := 0
	for _, d := range st.Details {
		if d.Status == CommitmentActive {
			activeUnused += d.RemainingQuantity
		}
	}
	if activeUnused != st.ActiveOccupied {
		t.Fatalf("active details unused sum = %d, want ActiveOccupied %d", activeUnused, st.ActiveOccupied)
	}
	if st.PhysicalRemaining-st.ActiveOccupied != st.Committable {
		t.Fatalf("committable %d != physical %d - occupied %d",
			st.Committable, st.PhysicalRemaining, st.ActiveOccupied)
	}

	assertUseCancelFixedFacts(t, s)
}

// TestConcurrentUseAndCancelEitherOrderValid 主并发回归：c1 剩余四件的领取与
// 取消同时发生，不预设处理先后。每次迭代允许出现任一合法完整结果（领取先成功
// 的 4/4/0 账目与六件使用明细，或取消先完成的 ErrCommitmentClosed、8/4/4
// 账目与两件使用明细），两种先后关系下的全部记录、账目、查询与明细不变量都
// 必须成立，不能依赖某一种执行顺序总会获胜。
func TestConcurrentUseAndCancelEitherOrderValid(t *testing.T) {
	const iterations = 50
	var sawUseFirst, sawCancelFirst int
	for i := 0; i < iterations; i++ {
		t.Run("iteration", func(t *testing.T) {
			s := useCancelStore(t)
			rr := fanOutUseCancel(t, s, ucRaceNow)
			switch {
			case rr.useErr == nil:
				sawUseFirst++
			case errors.Is(rr.useErr, ErrCommitmentClosed):
				sawCancelFirst++
			default:
				t.Fatalf("iteration: unexpected use error %v", rr.useErr)
			}
			assertUseCancelOutcome(t, s, rr, rr.useErr == nil)
		})
	}
	// 仅作信息记录：两种先后关系在足量迭代下通常都会真实发生；即使某次运行
	// 调度只给出一侧，assertUseCancelOutcome 与两个确定性测试也已分别覆盖两侧。
	t.Logf("across %d iterations: use-before-cancel=%d, cancel-before-use=%d",
		iterations, sawUseFirst, sawCancelFirst)
}

// TestUseFourThenCancelFullyConsumesCommitment 确定地覆盖“领取先成功”的先后
// 关系：四件领取成功（c1 6/6/0、实物四件），随后取消也成功，但取消只释放当时
// 尚未领取的占用——未用已为零，故没有数量可释放，c2 仍占用四件，可承诺仍为
// 零。取消返回的承诺保留六件已用事实，成功使用明细保留两件和四件两条记录。
func TestUseFourThenCancelFullyConsumesCommitment(t *testing.T) {
	s := useCancelStore(t)

	u, err := s.Use(ucFinal, ucC1, 4, ucRaceNow)
	if err != nil {
		t.Fatalf("use remaining four: %v", err)
	}
	if u != (Usage{ID: ucFinal, CommitmentID: ucC1, Quantity: 4}) {
		t.Fatalf("use result = %+v, want {uFinal c1 4}", u)
	}
	// 领取后、取消前：实物四件，有效占用四件（c2），可承诺零。
	st, err := s.PartStatus(ucPart, ucCheckNow)
	if err != nil {
		t.Fatalf("part status after use: %v", err)
	}
	if st.PhysicalRemaining != 4 || st.ActiveOccupied != 4 || st.Committable != 0 {
		t.Fatalf("account after use: %+v, want 4/4/0", st)
	}
	d1 := mustDetailByID(t, st.Details, ucC1)
	if d1.Status != CommitmentActive || d1.UsedQuantity != 6 || d1.RemainingQuantity != 0 {
		t.Fatalf("c1 after full use = %+v, want active 6/6/0", d1)
	}

	// 随后取消：成功，但未用为零，没有任何占用可释放。
	canceled, err := s.Cancel(ucC1, ucRaceNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("cancel after full use: %v", err)
	}
	if !canceled.Canceled || canceled.Quantity != 6 || canceled.Used != 6 || canceled.Unused() != 0 {
		t.Fatalf("cancel result = %+v, want canceled 6/6/0", canceled)
	}
	st2, err := s.PartStatus(ucPart, ucCheckNow)
	if err != nil {
		t.Fatalf("part status after cancel: %v", err)
	}
	if st2.PhysicalRemaining != 4 || st2.ActiveOccupied != 4 || st2.Committable != 0 {
		t.Fatalf("account after cancel: phys=%d occupied=%d committable=%d, want 4/4/0",
			st2.PhysicalRemaining, st2.ActiveOccupied, st2.Committable)
	}

	// 已成功使用编号原样重试只取回本次记录，不再次扣减。
	again, err := s.Use(ucFinal, ucC1, 4, ucCheckNow)
	if err != nil || again != u {
		t.Fatalf("idempotent retry = %+v err %v, want first result %+v", again, err, u)
	}
	st3, _ := s.PartStatus(ucPart, ucCheckNow)
	if st3.PhysicalRemaining != 4 || st3.ActiveOccupied != 4 || st3.Committable != 0 {
		t.Fatalf("retry changed stock: %+v", st3)
	}

	// 新使用编号在取消后一律关闭。
	if _, err := s.Use("uAfter", ucC1, 1, ucCheckNow); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new use after cancel: got %v, want ErrCommitmentClosed", err)
	}

	// 使用明细保留之前的两件和本次四件，合计六件。
	usages, err := s.CommitmentUsages(ucC1)
	if err != nil {
		t.Fatalf("usages: %v", err)
	}
	if len(usages) != 2 ||
		usages[0] != (Usage{ID: ucFinal, CommitmentID: ucC1, Quantity: 4}) ||
		usages[1] != (Usage{ID: ucPrev, CommitmentID: ucC1, Quantity: 2}) {
		t.Fatalf("usages = %+v, want [uFinal=4 uPrev=2]", usages)
	}

	assertUseCancelFixedFacts(t, s)
}

// TestCancelThenUseFourRejectedLeavesFourReleasable 确定地覆盖“取消先完成”的
// 先后关系：取消只释放当时尚未领取的四件占用（已用两件不回补实物），随后以
// 尚未成功使用过的编号领取剩下四件返回 ErrCommitmentClosed，整次不扣减、不
// 写使用明细、不占用编号。实物仍八件、c2 占用四件、可承诺变为四件；取消记录
// 保留 6/2/4 的数量事实，c2 的归属、数量与到期时刻不变。
func TestCancelThenUseFourRejectedLeavesFourReleasable(t *testing.T) {
	s := useCancelStore(t)

	canceled, err := s.Cancel(ucC1, ucRaceNow)
	if err != nil {
		t.Fatalf("cancel before use: %v", err)
	}
	// 取消释放的是尚未领取的四件：返回值保留两件已用事实。
	if !canceled.Canceled || canceled.Quantity != 6 || canceled.Used != 2 || canceled.Unused() != 4 {
		t.Fatalf("cancel result = %+v, want canceled 6/2/4", canceled)
	}
	st, err := s.PartStatus(ucPart, ucCheckNow)
	if err != nil {
		t.Fatalf("part status after cancel: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("account after cancel: %+v, want 8/4/4", st)
	}

	// 新编号领取四件：承诺已取消，整次拒绝。
	u, err := s.Use(ucFinal, ucC1, 4, ucRaceNow.Add(time.Minute))
	if !errors.Is(err, ErrCommitmentClosed) || u != (Usage{}) {
		t.Fatalf("use after cancel: u=%+v err=%v, want ErrCommitmentClosed with empty result", u, err)
	}

	// 被拒绝后账目与已用数量保持取消后的原值。
	st2, err := s.PartStatus(ucPart, ucCheckNow)
	if err != nil {
		t.Fatalf("part status after rejected use: %v", err)
	}
	if st2.PhysicalRemaining != 8 || st2.ActiveOccupied != 4 || st2.Committable != 4 {
		t.Fatalf("account after rejected use: %+v, want 8/4/4", st2)
	}
	c1, _ := s.Commitment(ucC1)
	if !c1.Canceled || c1.Used != 2 || c1.Unused() != 4 {
		t.Fatalf("c1 after rejected use = %+v, want canceled 6/2/4", c1)
	}

	// 被拒绝的领取不出现在成功使用明细中：仍只有两件记录，合计两件。
	usages, err := s.CommitmentUsages(ucC1)
	if err != nil {
		t.Fatalf("usages: %v", err)
	}
	if len(usages) != 1 || usages[0] != (Usage{ID: ucPrev, CommitmentID: ucC1, Quantity: 2}) {
		t.Fatalf("usages = %+v, want only [uPrev=2]", usages)
	}

	// 取消记录上的四件未用不占有效占用：c2 是唯一的 active 明细。
	d2 := mustDetailByID(t, st2.Details, ucC2)
	if d2.Status != CommitmentActive || d2.RemainingQuantity != 4 {
		t.Fatalf("c2 detail = %+v, want active remaining=4", d2)
	}
	d1 := mustDetailByID(t, st2.Details, ucC1)
	if d1.Status != CommitmentCanceled || d1.RemainingQuantity != 4 || d1.UsedQuantity != 2 {
		t.Fatalf("c1 detail = %+v, want canceled 6/2/4 retained as fact only", d1)
	}

	// 释放出的四件可承诺数量真实可用：第三张合格请求恰好预留四件成功，
	// 之后实物八件、有效占用八件、可承诺归零，证明取消没有误放 c2 的占用。
	if err := s.SubmitRequest("r3", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r3: %v", err)
	}
	c3, err := s.Reserve("c3", "r3", ucPart, 4, t0.Add(45*day), ucCheckNow)
	if err != nil {
		t.Fatalf("reserve freed four for r3: %v", err)
	}
	if c3.Quantity != 4 || c3.Used != 0 || c3.RequestID != "r3" || c3.Canceled || c3.Expired {
		t.Fatalf("c3 = %+v, want fresh active 4 for r3", c3)
	}
	st3, _ := s.PartStatus(ucPart, ucCheckNow)
	if st3.PhysicalRemaining != 8 || st3.ActiveOccupied != 8 || st3.Committable != 0 {
		t.Fatalf("account after re-reserve: %+v, want 8/8/0", st3)
	}

	assertUseCancelFixedFacts(t, s)
}
