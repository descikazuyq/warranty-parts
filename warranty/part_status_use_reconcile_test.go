package warranty

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归保障“按备件查看库存与承诺明细”与“分批使用”同时发生时，每一份
// PartStatus 返回结果内部的数量必须互相核对、属于同一个完整状态：查询可能
// 恰好看到某次使用处理前或处理后的结果，但不能混用不同阶段的数量。
//
// 主例数据（与用户描述一致）：备件 part-recon 初始实物库存二十件，两笔在保
// 请求分别预留九件（c-nine，属于 r-recon-nine）和六件（c-six，属于
// r-recon-six），查询开始前均未使用：实物剩余二十件、有效占用十五件、可承诺
// 五件。随后用互不相同的使用编号交错分批使用：九件承诺先用五件再用四件（共
// 九件，全部用完），六件承诺先用一件再用三件（共四件，未用两件）。所有当前
// 时刻都早于两笔承诺的到期时刻，承诺始终未取消。
//
// 每份查询结果都必须满足：每条明细原定数量等于已用加未用；有效占用等于两条
// 明细未用数量之和；实物剩余等于二十件减去两条明细已用总量；可承诺数量始终
// 为五件。两条明细始终保留各自的请求归属、原定数量与到期时刻，既不因分批
// 使用丢失或重复，也继续按承诺编号排序。使用期间取得并保留的结果保留取得时
// 的数量，不随后续使用变化。
//
// 同时覆盖失败条件：用一个从未成功使用过的编号向九件承诺提交十件使用量。
// 九件承诺在整个过程中的未用数量最多九件，因此无论该提交与查询、正常使用
// 谁先处理，都必须返回 ErrUsageExceeded 整次拒绝：不扣减实物库存、不增加
// 任何承诺的已用数量、不占用该使用编号。

const (
	reconPartID      = "part-recon"
	reconProductID   = "p-recon"
	reconNineRequest = "r-recon-nine"
	reconSixRequest  = "r-recon-six"
	reconNineCommit  = "c-nine"
	reconSixCommit   = "c-six"
	reconStock       = 20
)

// 两笔承诺给不同的到期时刻，均晚于本文件全部操作时刻，以便回归保障分批使用
// 不会篡改任一笔承诺的到期时刻。
var (
	reconExpiryNine = t0.Add(50 * day)
	reconExpirySix  = t0.Add(45 * day)
)

// reconNewStore 构造主例初始状态：初始库存二十件的备件、两笔合格请求，以及
// 分别预留九件和六件、当时均未使用的两笔承诺。
func reconNewStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct(reconProductID, t0, 120, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(reconPartID, reconStock); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(reconNineRequest, reconProductID, "NOISE"); err != nil {
		t.Fatalf("submit nine request: %v", err)
	}
	if err := s.SubmitRequest(reconSixRequest, reconProductID, "NOISE"); err != nil {
		t.Fatalf("submit six request: %v", err)
	}
	if _, err := s.Reserve(reconNineCommit, reconNineRequest, reconPartID, 9, reconExpiryNine, nowOK); err != nil {
		t.Fatalf("reserve c-nine: %v", err)
	}
	if _, err := s.Reserve(reconSixCommit, reconSixRequest, reconPartID, 6, reconExpirySix, nowOK); err != nil {
		t.Fatalf("reserve c-six: %v", err)
	}
	return s
}

// assertReconAccount 断言一份查询结果的库存三项为期望值。
func assertReconAccount(t *testing.T, st *PartStatus, physical, occupied, committable int) {
	t.Helper()
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock account phys=%d occupied=%d committable=%d, want phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// assertReconDetail 断言一条明细完整保留一笔承诺在查询当时的归属、原定数量、
// 到期时刻与有效状态，以及指定的已用、未用数量。
func assertReconDetail(t *testing.T, d CommitmentDetail, commitID, requestID string, original, used, remaining int, expiry time.Time) {
	t.Helper()
	if d.CommitmentID != commitID || d.RequestID != requestID || d.PartID != reconPartID ||
		d.OriginalQuantity != original || d.UsedQuantity != used || d.RemainingQuantity != remaining ||
		d.Status != CommitmentActive || !d.Expiry.Equal(expiry) {
		t.Fatalf("detail %q: part=%s req=%s orig=%d used=%d rem=%d status=%s expiry=%v; "+
			"want part=%s req=%s orig=%d used=%d rem=%d status=%s expiry=%v",
			d.CommitmentID, d.PartID, d.RequestID, d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status, d.Expiry,
			reconPartID, requestID, original, used, remaining, CommitmentActive, expiry)
	}
}

// assertReconSnapshotReconciles 核对一份查询结果内部的数量属于同一个完整状态：
//   - 恰好两条明细，按承诺编号 c-nine、c-six 排序，归属、原定数量与到期时刻
//     分别保留，且都仍有效（本文件没有取消或到期）；
//   - 每条明细原定数量等于已用加未用，数量非负；
//   - 有效占用等于两条明细未用数量之和；
//   - 实物剩余等于二十件减去两条明细已用总量；
//   - 可承诺数量始终为五件。
//
// 返回两条明细当时的已用数量，供调用方进一步核对。
func assertReconSnapshotReconciles(t *testing.T, st *PartStatus) (usedNine, usedSix int) {
	t.Helper()
	if st == nil {
		t.Fatal("nil part status")
	}
	if st.PartID != reconPartID {
		t.Fatalf("part id = %q, want %q", st.PartID, reconPartID)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details = %d, want exactly 2 (no loss or duplication): %+v", len(st.Details), st.Details)
	}
	d9, d6 := st.Details[0], st.Details[1]
	if d9.CommitmentID != reconNineCommit || d6.CommitmentID != reconSixCommit {
		t.Fatalf("details order = %q, %q, want sorted %q, %q",
			d9.CommitmentID, d6.CommitmentID, reconNineCommit, reconSixCommit)
	}
	if d9.UsedQuantity < 0 || d9.RemainingQuantity < 0 || d6.UsedQuantity < 0 || d6.RemainingQuantity < 0 {
		t.Fatalf("negative quantities in details: %+v, %+v", d9, d6)
	}
	if d9.UsedQuantity+d9.RemainingQuantity != 9 {
		t.Fatalf("c-nine detail orig=%d used=%d rem=%d, want used+rem=9", d9.OriginalQuantity, d9.UsedQuantity, d9.RemainingQuantity)
	}
	if d6.UsedQuantity+d6.RemainingQuantity != 6 {
		t.Fatalf("c-six detail orig=%d used=%d rem=%d, want used+rem=6", d6.OriginalQuantity, d6.UsedQuantity, d6.RemainingQuantity)
	}
	assertReconDetail(t, d9, reconNineCommit, reconNineRequest, 9, d9.UsedQuantity, d9.RemainingQuantity, reconExpiryNine)
	assertReconDetail(t, d6, reconSixCommit, reconSixRequest, 6, d6.UsedQuantity, d6.RemainingQuantity, reconExpirySix)

	usedTotal := d9.UsedQuantity + d6.UsedQuantity
	unusedTotal := d9.RemainingQuantity + d6.RemainingQuantity
	if st.ActiveOccupied != unusedTotal {
		t.Fatalf("active occupied = %d, want sum of remaining quantities %d (details: %d/%d used)",
			st.ActiveOccupied, unusedTotal, d9.UsedQuantity, d6.UsedQuantity)
	}
	if st.PhysicalRemaining != reconStock-usedTotal {
		t.Fatalf("physical remaining = %d, want %d - used total %d",
			st.PhysicalRemaining, reconStock, usedTotal)
	}
	if st.Committable != 5 {
		t.Fatalf("committable = %d, want always 5", st.Committable)
	}
	if st.PhysicalRemaining-st.ActiveOccupied != st.Committable {
		t.Fatalf("account does not reconcile: phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	return d9.UsedQuantity, d6.UsedQuantity
}

// assertReconReachable 断言查询看到的已用数量对应“某次正常使用处理前或处理
// 后”的完整状态：九件承诺的已用数量只能是 0、5、4、9（先五后四的任一先后
// 组合），六件承诺只能是 0、1、3、4（先一后三的任一先后组合）。任何其他
// 取值都意味着查询看到了只完成一半的扣减，即混用了不同阶段的数量。
func assertReconReachable(t *testing.T, usedNine, usedSix int) {
	t.Helper()
	switch usedNine {
	case 0, 4, 5, 9:
	default:
		t.Fatalf("c-nine used = %d in a snapshot, want one of 0/4/5/9 (whole-use boundaries only)", usedNine)
	}
	switch usedSix {
	case 0, 1, 3, 4:
	default:
		t.Fatalf("c-six used = %d in a snapshot, want one of 0/1/3/4 (whole-use boundaries only)", usedSix)
	}
}

// TestPartStatusBatchUseInterleavedSnapshots 顺序复现交错分批使用的完整过程：
// 初始 20/15/5；九件先用五件、六件先用一件后查询得 14/9/5；期间用新编号提交
// 十件被整次拒绝且数量不变；随后九件再用四件全部用完、六件再用三件共用四件，
// 查询得 7/2/5，已用完的承诺仍在明细中显示已用九件、未用零件。使用途中取得
// 并保留的两份结果不随后续使用变化。
func TestPartStatusBatchUseInterleavedSnapshots(t *testing.T) {
	s := reconNewStore(t)

	// 查询开始前均未使用：实物剩余二十、有效占用十五、可承诺五。
	before, err := s.PartStatus(reconPartID, nowOK)
	if err != nil {
		t.Fatalf("part status before uses: %v", err)
	}
	assertReconAccount(t, before, 20, 15, 5)
	u9, u6 := assertReconSnapshotReconciles(t, before)
	assertReconReachable(t, u9, u6)
	assertReconDetail(t, before.Details[0], reconNineCommit, reconNineRequest, 9, 0, 9, reconExpiryNine)
	assertReconDetail(t, before.Details[1], reconSixCommit, reconSixRequest, 6, 0, 6, reconExpirySix)

	// 交错分批使用：九件承诺先用五件，六件承诺先用一件。
	if _, err := s.Use("u-nine-5", reconNineCommit, 5, nowOK); err != nil {
		t.Fatalf("use 5 from c-nine: %v", err)
	}
	if _, err := s.Use("u-six-1", reconSixCommit, 1, nowOK); err != nil {
		t.Fatalf("use 1 from c-six: %v", err)
	}

	// 使用期间查询：实物 20-6=14，有效占用 4+5=9，可承诺仍为五。
	mid, err := s.PartStatus(reconPartID, nowOK.Add(time.Minute))
	if err != nil {
		t.Fatalf("part status midway: %v", err)
	}
	assertReconAccount(t, mid, 14, 9, 5)
	assertReconSnapshotReconciles(t, mid)
	assertReconDetail(t, mid.Details[0], reconNineCommit, reconNineRequest, 9, 5, 4, reconExpiryNine)
	assertReconDetail(t, mid.Details[1], reconSixCommit, reconSixRequest, 6, 1, 5, reconExpirySix)

	// 失败条件：用尚未成功过的编号向九件承诺提交十件（其未用仅四件）。
	// 即使实物仍有十四件、另一笔承诺还有五件未用，也必须 ErrUsageExceeded
	// 整次拒绝，返回空记录。
	rejected, err := s.Use("u-bad-ten", reconNineCommit, 10, nowOK.Add(2*time.Minute))
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use 10 over unused 4: got %v, want ErrUsageExceeded", err)
	}
	if rejected != (Usage{}) {
		t.Fatalf("rejected usage returned non-empty record: %+v", rejected)
	}

	// 整次失败不扣减实物、不增加承诺已用数量：紧接查询与失败前完全一致。
	afterReject, err := s.PartStatus(reconPartID, nowOK.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("part status after rejected use: %v", err)
	}
	assertReconAccount(t, afterReject, 14, 9, 5)
	assertReconDetail(t, afterReject.Details[0], reconNineCommit, reconNineRequest, 9, 5, 4, reconExpiryNine)
	assertReconDetail(t, afterReject.Details[1], reconSixCommit, reconSixRequest, 6, 1, 5, reconExpirySix)
	if c := mustCommitment(t, s, reconNineCommit); c.Used != 5 || c.Unused() != 4 || c.Canceled || c.Expired {
		t.Fatalf("c-nine disturbed by rejected use: %+v", c)
	}
	if p, err := s.Part(reconPartID); err != nil || p.Stock != 14 {
		t.Fatalf("physical stock after rejected use = %d (err %v), want 14", p.Stock, err)
	}

	// 继续完成全部正常使用：九件再用四件（全部用完），六件再用三件（共用四件）。
	if _, err := s.Use("u-nine-4", reconNineCommit, 4, nowOK.Add(4*time.Minute)); err != nil {
		t.Fatalf("use 4 from c-nine: %v", err)
	}
	if _, err := s.Use("u-six-3", reconSixCommit, 3, nowOK.Add(5*time.Minute)); err != nil {
		t.Fatalf("use 3 from c-six: %v", err)
	}

	// 使用处理均已结束：实物剩余七件、有效占用两件、可承诺五件。已全部使用的
	// 承诺仍在明细中，显示已用九件、未用零件，归属、原定数量与到期时刻不变。
	final, err := s.PartStatus(reconPartID, nowOK.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("final part status: %v", err)
	}
	assertReconAccount(t, final, 7, 2, 5)
	assertReconSnapshotReconciles(t, final)
	assertReconDetail(t, final.Details[0], reconNineCommit, reconNineRequest, 9, 9, 0, reconExpiryNine)
	assertReconDetail(t, final.Details[1], reconSixCommit, reconSixRequest, 6, 4, 2, reconExpirySix)

	// 使用途中取得并保留的两份结果保留取得时的数量，不随后续使用变化。
	assertReconAccount(t, before, 20, 15, 5)
	assertReconDetail(t, before.Details[0], reconNineCommit, reconNineRequest, 9, 0, 9, reconExpiryNine)
	assertReconDetail(t, before.Details[1], reconSixCommit, reconSixRequest, 6, 0, 6, reconExpirySix)
	assertReconAccount(t, mid, 14, 9, 5)
	assertReconDetail(t, mid.Details[0], reconNineCommit, reconNineRequest, 9, 5, 4, reconExpiryNine)
	assertReconDetail(t, mid.Details[1], reconSixCommit, reconSixRequest, 6, 1, 5, reconExpirySix)

	// 仓库保存的事实与最终查询一致：失败的十件提交没有留下任何扣减。
	if c := mustCommitment(t, s, reconNineCommit); c.Quantity != 9 || c.Used != 9 || c.Unused() != 0 || c.Canceled || c.Expired {
		t.Fatalf("stored c-nine: %+v, want qty=9 used=9 unused=0 active", c)
	}
	if c := mustCommitment(t, s, reconSixCommit); c.Quantity != 6 || c.Used != 4 || c.Unused() != 2 || c.Canceled || c.Expired {
		t.Fatalf("stored c-six: %+v, want qty=6 used=4 unused=2 active", c)
	}
	if p, err := s.Part(reconPartID); err != nil || p.Stock != 7 {
		t.Fatalf("physical stock = %d (err %v), want 7", p.Stock, err)
	}
}

// reconFloodSpec 是并发洪泛中的一笔使用提交内容。
type reconFloodSpec struct {
	usageID  string
	commitID string
	quantity int
}

// reconFloodNormalSpecs 是四笔正常分批使用（两笔承诺各两批）。
var reconFloodNormalSpecs = []reconFloodSpec{
	{"u-nine-5", reconNineCommit, 5},
	{"u-nine-4", reconNineCommit, 4},
	{"u-six-1", reconSixCommit, 1},
	{"u-six-3", reconSixCommit, 3},
}

// fanOutReconFlood 让四笔正常使用（每笔多个相同内容副本）、十件失败提交
// （多个副本）和大量 PartStatus 查询在同一道闸机释放后同时运行，尽量制造
// 查询与使用的交叉。返回各使用调用的结果和查询期间取得的全部快照。
func fanOutReconFlood(t *testing.T, s *Store, replicas, queryWorkers, queriesEach int) ([]concurrentUseResult, []*PartStatus) {
	t.Helper()
	specs := append([]reconFloodSpec{}, reconFloodNormalSpecs...)
	specs = append(specs, reconFloodSpec{"u-bad-ten", reconNineCommit, 10})

	calls := make([]concurrentUseCall, 0, replicas*len(specs))
	for rep := 0; rep < replicas; rep++ {
		for k, sp := range specs {
			// 各副本给不同但都早于到期时刻的当前时刻；当前时刻不属于使用编号
			// 的绑定内容。
			calls = append(calls, concurrentUseCall{
				usageID:  sp.usageID,
				commitID: sp.commitID,
				quantity: sp.quantity,
				now:      nowOK.Add(time.Duration(rep*len(specs)+k) * time.Minute),
			})
		}
	}
	results := make([]concurrentUseResult, len(calls))

	// 每个查询 goroutine 只写自己名下的切片，闸机等待结束后主 goroutine 才
	// 读取，因此无需额外加锁。
	workerSnapshots := make([][]*PartStatus, queryWorkers)
	workerErrors := make([]error, queryWorkers)

	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c concurrentUseCall) {
			defer wg.Done()
			<-barrier
			results[i].usage, results[i].err = s.Use(c.usageID, c.commitID, c.quantity, c.now)
		}(i, c)
	}
	for w := 0; w < queryWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-barrier
			snaps := make([]*PartStatus, 0, queriesEach)
			for q := 0; q < queriesEach; q++ {
				// 查询时刻始终早于两笔到期时刻，承诺不发生到期确认。
				st, err := s.PartStatus(reconPartID, nowOK.Add(time.Duration(w*queriesEach+q)*time.Second))
				if err != nil {
					workerErrors[w] = err
					return
				}
				snaps = append(snaps, st)
			}
			workerSnapshots[w] = snaps
		}(w)
	}
	close(barrier)
	wg.Wait()

	for w, err := range workerErrors {
		if err != nil {
			t.Fatalf("concurrent part status query in worker %d: %v", w, err)
		}
	}
	var snapshots []*PartStatus
	for _, snaps := range workerSnapshots {
		snapshots = append(snapshots, snaps...)
	}
	return results, snapshots
}

// TestPartStatusQueriesConcurrentWithBatchUses 不规定查询与使用的完成先后：
// 四笔正常使用（相同内容并发副本只产生一次真实扣减）、十件失败提交（每个副本
// 都失败）与大量查询完全并发。每份查询都必须自洽（同一完整状态），且只能
// 观察到某次使用处理前或处理后的整数边界状态。全部结束后数量确定为 7/2/5。
func TestPartStatusQueriesConcurrentWithBatchUses(t *testing.T) {
	const iterations = 6
	for iter := 0; iter < iterations; iter++ {
		s := reconNewStore(t)

		results, snapshots := fanOutReconFlood(t, s, 8, 8, 25)
		if len(snapshots) == 0 {
			t.Fatal("no snapshots captured")
		}

		// 成功记录必须是四笔正常使用之一，数量、承诺与编号一一对应；其余调用
		// （十件超量提交的全部相同内容副本）必须 ErrUsageExceeded 整次拒绝、
		// 返回空记录。
		for i, r := range results {
			if r.err != nil {
				if !errors.Is(r.err, ErrUsageExceeded) || r.usage != (Usage{}) {
					t.Fatalf("iter %d call %d: usage=%+v err=%v, want ErrUsageExceeded with empty result", iter, i, r.usage, r.err)
				}
				continue
			}
			want, ok := mapUsageRecord(r.usage)
			if !ok {
				t.Fatalf("iter %d call %d: unexpected successful usage %+v", iter, i, r.usage)
			}
			if r.usage != want {
				t.Fatalf("iter %d call %d: usage=%+v, want first record %+v", iter, i, r.usage, want)
			}
		}

		// 使用期间取得的每一份结果都必须内部自洽，且处于某个完整使用边界上。
		for _, st := range snapshots {
			u9, u6 := assertReconSnapshotReconciles(t, st)
			assertReconReachable(t, u9, u6)
		}

		// 全部处理结束：实物剩余七件、有效占用两件、可承诺五件。
		final, err := s.PartStatus(reconPartID, nowOK.Add(time.Hour))
		if err != nil {
			t.Fatalf("iter %d final part status: %v", iter, err)
		}
		assertReconAccount(t, final, 7, 2, 5)
		assertReconSnapshotReconciles(t, final)
		assertReconDetail(t, final.Details[0], reconNineCommit, reconNineRequest, 9, 9, 0, reconExpiryNine)
		assertReconDetail(t, final.Details[1], reconSixCommit, reconSixRequest, 6, 4, 2, reconExpirySix)

		// 仓库事实与最终查询一致。
		if c := mustCommitment(t, s, reconNineCommit); c.Used != 9 || c.Unused() != 0 {
			t.Fatalf("iter %d stored c-nine: %+v, want used=9 unused=0", iter, c)
		}
		if c := mustCommitment(t, s, reconSixCommit); c.Used != 4 || c.Unused() != 2 {
			t.Fatalf("iter %d stored c-six: %+v, want used=4 unused=2", iter, c)
		}
		if p, err := s.Part(reconPartID); err != nil || p.Stock != 7 {
			t.Fatalf("iter %d physical stock = %d (err %v), want 7", iter, p.Stock, err)
		}

		// 调用方保留的快照不随最终状态变化：全部仍是取得时的完整状态。
		for _, st := range snapshots {
			assertReconSnapshotReconciles(t, st)
		}
	}
}

// mapUsageRecord 判定一份成功使用记录是否对应四笔正常分批使用之一。
func mapUsageRecord(u Usage) (Usage, bool) {
	for _, sp := range reconFloodNormalSpecs {
		want := Usage{ID: sp.usageID, CommitmentID: sp.commitID, Quantity: sp.quantity}
		if u == want {
			return want, true
		}
	}
	return Usage{}, false
}

// TestPartStatusQueriesConcurrentWithRejectedTenPieceUse 在没有任何正常使用
// 介入时，让十件超量提交的多个副本与查询完全并发：九件承诺的未用数量始终为
// 九，十件提交无论先处理还是后处理都必须 ErrUsageExceeded，期间每份查询都
// 保持初始的 20/15/5，失败提交不造成实物扣减或已用数量增加。
func TestPartStatusQueriesConcurrentWithRejectedTenPieceUse(t *testing.T) {
	const iterations = 6
	for iter := 0; iter < iterations; iter++ {
		s := reconNewStore(t)

		const submitters = 12
		results := make([]concurrentUseResult, submitters)
		const queryWorkers, queriesEach = 8, 20
		workerSnapshots := make([][]*PartStatus, queryWorkers)
		workerErrors := make([]error, queryWorkers)

		barrier := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < submitters; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-barrier
				results[i].usage, results[i].err = s.Use(
					"u-bad-ten", reconNineCommit, 10,
					nowOK.Add(time.Duration(i)*time.Minute))
			}(i)
		}
		for w := 0; w < queryWorkers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-barrier
				snaps := make([]*PartStatus, 0, queriesEach)
				for q := 0; q < queriesEach; q++ {
					st, err := s.PartStatus(reconPartID, nowOK.Add(time.Duration(w*queriesEach+q)*time.Second))
					if err != nil {
						workerErrors[w] = err
						return
					}
					snaps = append(snaps, st)
				}
				workerSnapshots[w] = snaps
			}(w)
		}
		close(barrier)
		wg.Wait()

		for w, err := range workerErrors {
			if err != nil {
				t.Fatalf("iter %d query worker %d: %v", iter, w, err)
			}
		}

		// 同一个新编号的全部并发提交都按超量使用整次拒绝；编号未被占用，
		// 不会有任何一份“抢先成功”。
		for i, r := range results {
			if !errors.Is(r.err, ErrUsageExceeded) || r.usage != (Usage{}) {
				t.Fatalf("iter %d submitter %d: usage=%+v err=%v, want ErrUsageExceeded with empty result",
					iter, i, r.usage, r.err)
			}
		}

		// 与查询谁先处理都一样：期间每份结果都是初始的 20/15/5 完整状态。
		var count int
		for _, snaps := range workerSnapshots {
			for _, st := range snaps {
				count++
				assertReconAccount(t, st, 20, 15, 5)
				assertReconSnapshotReconciles(t, st)
				assertReconDetail(t, st.Details[0], reconNineCommit, reconNineRequest, 9, 0, 9, reconExpiryNine)
				assertReconDetail(t, st.Details[1], reconSixCommit, reconSixRequest, 6, 0, 6, reconExpirySix)
			}
		}
		if count == 0 {
			t.Fatalf("iter %d: no snapshots captured", iter)
		}

		// 结束后承诺与实物库存保持初始值。
		if c := mustCommitment(t, s, reconNineCommit); c.Used != 0 || c.Unused() != 9 || c.Canceled || c.Expired {
			t.Fatalf("iter %d c-nine after rejected flood: %+v, want used=0 unused=9 active", iter, c)
		}
		if c := mustCommitment(t, s, reconSixCommit); c.Used != 0 || c.Unused() != 6 {
			t.Fatalf("iter %d c-six after rejected flood: %+v, want used=0 unused=6", iter, c)
		}
		if p, err := s.Part(reconPartID); err != nil || p.Stock != 20 {
			t.Fatalf("iter %d physical stock = %d (err %v), want 20", iter, p.Stock, err)
		}
	}
}
