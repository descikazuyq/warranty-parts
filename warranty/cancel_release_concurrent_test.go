package warranty

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障“取消承诺释放未用占用”这一既有功能，在另一个保修请求正为
// 同一种备件提交新预留时的并发正确性。业务主例：
//
//	备件 part1 初始库存十件；
//	cOld6：请求 rA 的承诺预留六件，已成功使用两件（已领取实物两件）；
//	cOther4：请求 rB 的承诺预留四件，尚未使用。
//	此时实物剩余八件、有效占用八件（cOld6 未用四件 + cOther4 四件）、
//	可承诺数量为零。
//
// 第三个具备保修资格的请求 rC 使用从未成功过的新承诺编号申请预留四件，这次
// 提交与取消 cOld6 同时发生，不预设谁先被处理。两种先后关系都合法：
//
//   - 新预留在取消生效前被处理：可承诺为零，返回 ErrInsufficientStock，不创建
//     新承诺、不留下任何占用；取消完成后实物八件、有效占用四件、可承诺四件。
//   - 取消先生效：cOld6 未用的四件被释放（已用两件不回补实物库存），新预留
//     成功，最终实物仍八件、有效占用八件、可承诺为零。
//
// 无论哪种结果，cOld6 最终都必须显示已取消并保留原定六件、已用两件、未用四件
// 及原到期时刻；cOther4 的状态、数量与归属不变；不能出现预留成功却没有对应
// 占用的结果。另覆盖取消释放后、四件尚未被新承诺占用时申请五件的边界。
// 这些测试只沿用登记、资格、预留、使用、取消与查询等既有公开入口、错误类别
// 及资格、到期和编号重试规则。

const (
	crPart     = "part1"
	crReqOld   = "rA"
	crReqOther = "rB"
	crReqNew   = "rC"
	crOld      = "cOld6"
	crOther    = "cOther4"
	crNew      = "cNew4"
	crFive     = "cNew5"
	crFour     = "cNew4b"
	crUsage    = "uOldUsed2"
)

var (
	crExpiryOld   = t0.Add(40 * day)
	crExpiryOther = t0.Add(30 * day)
	crExpiryNew   = t0.Add(45 * day)
	// crRaceNow 是取消与新预留并发提交的当前时刻，早于全部承诺到期时刻，
	// 也在保修期内；查询沿用稍后但仍早于最早到期时刻的 crCheckNow。
	crRaceNow  = nowOK.Add(time.Hour)
	crCheckNow = crRaceNow.Add(time.Hour)
)

// cancelReleaseStore 构造上述业务主例并断言取消前的初始账目：实物八件、
// 有效占用八件、可承诺为零。
func cancelReleaseStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(crPart, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, id := range []string{crReqOld, crReqOther, crReqNew} {
		if err := s.SubmitRequest(id, "p1", "FAULTY"); err != nil {
			t.Fatalf("submit request %s: %v", id, err)
		}
	}
	if _, err := s.Reserve(crOld, crReqOld, crPart, 6, crExpiryOld, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", crOld, err)
	}
	if _, err := s.Reserve(crOther, crReqOther, crPart, 4, crExpiryOther, nowOK); err != nil {
		t.Fatalf("reserve %s: %v", crOther, err)
	}
	if _, err := s.Use(crUsage, crOld, 2, nowOK); err != nil {
		t.Fatalf("use 2 from %s: %v", crOld, err)
	}
	st, err := s.PartStatus(crPart, crCheckNow)
	if err != nil {
		t.Fatalf("part status baseline: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 8 || st.Committable != 0 {
		t.Fatalf("baseline account: phys=%d occupied=%d committable=%d, want 8/8/0",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	return s
}

// crRaceResult 收集一次并发“取消 + 新预留”的两个返回值。
type crRaceResult struct {
	canceled   Commitment
	cancelErr  error
	reserved   Commitment
	reserveErr error
}

// fanOutCancelReserve 让取消 cOld6 与新预留（新编号、四件）在同一道闸机释放后
// 同时进入仓库，尽量制造交叉；两者的实际先后不由测试预设。
func fanOutCancelReserve(t *testing.T, s *Store, newID string, now time.Time) crRaceResult {
	t.Helper()
	var rr crRaceResult
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		rr.canceled, rr.cancelErr = s.Cancel(crOld, now)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		rr.reserved, rr.reserveErr = s.Reserve(newID, crReqNew, crPart, 4, crExpiryNew, now)
	}()
	close(start)
	wg.Wait()
	return rr
}

// assertCancelReleaseFixedCommitments 核对与本次并发结果无关的既有事实：
// cOld6 已取消但保留原定六件、已用两件、未用四件、归属与原到期时刻，其成功
// 使用记录仍在（已领取实物不回库存）；cOther4 完全不受影响。
func assertCancelReleaseFixedCommitments(t *testing.T, s *Store) {
	t.Helper()
	old, err := s.Commitment(crOld)
	if err != nil {
		t.Fatalf("commitment %s: %v", crOld, err)
	}
	if !old.Canceled || old.Expired || old.Quantity != 6 || old.Used != 2 ||
		old.Unused() != 4 || old.RequestID != crReqOld || old.PartID != crPart ||
		!old.Expiry.Equal(crExpiryOld) {
		t.Fatalf("old commitment after cancel: %+v, want canceled qty=6 used=2 unused=4", old)
	}
	// 已成功领取的两件实物保留为使用记录，取消不抹除它们。
	usages, err := s.CommitmentUsages(crOld)
	if err != nil {
		t.Fatalf("usages of %s: %v", crOld, err)
	}
	if len(usages) != 1 || usages[0] != (Usage{ID: crUsage, CommitmentID: crOld, Quantity: 2}) {
		t.Fatalf("usages of old commitment = %+v, want single 2-piece usage", usages)
	}
	other, err := s.Commitment(crOther)
	if err != nil {
		t.Fatalf("commitment %s: %v", crOther, err)
	}
	if other.Canceled || other.Expired || other.Quantity != 4 || other.Used != 0 ||
		other.Unused() != 4 || other.RequestID != crReqOther || other.PartID != crPart ||
		!other.Expiry.Equal(crExpiryOther) {
		t.Fatalf("other commitment disturbed: %+v, want active qty=4 used=0", other)
	}
}

// assertCancelReleaseNewRequestHistory 核对 rC 的预留处理历史与实际结果一致：
// 只有一条记录，成功或库存不足失败（含处理前库存依据快照）二选一，资格依据
// 均为合格。
func assertCancelReleaseNewRequestHistory(t *testing.T, s *Store, succeeded bool, wantBasis StockBasis) {
	t.Helper()
	hist, err := s.RequestHistory(crReqNew)
	if err != nil {
		t.Fatalf("request history %s: %v", crReqNew, err)
	}
	if len(hist) != 1 {
		t.Fatalf("rC history len = %d, want exactly 1 record", len(hist))
	}
	rec := hist[0]
	if rec.Seq != 1 || rec.CommitID != crNew || rec.PartID != crPart ||
		rec.Quantity != 4 || !rec.Expiry.Equal(crExpiryNew) || !rec.Now.Equal(crRaceNow) {
		t.Fatalf("rC record submission fields = %+v", rec)
	}
	if rec.Eligibility == nil || !rec.Eligibility.Eligible || rec.Eligibility.Excluded ||
		rec.Eligibility.RequestID != crReqNew || len(rec.Eligibility.Reasons) != 0 {
		t.Fatalf("rC record eligibility = %+v, want eligible snapshot", rec.Eligibility)
	}
	if rec.StockBasis == nil ||
		rec.StockBasis.PhysicalRemaining != wantBasis.PhysicalRemaining ||
		rec.StockBasis.ActiveOccupied != wantBasis.ActiveOccupied ||
		rec.StockBasis.Committable != wantBasis.Committable {
		t.Fatalf("rC record stock basis = %+v, want %+v", rec.StockBasis, wantBasis)
	}
	if succeeded {
		if !rec.Success || rec.Error != "" {
			t.Fatalf("rC record = %+v, want success", rec)
		}
	} else {
		if rec.Success || rec.Error != HistoryErrorInsufficientStock {
			t.Fatalf("rC record = %+v, want insufficient_stock failure", rec)
		}
	}
}

// assertCancelReleaseOutcome 在并发结束后核对全部不变量。succeeded 仅用于交叉
// 校验调用方对 rr.reserveErr 的分支判断；账目、明细、查询与历史的期望值全部
// 按仓库实际状态从该分支推导，不预设固定结果。
func assertCancelReleaseOutcome(t *testing.T, s *Store, rr crRaceResult, succeeded bool, now time.Time) {
	t.Helper()

	// 取消无论先后都成功，返回值即显示已取消并保留全部原定/已用/未用事实。
	if rr.cancelErr != nil {
		t.Fatalf("concurrent cancel: %v", rr.cancelErr)
	}
	if !rr.canceled.Canceled || rr.canceled.Quantity != 6 || rr.canceled.Used != 2 ||
		rr.canceled.Unused() != 4 || rr.canceled.RequestID != crReqOld ||
		!rr.canceled.Expiry.Equal(crExpiryOld) {
		t.Fatalf("cancel result = %+v, want canceled 6/used2/unused4", rr.canceled)
	}

	st, err := s.PartStatus(crPart, now)
	if err != nil {
		t.Fatalf("part status after race: %v", err)
	}
	// 实物剩余始终为八：预留不扣实物，取消不回补已领取的两件。
	if st.PhysicalRemaining != 8 {
		t.Fatalf("physical remaining = %d, want 8", st.PhysicalRemaining)
	}

	wantNew := Commitment{
		ID:        crNew,
		RequestID: crReqNew,
		PartID:    crPart,
		Quantity:  4,
		Expiry:    crExpiryNew,
	}

	switch {
	case succeeded:
		// 取消先生效：新预留成功并返回完整的首次承诺（已用为零、未取消）。
		if rr.reserveErr != nil {
			t.Fatalf("reserve after cancel released stock: %v", rr.reserveErr)
		}
		if rr.reserved != wantNew || rr.reserved.Used != 0 ||
			rr.reserved.Canceled || rr.reserved.Expired {
			t.Fatalf("new reserve result = %+v, want %+v", rr.reserved, wantNew)
		}
		// 实物八件、有效占用八件（cOther4 + cNew4）、可承诺为零。
		if st.ActiveOccupied != 8 || st.Committable != 0 {
			t.Fatalf("account on success branch: occupied=%d committable=%d, want 8/0",
				st.ActiveOccupied, st.Committable)
		}
		if len(st.Details) != 3 {
			t.Fatalf("part details len = %d, want 3 (old canceled + other + new)", len(st.Details))
		}
		dNew := mustDetailByID(t, st.Details, crNew)
		if dNew.Status != CommitmentActive || dNew.RequestID != crReqNew ||
			dNew.PartID != crPart || dNew.OriginalQuantity != 4 ||
			dNew.UsedQuantity != 0 || dNew.RemainingQuantity != 4 ||
			!dNew.Expiry.Equal(crExpiryNew) {
			t.Fatalf("new detail = %+v, want active 4/0/4 for rC", dNew)
		}
		got, err := s.Commitment(crNew)
		if err != nil || got != wantNew {
			t.Fatalf("stored new commitment = %+v, err %v; want %+v", got, err, wantNew)
		}
		view, err := s.RequestView(crReqNew, now)
		if err != nil {
			t.Fatalf("request view rC: %v", err)
		}
		if view.Eligibility == nil || !view.Eligibility.Eligible || len(view.Commitments) != 1 ||
			view.Commitments[0].CommitmentID != crNew || view.Commitments[0].Status != CommitmentActive {
			t.Fatalf("rC view = elig %+v commitments %+v, want single active %s",
				view.Eligibility, view.Commitments, crNew)
		}
		// 成功时处理前账目是取消后的 8/4/4。
		assertCancelReleaseNewRequestHistory(t, s, true, StockBasis{8, 4, 4})

	case errors.Is(rr.reserveErr, ErrInsufficientStock):
		// 新预留在取消生效前被处理：ErrInsufficientStock，返回空承诺，不创建记录。
		if rr.reserved != (Commitment{}) {
			t.Fatalf("failed reserve returned commitment = %+v, want empty", rr.reserved)
		}
		// 取消随后完成：实物八件、有效占用四件（只剩 cOther4）、可承诺四件。
		if st.ActiveOccupied != 4 || st.Committable != 4 {
			t.Fatalf("account on failure branch: occupied=%d committable=%d, want 4/4",
				st.ActiveOccupied, st.Committable)
		}
		if len(st.Details) != 2 {
			t.Fatalf("part details len = %d, want 2 (no failed new commitment)", len(st.Details))
		}
		if _, err := s.Commitment(crNew); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failed commitment lookup: got %v / err %v, want ErrNotFound", rr.reserved, err)
		}
		view, err := s.RequestView(crReqNew, now)
		if err != nil {
			t.Fatalf("request view rC: %v", err)
		}
		if view.Eligibility == nil || !view.Eligibility.Eligible || len(view.Commitments) != 0 {
			t.Fatalf("rC view = elig %+v commitments %+v, want eligible with no commitments",
				view.Eligibility, view.Commitments)
		}
		// 失败时处理前账目是取消前的 8/8/0。
		assertCancelReleaseNewRequestHistory(t, s, false, StockBasis{8, 8, 0})

	default:
		t.Fatalf("concurrent reserve returned unexpected error: %v (commitment %+v)",
			rr.reserveErr, rr.reserved)
	}

	// 备件明细中已取消的 cOld6 必须保留可追查，cOther4 保持有效；活动明细的
	// 未用数量之和必须恰好等于有效占用，杜绝“预留成功却没有对应占用”。
	dOld := mustDetailByID(t, st.Details, crOld)
	if dOld.Status != CommitmentCanceled || dOld.RequestID != crReqOld ||
		dOld.OriginalQuantity != 6 || dOld.UsedQuantity != 2 || dOld.RemainingQuantity != 4 ||
		!dOld.Expiry.Equal(crExpiryOld) {
		t.Fatalf("old detail = %+v, want canceled 6/2/4", dOld)
	}
	dOther := mustDetailByID(t, st.Details, crOther)
	if dOther.Status != CommitmentActive || dOther.RequestID != crReqOther ||
		dOther.OriginalQuantity != 4 || dOther.UsedQuantity != 0 || dOther.RemainingQuantity != 4 ||
		!dOther.Expiry.Equal(crExpiryOther) {
		t.Fatalf("other detail = %+v, want active 4/0/4", dOther)
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
	if st.PhysicalRemaining-st.ActiveOccupied != st.Committable {
		t.Fatalf("committable %d != physical %d - occupied %d",
			st.Committable, st.PhysicalRemaining, st.ActiveOccupied)
	}

	// 按请求查看：rA 仍只看到已取消的旧记录，rB 仍只看到自己未受影响的承诺。
	viewA, err := s.RequestView(crReqOld, now)
	if err != nil {
		t.Fatalf("request view rA: %v", err)
	}
	if len(viewA.Commitments) != 1 {
		t.Fatalf("rA commitments = %d, want 1 (canceled record retained)", len(viewA.Commitments))
	}
	if d := viewA.Commitments[0]; d.CommitmentID != crOld || d.Status != CommitmentCanceled ||
		d.OriginalQuantity != 6 || d.UsedQuantity != 2 || d.RemainingQuantity != 4 ||
		!d.Expiry.Equal(crExpiryOld) {
		t.Fatalf("rA view of old commitment = %+v", d)
	}
	viewB, err := s.RequestView(crReqOther, now)
	if err != nil {
		t.Fatalf("request view rB: %v", err)
	}
	if len(viewB.Commitments) != 1 {
		t.Fatalf("rB commitments = %d, want 1", len(viewB.Commitments))
	}
	if d := viewB.Commitments[0]; d.CommitmentID != crOther || d.Status != CommitmentActive ||
		d.OriginalQuantity != 4 || d.UsedQuantity != 0 || d.RemainingQuantity != 4 ||
		!d.Expiry.Equal(crExpiryOther) {
		t.Fatalf("rB view changed after cancel/reserve race: %+v", d)
	}

	// 取消与新预留都不向两个既有请求追加历史：各自只有建仓时的一条成功记录。
	for _, rid := range []string{crReqOld, crReqOther} {
		hist, err := s.RequestHistory(rid)
		if err != nil {
			t.Fatalf("history %s: %v", rid, err)
		}
		if len(hist) != 1 || !hist[0].Success {
			t.Fatalf("history of %s = %+v, want single original success record", rid, hist)
		}
	}

	assertCancelReleaseFixedCommitments(t, s)
}

// TestConcurrentCancelAndNewReserveEitherOrderValid 主并发回归：取消旧承诺与
// rC 的四件新预留同时发生，不预设处理先后。每次迭代允许出现任一合法结果
// （成功并占满释放量，或 ErrInsufficientStock 且取消后释放四件），但两种先后
// 关系下的全部账目、明细、查询与历史不变量都必须成立，不能出现成功却无占用
// 或失败却留下承诺等中间态。
func TestConcurrentCancelAndNewReserveEitherOrderValid(t *testing.T) {
	const iterations = 50
	var sawSuccess, sawFailure int
	for i := 0; i < iterations; i++ {
		t.Run("iteration", func(t *testing.T) {
			s := cancelReleaseStore(t)
			rr := fanOutCancelReserve(t, s, crNew, crRaceNow)
			switch {
			case rr.reserveErr == nil:
				sawSuccess++
			case errors.Is(rr.reserveErr, ErrInsufficientStock):
				sawFailure++
			default:
				t.Fatalf("iteration: unexpected reserve error %v", rr.reserveErr)
			}
			assertCancelReleaseOutcome(t, s, rr, rr.reserveErr == nil, crCheckNow)
		})
	}
	// 仅作信息记录：两种先后关系在足量迭代下通常都会真实发生；即使某次运行
	// 调度只给出一侧，assertCancelReleaseOutcome 也已覆盖该侧的全部不变量。
	t.Logf("across %d iterations: reserve-after-cancel successes=%d, reserve-before-cancel failures=%d",
		iterations, sawSuccess, sawFailure)
}

// TestReserveBeforeCancelFailsThenCancelReleasesFour 确定地覆盖“新预留先被
// 处理”的先后关系：可承诺为零时四件预留返回 ErrInsufficientStock 且不创建
// 承诺；随后取消完成，只释放旧承诺未用的四件，实物仍八件。失败不占用编号，
// 取消生效后用同一编号以相同内容再次提交应当成功（这是失败后的新提交，不是
// 幂等重试），最终账目与“取消先成功”一侧一致。
func TestReserveBeforeCancelFailsThenCancelReleasesFour(t *testing.T) {
	s := cancelReleaseStore(t)

	// 取消尚未生效：可承诺为零，整次拒绝。
	c, err := s.Reserve(crNew, crReqNew, crPart, 4, crExpiryNew, crRaceNow)
	if !errors.Is(err, ErrInsufficientStock) || c != (Commitment{}) {
		t.Fatalf("reserve before cancel: c=%+v err=%v, want ErrInsufficientStock", c, err)
	}
	if _, err := s.Commitment(crNew); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve created commitment: err=%v", err)
	}
	// 失败不改变账目：实物八件、占用八件、可承诺零。
	if st, _ := s.PartStatus(crPart, crCheckNow); st.PhysicalRemaining != 8 ||
		st.ActiveOccupied != 8 || st.Committable != 0 {
		t.Fatalf("state after failed reserve: %+v, want 8/8/0", st)
	}

	// 随后取消：释放未用四件，已用两件不回补实物。
	canceled, err := s.Cancel(crOld, crRaceNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("cancel after failed reserve: %v", err)
	}
	if !canceled.Canceled || canceled.Quantity != 6 || canceled.Used != 2 || canceled.Unused() != 4 {
		t.Fatalf("cancel result = %+v", canceled)
	}
	st, _ := s.PartStatus(crPart, crCheckNow)
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("state after cancel: phys=%d occupied=%d committable=%d, want 8/4/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 失败记录不占用新编号：取消生效后用相同内容再次提交应当首次成功。
	later := crCheckNow.Add(time.Hour)
	got, err := s.Reserve(crNew, crReqNew, crPart, 4, crExpiryNew, later)
	if err != nil {
		t.Fatalf("resubmit same id after cancel: %v", err)
	}
	wantNew := Commitment{ID: crNew, RequestID: crReqNew, PartID: crPart, Quantity: 4, Expiry: crExpiryNew}
	if got != wantNew {
		t.Fatalf("resubmitted commitment = %+v, want %+v", got, wantNew)
	}
	st2, _ := s.PartStatus(crPart, later)
	if st2.PhysicalRemaining != 8 || st2.ActiveOccupied != 8 || st2.Committable != 0 {
		t.Fatalf("state after resubmit: %+v, want phys=8 occupied=8 committable=0", st2)
	}

	// rC 历史两条：先是取消前库存不足失败（依据 8/8/0），再是取消后成功
	// （依据 8/4/4），序号连续，失败记录不是承诺、成功记录必有对应占用。
	hist, err := s.RequestHistory(crReqNew)
	if err != nil {
		t.Fatalf("rC history: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("rC history len = %d, want 2", len(hist))
	}
	if hist[0].Seq != 1 || hist[0].Success || hist[0].Error != HistoryErrorInsufficientStock ||
		hist[0].StockBasis == nil || hist[0].StockBasis.Committable != 0 {
		t.Fatalf("rC failure record = %+v", hist[0])
	}
	if hist[1].Seq != 2 || !hist[1].Success || hist[1].Error != "" ||
		hist[1].StockBasis == nil || hist[1].StockBasis.ActiveOccupied != 4 ||
		hist[1].StockBasis.Committable != 4 {
		t.Fatalf("rC success record = %+v", hist[1])
	}

	assertCancelReleaseFixedCommitments(t, s)
}

// TestCancelBeforeReserveSucceedsAndRetryRulesHold 确定地覆盖“取消先生效”的
// 先后关系：旧承诺未用四件释放后，rC 的四件预留成功，实物仍八件、可承诺归零。
// 新承诺随后沿用既有编号重试规则：相同内容原样取回首次承诺，不重复占用、不
// 追加历史；改数量即 ErrConflict，库存与归属不变。
func TestCancelBeforeReserveSucceedsAndRetryRulesHold(t *testing.T) {
	s := cancelReleaseStore(t)

	if _, err := s.Cancel(crOld, crRaceNow); err != nil {
		t.Fatalf("cancel before reserve: %v", err)
	}
	st, _ := s.PartStatus(crPart, crCheckNow)
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("state after cancel only: %+v, want 8/4/4", st)
	}

	got, err := s.Reserve(crNew, crReqNew, crPart, 4, crExpiryNew, crRaceNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("reserve freed four: %v", err)
	}
	wantNew := Commitment{ID: crNew, RequestID: crReqNew, PartID: crPart, Quantity: 4, Expiry: crExpiryNew}
	if got != wantNew {
		t.Fatalf("new commitment = %+v, want %+v", got, wantNew)
	}

	// 原样重试（当前时刻不同也不影响）：取回首次承诺，不重复占用、不追加历史。
	retryNow := crCheckNow.Add(time.Hour)
	again, err := s.Reserve(crNew, crReqNew, crPart, 4, crExpiryNew, retryNow)
	if err != nil || again != wantNew {
		t.Fatalf("idempotent retry = %+v err %v, want %+v", again, err, wantNew)
	}
	st2, _ := s.PartStatus(crPart, retryNow)
	if st2.PhysicalRemaining != 8 || st2.ActiveOccupied != 8 || st2.Committable != 0 {
		t.Fatalf("idempotent retry changed stock: %+v", st2)
	}
	hist, _ := s.RequestHistory(crReqNew)
	if len(hist) != 1 || !hist[0].Success {
		t.Fatalf("rC history after retry = %+v, want single success record", hist)
	}

	// 改数量复用编号：ErrConflict，不改变库存与归属，冲突失败记录挂在 rC 下。
	if conflict, err := s.Reserve(crNew, crReqNew, crPart, 5, crExpiryNew, retryNow); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry with changed quantity: c=%+v err=%v, want ErrConflict", conflict, err)
	}
	st3, _ := s.PartStatus(crPart, retryNow)
	if st3.PhysicalRemaining != 8 || st3.ActiveOccupied != 8 || st3.Committable != 0 {
		t.Fatalf("conflict retry changed stock: %+v", st3)
	}
	stored, err := s.Commitment(crNew)
	if err != nil || stored != wantNew {
		t.Fatalf("new commitment overwritten by conflict attempt: %+v err %v", stored, err)
	}
	hist2, _ := s.RequestHistory(crReqNew)
	if len(hist2) != 2 || hist2[1].Success || hist2[1].Error != HistoryErrorConflict ||
		hist2[1].Eligibility != nil || hist2[1].StockBasis != nil {
		t.Fatalf("rC history after conflict = %+v, want appended conflict record with nil basis", hist2)
	}

	assertCancelReleaseFixedCommitments(t, s)
}

// TestAfterCancelFreedFourReserveFiveRejectedWhole 边界回归：取消释放后四件
// 尚未被新承诺占用（实物八件、有效占用四件、可承诺四件），第三张请求申请五件
// 必须整次拒绝：返回 ErrInsufficientStock，不创建承诺，实物库存与其他承诺的
// 占用均不改变。失败不消耗可承诺数量，随后申请恰好四件应成功。
func TestAfterCancelFreedFourReserveFiveRejectedWhole(t *testing.T) {
	s := cancelReleaseStore(t)

	if _, err := s.Cancel(crOld, crRaceNow); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, _ := s.PartStatus(crPart, crCheckNow)
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("state after cancel: %+v, want 8/4/4", st)
	}

	// 可承诺只有四件：申请五件整次拒绝。
	c, err := s.Reserve(crFive, crReqNew, crPart, 5, crExpiryNew, crCheckNow)
	if !errors.Is(err, ErrInsufficientStock) || c != (Commitment{}) {
		t.Fatalf("reserve five against four committable: c=%+v err=%v, want ErrInsufficientStock", c, err)
	}
	if _, err := s.Commitment(crFive); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected five-piece reserve left a commitment: err=%v", err)
	}

	// 实物库存、有效占用、可承诺数量与其他承诺均不改变。
	st2, err := s.PartStatus(crPart, crCheckNow)
	if err != nil {
		t.Fatalf("part status after rejection: %v", err)
	}
	if st2.PhysicalRemaining != 8 || st2.ActiveOccupied != 4 || st2.Committable != 4 {
		t.Fatalf("rejected reserve changed stock: %+v, want 8/4/4", st2)
	}
	if len(st2.Details) != 2 {
		t.Fatalf("details len = %d, want 2 (old canceled + other)", len(st2.Details))
	}
	assertCancelReleaseFixedCommitments(t, s)

	// 失败记录与实际预留一致：一条库存不足记录，处理前依据为 8/4/4。
	hist, err := s.RequestHistory(crReqNew)
	if err != nil {
		t.Fatalf("rC history: %v", err)
	}
	if len(hist) != 1 || hist[0].Success || hist[0].Error != HistoryErrorInsufficientStock ||
		hist[0].CommitID != crFive || hist[0].Quantity != 5 ||
		hist[0].StockBasis == nil || hist[0].StockBasis.PhysicalRemaining != 8 ||
		hist[0].StockBasis.ActiveOccupied != 4 || hist[0].StockBasis.Committable != 4 {
		t.Fatalf("rC rejection record = %+v", hist)
	}

	// 被整次拒绝没有消耗任何可承诺数量：另一个新编号申请恰好四件应当成功，
	// 成功后实物仍八件、有效占用八件、可承诺为零。
	later := crCheckNow.Add(time.Hour)
	four, err := s.Reserve(crFour, crReqNew, crPart, 4, crExpiryNew, later)
	if err != nil {
		t.Fatalf("reserve exact four after five rejected: %v", err)
	}
	if four.Quantity != 4 || four.RequestID != crReqNew || four.Used != 0 ||
		four.Canceled || four.Expired {
		t.Fatalf("exact-four commitment = %+v", four)
	}
	st3, _ := s.PartStatus(crPart, later)
	if st3.PhysicalRemaining != 8 || st3.ActiveOccupied != 8 || st3.Committable != 0 {
		t.Fatalf("stock after exact-four reserve: %+v, want 8/8/0", st3)
	}
}
