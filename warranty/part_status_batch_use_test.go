package warranty

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障“按备件查看库存与承诺明细”在分批使用交错发生时的数量自洽：
// 查询（PartStatus）与为有效承诺登记使用（Use）可能同时进行，查询既可能看到
// 某次使用处理前、也可能看到处理后的结果，但同一份返回结果里的实物剩余、有效
// 占用与各条明细必须属于同一个完整状态，不能混用不同阶段的数量。所有操作只
// 沿用登记、预留、分批使用与查询等既有公开入口与业务规则，全部当前时刻都早
// 于承诺到期时刻，承诺始终未取消。
//
// 主例：同一备件初始实物库存二十件，两笔在保请求分别预留九件和六件，使用前
// 实物剩余二十、有效占用十五、可承诺五。之后用各自不同的使用编号交错分批
// 使用这两笔承诺：九件那笔最终全部使用，六件那笔使用四件，结束时实物剩余
// 七件、有效占用两件、可承诺五件；已全部使用的承诺仍在明细中显示已用九件、
// 未用零件。使用期间取得并保留的查询结果保留取得时的数量，不随后续使用变化。
//
// 失败条件：使用期间另用一个尚未成功使用的编号向九件承诺提交十件使用量，
// 无论它与查询、正常使用谁先处理，都整次返回 ErrUsageExceeded，不扣减实物、
// 不增加任何承诺的已用数量。

const (
	batchPartID    = "part-batch"
	batchProductID = "p-batch"
	batchReqNine   = "req-batch-nine"
	batchReqSix    = "req-batch-six"
	batchCommit9   = "c-nine"
	batchCommit6   = "c-six"
)

// batchExpiryNine / batchExpirySix 给出晚于全部操作时刻的到期时刻；两笔承诺
// 给不同时刻，以回归查询明细始终保留各自的到期时刻，不被分批使用改写。
func batchExpiryNine() time.Time { return t0.Add(50 * day) }
func batchExpirySix() time.Time  { return t0.Add(40 * day) }

// batchUseStore 构造主例仓库：保修六十天的产品、初始库存二十件的备件，以及
// 两个分别对应九件承诺与六件承诺的合格请求。
func batchUseStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct(batchProductID, t0, 60, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(batchPartID, 20); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(batchReqNine, batchProductID, "FAULTY"); err != nil {
		t.Fatalf("submit %s: %v", batchReqNine, err)
	}
	if err := s.SubmitRequest(batchReqSix, batchProductID, "NOISE"); err != nil {
		t.Fatalf("submit %s: %v", batchReqSix, err)
	}
	return s
}

// batchUseReserveBoth 预留九件与六件两笔承诺（同属一个备件、分属两个请求）。
// 预留不扣减实物库存，完成后实物二十、占用十五、可承诺五。
func batchUseReserveBoth(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Reserve(batchCommit9, batchReqNine, batchPartID, 9, batchExpiryNine(), nowOK); err != nil {
		t.Fatalf("reserve %s: %v", batchCommit9, err)
	}
	if _, err := s.Reserve(batchCommit6, batchReqSix, batchPartID, 6, batchExpirySix(), nowOK); err != nil {
		t.Fatalf("reserve %s: %v", batchCommit6, err)
	}
}

// assertBatchSnapshotCoherent 校验一份 PartStatus 结果内部完全自洽，并返回明细
// 中两笔承诺各自的已用数量。自洽关系包括：
//   - 始终只有两条明细，保留各自的请求归属、原定数量、到期时刻，按承诺编号
//     排序，状态均为 active（时刻未到期、承诺未取消）；
//   - 每条明细原定数量等于已用数量加未用数量；
//   - 有效占用等于两条明细未用数量之和；
//   - 实物剩余等于二十件减去两条明细的已用总量；
//   - 可承诺数量始终为五件，且等于实物剩余减去有效占用。
func assertBatchSnapshotCoherent(t *testing.T, st *PartStatus) (usedNine, usedSix int) {
	t.Helper()
	if st.PartID != batchPartID {
		t.Fatalf("part id = %q, want %q", st.PartID, batchPartID)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details = %d, want 2 (neither lost nor duplicated by batch use)", len(st.Details))
	}
	// 明细始终按承诺编号排序：c-nine 在 c-six 之前。
	if st.Details[0].CommitmentID != batchCommit9 || st.Details[1].CommitmentID != batchCommit6 {
		t.Fatalf("details order = %s, %s, want %s, %s",
			st.Details[0].CommitmentID, st.Details[1].CommitmentID, batchCommit9, batchCommit6)
	}
	d9 := st.Details[0]
	d6 := st.Details[1]
	if d9.RequestID != batchReqNine || d9.PartID != batchPartID ||
		d9.OriginalQuantity != 9 || d9.Status != CommitmentActive || !d9.Expiry.Equal(batchExpiryNine()) {
		t.Fatalf("nine detail drifted from reservation: %+v", d9)
	}
	if d6.RequestID != batchReqSix || d6.PartID != batchPartID ||
		d6.OriginalQuantity != 6 || d6.Status != CommitmentActive || !d6.Expiry.Equal(batchExpirySix()) {
		t.Fatalf("six detail drifted from reservation: %+v", d6)
	}
	// 每条明细：原定 = 已用 + 未用。
	if d9.OriginalQuantity != d9.UsedQuantity+d9.RemainingQuantity {
		t.Fatalf("nine detail orig=%d used=%d remaining=%d, want orig=used+remaining",
			d9.OriginalQuantity, d9.UsedQuantity, d9.RemainingQuantity)
	}
	if d6.OriginalQuantity != d6.UsedQuantity+d6.RemainingQuantity {
		t.Fatalf("six detail orig=%d used=%d remaining=%d, want orig=used+remaining",
			d6.OriginalQuantity, d6.UsedQuantity, d6.RemainingQuantity)
	}
	usedNine, usedSix = d9.UsedQuantity, d6.UsedQuantity
	totalUsed := usedNine + usedSix
	// 实物剩余 = 初始二十件 − 明细已用总量。
	if wantPhysical := 20 - totalUsed; st.PhysicalRemaining != wantPhysical {
		t.Fatalf("physical = %d, want 20 - used total %d = %d (snapshot mixes stages)",
			st.PhysicalRemaining, totalUsed, wantPhysical)
	}
	// 有效占用 = 两条明细未用数量之和。
	if wantOccupied := d9.RemainingQuantity + d6.RemainingQuantity; st.ActiveOccupied != wantOccupied {
		t.Fatalf("occupied = %d, want sum of remaining %d (snapshot mixes stages)",
			st.ActiveOccupied, wantOccupied)
	}
	if wantOccupied := 15 - totalUsed; st.ActiveOccupied != wantOccupied {
		t.Fatalf("occupied = %d, want 15 - used total %d = %d", st.ActiveOccupied, totalUsed, wantOccupied)
	}
	// 可承诺始终为五件，且与实物剩余、有效占用同属一个状态。
	if st.Committable != 5 {
		t.Fatalf("committable = %d, want always 5", st.Committable)
	}
	if st.Committable != st.PhysicalRemaining-st.ActiveOccupied {
		t.Fatalf("committable = %d, want physical %d - occupied %d = %d",
			st.Committable, st.PhysicalRemaining, st.ActiveOccupied, st.PhysicalRemaining-st.ActiveOccupied)
	}
	return usedNine, usedSix
}

// assertBatchSnapshot 在内洽校验之外，断言这份结果取得时两笔承诺的已用数量。
func assertBatchSnapshot(t *testing.T, st *PartStatus, wantUsedNine, wantUsedSix int) {
	t.Helper()
	gotNine, gotSix := assertBatchSnapshotCoherent(t, st)
	if gotNine != wantUsedNine || gotSix != wantUsedSix {
		t.Fatalf("snapshot used nine=%d six=%d, want nine=%d six=%d",
			gotNine, gotSix, wantUsedNine, wantUsedSix)
	}
}

// TestPartStatusConsistentAcrossInterleavedBatchUse 顺序但交错地分批使用两笔
// 承诺，并在每个阶段查询并保留结果：每份结果的三项库存与两条明细必须互相
// 核对；全部使用结束后，先前保留的结果仍是取得当时的数量；最终九件承诺全部
// 使用（明细仍显示已用九、未用零），六件承诺使用四件，实物七、占用二、可
// 承诺五。按请求查看也仍只保留各自那笔承诺的归属与到期时刻。
func TestPartStatusConsistentAcrossInterleavedBatchUse(t *testing.T) {
	s := batchUseStore(t)
	batchUseReserveBoth(t, s)

	// 使用前：实物二十、占用十五（九加六）、可承诺五。
	snap0, err := s.PartStatus(batchPartID, nowOK)
	if err != nil {
		t.Fatalf("part status before uses: %v", err)
	}
	assertBatchSnapshot(t, snap0, 0, 0)
	if snap0.PhysicalRemaining != 20 || snap0.ActiveOccupied != 15 || snap0.Committable != 5 {
		t.Fatalf("initial account = %+v, want 20/15/5", snap0)
	}

	// 第一批：九件承诺使用四件。已用总量四 → 实物十六、占用十一（五加六）。
	if _, err := s.Use("u-n1", batchCommit9, 4, nowOK.Add(time.Hour)); err != nil {
		t.Fatalf("use 4 from nine: %v", err)
	}
	snap1, err := s.PartStatus(batchPartID, nowOK.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("part status after nine-4: %v", err)
	}
	assertBatchSnapshot(t, snap1, 4, 0)

	// 第二批：六件承诺使用两件。已用总量六 → 实物十四、占用九（五加四）。
	if _, err := s.Use("u-s1", batchCommit6, 2, nowOK.Add(3*time.Hour)); err != nil {
		t.Fatalf("use 2 from six: %v", err)
	}
	snap2, err := s.PartStatus(batchPartID, nowOK.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("part status after six-2: %v", err)
	}
	assertBatchSnapshot(t, snap2, 4, 2)

	// 第三批：九件承诺再使用五件，恰好全部使用。已用总量十一 → 实物九、
	// 占用四（零加四）；用尽的承诺仍在明细中显示已用九、未用零。
	if _, err := s.Use("u-n2", batchCommit9, 5, nowOK.Add(5*time.Hour)); err != nil {
		t.Fatalf("use remaining 5 from nine: %v", err)
	}
	snap3, err := s.PartStatus(batchPartID, nowOK.Add(6*time.Hour))
	if err != nil {
		t.Fatalf("part status after nine exhausted: %v", err)
	}
	assertBatchSnapshot(t, snap3, 9, 2)
	d9Exhausted := mustDetailByID(t, snap3.Details, batchCommit9)
	if d9Exhausted.UsedQuantity != 9 || d9Exhausted.RemainingQuantity != 0 || d9Exhausted.Status != CommitmentActive {
		t.Fatalf("exhausted nine detail = %+v, want used=9 remaining=0 active", d9Exhausted)
	}

	// 第四批：六件承诺再使用两件，累计使用四件。已用总量十三 → 实物七、
	// 占用二（零加二）、可承诺五。
	if _, err := s.Use("u-s2", batchCommit6, 2, nowOK.Add(7*time.Hour)); err != nil {
		t.Fatalf("use second 2 from six: %v", err)
	}
	final, err := s.PartStatus(batchPartID, nowOK.Add(8*time.Hour))
	if err != nil {
		t.Fatalf("final part status: %v", err)
	}
	assertBatchSnapshot(t, final, 9, 4)
	if final.PhysicalRemaining != 7 || final.ActiveOccupied != 2 || final.Committable != 5 {
		t.Fatalf("final account = %+v, want 7/2/5", final)
	}

	// 使用过程中取得并保留的结果保留取得时的数量，不随后续使用变化。
	assertBatchSnapshot(t, snap0, 0, 0)
	assertBatchSnapshot(t, snap1, 4, 0)
	assertBatchSnapshot(t, snap2, 4, 2)
	assertBatchSnapshot(t, snap3, 9, 2)
	// 每份保留结果的三项库存也必须仍是取得当时的值。
	if snap0.PhysicalRemaining != 20 || snap0.ActiveOccupied != 15 {
		t.Fatalf("snap0 changed: %+v", snap0)
	}
	if snap1.PhysicalRemaining != 16 || snap1.ActiveOccupied != 11 {
		t.Fatalf("snap1 changed: %+v", snap1)
	}
	if snap2.PhysicalRemaining != 14 || snap2.ActiveOccupied != 9 {
		t.Fatalf("snap2 changed: %+v", snap2)
	}
	if snap3.PhysicalRemaining != 9 || snap3.ActiveOccupied != 4 {
		t.Fatalf("snap3 changed: %+v", snap3)
	}

	// 明细不重复、不丢失：每条保留结果始终恰好两条，各自请求归属与到期时刻不变。
	for i, snap := range []*PartStatus{snap0, snap1, snap2, snap3, final} {
		if len(snap.Details) != 2 {
			t.Fatalf("snapshot %d details = %d, want 2", i, len(snap.Details))
		}
		d9 := mustDetailByID(t, snap.Details, batchCommit9)
		d6 := mustDetailByID(t, snap.Details, batchCommit6)
		if d9.RequestID != batchReqNine || !d9.Expiry.Equal(batchExpiryNine()) || d9.OriginalQuantity != 9 {
			t.Fatalf("snapshot %d nine identity changed: %+v", i, d9)
		}
		if d6.RequestID != batchReqSix || !d6.Expiry.Equal(batchExpirySix()) || d6.OriginalQuantity != 6 {
			t.Fatalf("snapshot %d six identity changed: %+v", i, d6)
		}
	}

	// 仓库事实与最终查询一致。
	c9, _ := s.Commitment(batchCommit9)
	c6, _ := s.Commitment(batchCommit6)
	if c9.Used != 9 || c9.Unused() != 0 || c9.Canceled || c9.Expired {
		t.Fatalf("stored nine commitment = %+v, want used=9 unused=0 active", c9)
	}
	if c6.Used != 4 || c6.Unused() != 2 || c6.Canceled || c6.Expired {
		t.Fatalf("stored six commitment = %+v, want used=4 unused=2 active", c6)
	}
	if p, _ := s.Part(batchPartID); p.Stock != 7 {
		t.Fatalf("stored physical stock = %d, want 7", p.Stock)
	}

	// 按请求查看：每个请求仍只关联各自那笔承诺，原定数量、到期时刻与分批
	// 已用数量与备件明细一致。
	v9, err := s.RequestView(batchReqNine, nowOK.Add(9*time.Hour))
	if err != nil {
		t.Fatalf("request view nine: %v", err)
	}
	if len(v9.Commitments) != 1 {
		t.Fatalf("nine request commitments = %d, want 1", len(v9.Commitments))
	}
	vd9 := v9.Commitments[0]
	if vd9.CommitmentID != batchCommit9 || vd9.OriginalQuantity != 9 || vd9.UsedQuantity != 9 ||
		vd9.RemainingQuantity != 0 || !vd9.Expiry.Equal(batchExpiryNine()) || vd9.Status != CommitmentActive {
		t.Fatalf("nine request detail = %+v", vd9)
	}
	v6, err := s.RequestView(batchReqSix, nowOK.Add(9*time.Hour))
	if err != nil {
		t.Fatalf("request view six: %v", err)
	}
	if len(v6.Commitments) != 1 {
		t.Fatalf("six request commitments = %d, want 1", len(v6.Commitments))
	}
	vd6 := v6.Commitments[0]
	if vd6.CommitmentID != batchCommit6 || vd6.OriginalQuantity != 6 || vd6.UsedQuantity != 4 ||
		vd6.RemainingQuantity != 2 || !vd6.Expiry.Equal(batchExpirySix()) || vd6.Status != CommitmentActive {
		t.Fatalf("six request detail = %+v", vd6)
	}
}

// fanOutBatchMixed 让正常分批使用、超量失败提交与库存查询全部在同一道闸机
// 释放后同时进入，尽量制造查询与使用处理前后交错；不规定任何固定完成顺序。
// 返回与入参一一对应的使用结果，以及全部查询取得的结果快照。
func fanOutBatchMixed(t *testing.T, s *Store, useCalls []concurrentUseCall, queryNows []time.Time) ([]concurrentUseResult, []*PartStatus) {
	useResults := make([]concurrentUseResult, len(useCalls))
	snapshots := make([]*PartStatus, len(queryNows))
	queryErrs := make([]error, len(queryNows))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, c := range useCalls {
		wg.Add(1)
		go func(i int, c concurrentUseCall) {
			defer wg.Done()
			<-start
			useResults[i].usage, useResults[i].err = s.Use(c.usageID, c.commitID, c.quantity, c.now)
		}(i, c)
	}
	for i, now := range queryNows {
		wg.Add(1)
		go func(i int, now time.Time) {
			defer wg.Done()
			<-start
			snapshots[i], queryErrs[i] = s.PartStatus(batchPartID, now)
		}(i, now)
	}
	close(start)
	wg.Wait()
	for i, err := range queryErrs {
		if err != nil {
			t.Fatalf("concurrent query %d: %v", i, err)
		}
	}
	return useResults, snapshots
}

// TestConcurrentPartStatusAndBatchUseSnapshotsCoherent 查询与分批使用同时
// 发生：四笔正常使用（九件承诺四加五、六件承诺二加二，各自独立编号）、十件
// 超量提交（重复多次，使用同一个从未成功的编号）和大量查询同时发出，完成
// 顺序任意。每一份查询结果都必须自洽，且只能落在某个合法的使用处理阶段上；
// 超量提交无论卡在哪个阶段都返回 ErrUsageExceeded，全部结束后数量恒为
// 七/二/五。多轮重复以覆盖各种先后顺序。
func TestConcurrentPartStatusAndBatchUseSnapshotsCoherent(t *testing.T) {
	const rounds = 25
	for round := 0; round < rounds; round++ {
		s := batchUseStore(t)
		batchUseReserveBoth(t, s)

		useCalls := []concurrentUseCall{
			{usageID: "u-n1", commitID: batchCommit9, quantity: 4, now: nowOK.Add(30 * time.Minute)},
			{usageID: "u-n2", commitID: batchCommit9, quantity: 5, now: nowOK.Add(31 * time.Minute)},
			{usageID: "u-s1", commitID: batchCommit6, quantity: 2, now: nowOK.Add(32 * time.Minute)},
			{usageID: "u-s2", commitID: batchCommit6, quantity: 2, now: nowOK.Add(33 * time.Minute)},
		}
		// 同一个尚未成功使用的编号，反复向九件承诺提交十件：任何阶段其未用
		// 数量至多为九，十件必然超量；失败不占用编号，每次都独立判超量。
		const badCopies = 8
		for i := 0; i < badCopies; i++ {
			useCalls = append(useCalls, concurrentUseCall{
				usageID:  "u-bad",
				commitID: batchCommit9,
				quantity: 10,
				now:      nowOK.Add(time.Hour + time.Duration(i)*time.Minute),
			})
		}
		queryNows := make([]time.Time, 24)
		for i := range queryNows {
			queryNows[i] = nowOK.Add(time.Duration(i) * time.Minute)
		}

		useResults, snapshots := fanOutBatchMixed(t, s, useCalls, queryNows)

		// 四笔正常使用全部成功，记录与入参一一对应。
		for i := 0; i < 4; i++ {
			r := useResults[i]
			if r.err != nil {
				t.Fatalf("round %d normal use %d (%s qty %d): %v",
					round, i, useCalls[i].commitID, useCalls[i].quantity, r.err)
			}
			if r.usage.ID != useCalls[i].usageID || r.usage.CommitmentID != useCalls[i].commitID ||
				r.usage.Quantity != useCalls[i].quantity {
				t.Fatalf("round %d normal use %d record = %+v", round, i, r.usage)
			}
		}
		// 超量提交整次拒绝，且始终是 ErrUsageExceeded：无论它先于查询、先于
		// 正常使用还是在承诺耗尽后处理，都不能是成功或编号冲突。
		for i := 4; i < len(useResults); i++ {
			if !errors.Is(useResults[i].err, ErrUsageExceeded) || useResults[i].usage != (Usage{}) {
				t.Fatalf("round %d over-use copy %d = %+v, err %v; want ErrUsageExceeded with empty result",
					round, i, useResults[i].usage, useResults[i].err)
			}
		}

		// 每份查询结果自洽，且只能对应某个由四笔正常使用线性化出来的完整阶段：
		// 九件承诺已用只能是 0/4/5/9，六件承诺已用只能是 0/2/4。
		for qi, st := range snapshots {
			usedNine, usedSix := assertBatchSnapshotCoherent(t, st)
			switch usedNine {
			case 0, 4, 5, 9:
			default:
				t.Fatalf("round %d snapshot %d nine used = %d, want one of 0/4/5/9", round, qi, usedNine)
			}
			switch usedSix {
			case 0, 2, 4:
			default:
				t.Fatalf("round %d snapshot %d six used = %d, want one of 0/2/4", round, qi, usedSix)
			}
		}

		// 全部处理结束：实物七、有效占用二、可承诺五；九件用尽、六件用四。
		final, err := s.PartStatus(batchPartID, nowOK.Add(2*time.Hour))
		if err != nil {
			t.Fatalf("round %d final part status: %v", round, err)
		}
		assertBatchSnapshot(t, final, 9, 4)
		if final.PhysicalRemaining != 7 || final.ActiveOccupied != 2 || final.Committable != 5 {
			t.Fatalf("round %d final account = %+v, want 7/2/5", round, final)
		}
		c9, _ := s.Commitment(batchCommit9)
		c6, _ := s.Commitment(batchCommit6)
		if c9.Used != 9 || c9.Unused() != 0 {
			t.Fatalf("round %d nine = used %d unused %d, want 9/0", round, c9.Used, c9.Unused())
		}
		if c6.Used != 4 || c6.Unused() != 2 {
			t.Fatalf("round %d six = used %d unused %d, want 4/2", round, c6.Used, c6.Unused())
		}
		if p, _ := s.Part(batchPartID); p.Stock != 7 {
			t.Fatalf("round %d physical stock = %d, want 7", round, p.Stock)
		}

		// 失败提交从未成功，编号未被占用：赛后再提交仍按超量处理，而不是编号
		// 冲突；且这次失败同样不改变任何数量。
		if _, err := s.Use("u-bad", batchCommit9, 10, nowOK.Add(3*time.Hour)); !errors.Is(err, ErrUsageExceeded) {
			t.Fatalf("round %d repeat over-use after drain: got %v, want ErrUsageExceeded", round, err)
		}
		again, _ := s.PartStatus(batchPartID, nowOK.Add(3*time.Hour))
		assertBatchSnapshot(t, again, 9, 4)

		// 并发期间取得并保留的快照在全部使用结束后仍是取得时的完整状态。
		for qi, st := range snapshots {
			assertBatchSnapshotCoherent(t, st)
			if st.Committable != 5 || st.PhysicalRemaining != 20-(mustSnapshotUsedTotal(st)) {
				t.Fatalf("round %d retained snapshot %d changed: %+v", round, qi, st)
			}
		}
	}
}

// mustSnapshotUsedTotal 取回一份已取得明细中的已用总量，用于复查保留快照不变。
func mustSnapshotUsedTotal(st *PartStatus) int {
	total := 0
	for _, d := range st.Details {
		total += d.UsedQuantity
	}
	return total
}

// TestOverUseAgainstNineCommitmentRejectedAtAnyPoint 确定性地覆盖失败条件与
// 查询的先后关系：十件超量提交分别在任何使用前、九件承诺部分使用后、以及它
// 全部使用后处理，都整次返回 ErrUsageExceeded；失败前后查询自洽，失败不造成
// 实物扣减或已用数量增加，失败编号也不被占用。
func TestOverUseAgainstNineCommitmentRejectedAtAnyPoint(t *testing.T) {
	s := batchUseStore(t)
	batchUseReserveBoth(t, s)

	// 任何正常使用之前：十 > 未用九，整次拒绝。
	before, err := s.PartStatus(batchPartID, nowOK)
	if err != nil {
		t.Fatalf("part status before: %v", err)
	}
	assertBatchSnapshot(t, before, 0, 0)
	if _, err := s.Use("u-bad", batchCommit9, 10, nowOK.Add(time.Minute)); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use before any use: got %v, want ErrUsageExceeded", err)
	}
	rejected, err := s.PartStatus(batchPartID, nowOK.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("part status after rejection: %v", err)
	}
	assertBatchSnapshot(t, rejected, 0, 0)
	if rejected.PhysicalRemaining != 20 || rejected.ActiveOccupied != 15 || rejected.Committable != 5 {
		t.Fatalf("rejection changed stock: %+v, want 20/15/5", rejected)
	}

	// 九件承诺正常使用四件后：十 > 未用五，仍整次拒绝，查询与失败交错。
	if _, err := s.Use("u-n1", batchCommit9, 4, nowOK.Add(3*time.Minute)); err != nil {
		t.Fatalf("use 4 from nine: %v", err)
	}
	mid, err := s.PartStatus(batchPartID, nowOK.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("part status mid: %v", err)
	}
	assertBatchSnapshot(t, mid, 4, 0)
	if _, err := s.Use("u-bad", batchCommit9, 10, nowOK.Add(5*time.Minute)); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use after partial use: got %v, want ErrUsageExceeded", err)
	}
	// 失败不扣减实物、不增加已用：仍是实物十六、占用十一（五加六）。
	assertBatchSnapshot(t, mid, 4, 0)
	afterMid, _ := s.PartStatus(batchPartID, nowOK.Add(6*time.Minute))
	assertBatchSnapshot(t, afterMid, 4, 0)
	if afterMid.PhysicalRemaining != 16 || afterMid.ActiveOccupied != 11 {
		t.Fatalf("mid rejection changed stock: %+v, want 16/11", afterMid)
	}

	// 完成全部正常使用：九件承诺用尽（五件），六件承诺用四件（两批各两件）。
	if _, err := s.Use("u-n2", batchCommit9, 5, nowOK.Add(7*time.Minute)); err != nil {
		t.Fatalf("use remaining 5 from nine: %v", err)
	}
	if _, err := s.Use("u-s1", batchCommit6, 2, nowOK.Add(8*time.Minute)); err != nil {
		t.Fatalf("use first 2 from six: %v", err)
	}
	if _, err := s.Use("u-s2", batchCommit6, 2, nowOK.Add(9*time.Minute)); err != nil {
		t.Fatalf("use second 2 from six: %v", err)
	}
	// 九件承诺已全部使用：十 > 未用零，依旧按超量整次拒绝。
	if _, err := s.Use("u-bad", batchCommit9, 10, nowOK.Add(10*time.Minute)); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use after exhaustion: got %v, want ErrUsageExceeded", err)
	}

	// 最终数量仍是七/二/五，三次失败提交没有造成任何扣减；用尽承诺仍显示
	// 已用九、未用零。
	final, _ := s.PartStatus(batchPartID, nowOK.Add(11*time.Minute))
	assertBatchSnapshot(t, final, 9, 4)
	if final.PhysicalRemaining != 7 || final.ActiveOccupied != 2 || final.Committable != 5 {
		t.Fatalf("final stock = %+v, want 7/2/5", final)
	}
	d9 := mustDetailByID(t, final.Details, batchCommit9)
	if d9.OriginalQuantity != 9 || d9.UsedQuantity != 9 || d9.RemainingQuantity != 0 {
		t.Fatalf("exhausted nine detail = %+v, want orig=9 used=9 remaining=0", d9)
	}

	// 使用期间保留的查询结果始终是取得时的数量。
	assertBatchSnapshot(t, before, 0, 0)
	assertBatchSnapshot(t, rejected, 0, 0)
	assertBatchSnapshot(t, mid, 4, 0)
}
