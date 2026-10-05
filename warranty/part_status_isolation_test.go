package warranty

import (
	"testing"
	"time"
)

// 本文件回归保障按备件查看库存与承诺明细（PartStatus）的返回结果相互独立：
// 每次查询都返回一份反映查询当时真实记录的全新结果，调用方为了展示而修改
// 自己手里的数量、状态或明细列表（替换、删除、补入），只能影响这份结果，
// 不能变成仓库里的库存调整、承诺使用或取消；先前已取得的结果保留查询当时
// 的内容，重新查询与仓库中保存的承诺归属和已用数量也不跟着改变。这些操作
// 只沿用 PartStatus 查询与登记、预留、使用、取消等既有公开行为，时刻均早于
// 承诺到期时刻。

// partIsolationPartID 等是本文件专用的编号，避免与其他测试文件的编号混用。
const (
	partIsolationPartID = "part-iso"
)

// partIsoStore 构造初始库存 12 件的已登记备件：可选登记一个保修 60 天的
// 产品和若干保修资格请求，用于随后预留两笔不同请求的承诺。
func partIsoStore(t *testing.T, requests ...string) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterPart(partIsolationPartID, 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if len(requests) == 0 {
		return s
	}
	if err := s.RegisterProduct("p-iso", t0, 60, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	for _, id := range requests {
		if err := s.SubmitRequest(id, "p-iso", "FAULTY"); err != nil {
			t.Fatalf("submit request %s: %v", id, err)
		}
	}
	return s
}

// partIsoExpiry 给出一个晚于全部操作时刻的到期时刻，各承诺可给不同时刻以
// 回归保障修改取回结果不会篡改其他承诺的到期时刻。
func partIsoExpiry(n int) time.Time {
	return t0.Add(time.Duration(n) * day)
}

// assertPartIsoAccount 断言某次 PartStatus 的库存三项为期望值。
func assertPartIsoAccount(t *testing.T, st *PartStatus, physical, occupied, committable int) {
	t.Helper()
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock account phys=%d occupied=%d committable=%d, want phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// assertDetailFact 断言一条明细完整反映一笔承诺的查询当时事实：归属、原定、
// 已用、未用、状态与到期时刻。
func assertDetailFact(t *testing.T, d CommitmentDetail, requestID string, original, used, remaining int, status CommitmentStatus, expiry time.Time) {
	t.Helper()
	if d.RequestID != requestID || d.PartID != partIsolationPartID ||
		d.OriginalQuantity != original || d.UsedQuantity != used || d.RemainingQuantity != remaining ||
		d.Status != status || !d.Expiry.Equal(expiry) {
		t.Fatalf("detail %q: req=%s part=%s orig=%d used=%d rem=%d status=%s expiry=%v; "+
			"want req=%s orig=%d used=%d rem=%d status=%s expiry=%v",
			d.CommitmentID, d.RequestID, d.PartID, d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status, d.Expiry,
			requestID, original, used, remaining, status, expiry)
	}
}

// TestPartStatusResultsAreIndependentAcrossQueries 同一备件被不同请求占用：
// 初始库存 12，两笔承诺分别预留 5（已使用 2）和 4，查询时两笔都未到期、
// 未取消。调用方取得两份结果后任意修改其中一份，另一份、重新查询的结果
// 以及仓库保存的承诺归属与已用数量都继续反映原有事实。
func TestPartStatusResultsAreIndependentAcrossQueries(t *testing.T) {
	s := partIsoStore(t, "req-five", "req-four")

	expFive := partIsoExpiry(40)
	expFour := partIsoExpiry(30)
	if _, err := s.Reserve("c-five", "req-five", partIsolationPartID, 5, expFive, nowOK); err != nil {
		t.Fatalf("reserve c-five: %v", err)
	}
	if _, err := s.Reserve("c-four", "req-four", partIsolationPartID, 4, expFour, nowOK); err != nil {
		t.Fatalf("reserve c-four: %v", err)
	}
	// 五件那笔已使用两件：实物剩余 10，有效占用 7（3+4），可承诺 3。
	if _, err := s.Use("u-five-2", "c-five", 2, nowOK); err != nil {
		t.Fatalf("use 2 from c-five: %v", err)
	}

	first, err := s.PartStatus(partIsolationPartID, nowOK)
	if err != nil {
		t.Fatalf("first part status: %v", err)
	}
	assertPartIsoAccount(t, first, 10, 7, 3)
	if len(first.Details) != 2 {
		t.Fatalf("first details = %d, want 2", len(first.Details))
	}
	assertDetailFact(t, mustDetailByID(t, first.Details, "c-five"), "req-five", 5, 2, 3, CommitmentActive, expFive)
	assertDetailFact(t, mustDetailByID(t, first.Details, "c-four"), "req-four", 4, 0, 4, CommitmentActive, expFour)
	// 明细按承诺编号排序。
	if first.Details[0].CommitmentID != "c-five" || first.Details[1].CommitmentID != "c-four" {
		t.Fatalf("details order = %s, %s, want c-five, c-four", first.Details[0].CommitmentID, first.Details[1].CommitmentID)
	}

	// 第二份结果随后取得，两者内容相同但必须是各自独立的结果。
	second, err := s.PartStatus(partIsolationPartID, nowOK)
	if err != nil {
		t.Fatalf("second part status: %v", err)
	}
	assertPartIsoAccount(t, second, 10, 7, 3)
	if len(second.Details) != 2 {
		t.Fatalf("second details = %d, want 2", len(second.Details))
	}

	// 调用方为了展示而修改第二份结果：改动库存数字、承诺数量与状态，并
	// 替换、删除、补入明细。这些修改只能影响第二份结果本身。
	second.PhysicalRemaining = 100
	second.ActiveOccupied = 90
	second.Committable = 10
	second.PartID = "rewritten-part"
	for i := range second.Details {
		d := &second.Details[i]
		d.OriginalQuantity += 50
		d.UsedQuantity += 40
		d.RemainingQuantity += 10
		d.RequestID = "rewritten-" + d.CommitmentID
		d.Status = CommitmentCanceled
	}
	// 替换整条 c-five 明细、删除 c-four 明细、补入一条虚构明细。
	second.Details[0] = CommitmentDetail{
		CommitmentID:      "c-five",
		RequestID:         "req-display",
		PartID:            "rewritten-part",
		OriginalQuantity:  77,
		UsedQuantity:      77,
		RemainingQuantity: 0,
		Expiry:            partIsoExpiry(99),
		Status:            CommitmentExpired,
	}
	second.Details = append(second.Details[:1], CommitmentDetail{
		CommitmentID:      "c-fabricated",
		RequestID:         "req-display",
		PartID:            "rewritten-part",
		OriginalQuantity:  6,
		UsedQuantity:      0,
		RemainingQuantity: 6,
		Expiry:            partIsoExpiry(99),
		Status:            CommitmentActive,
	})

	// 先前取得的第一份结果保留查询当时的内容。
	assertPartIsoAccount(t, first, 10, 7, 3)
	if first.PartID != partIsolationPartID {
		t.Fatalf("first result part id changed to %q", first.PartID)
	}
	if len(first.Details) != 2 {
		t.Fatalf("first details = %d after mutating second result, want 2", len(first.Details))
	}
	assertDetailFact(t, mustDetailByID(t, first.Details, "c-five"), "req-five", 5, 2, 3, CommitmentActive, expFive)
	assertDetailFact(t, mustDetailByID(t, first.Details, "c-four"), "req-four", 4, 0, 4, CommitmentActive, expFour)
	if _, ok := detailByID(first, "c-fabricated"); ok {
		t.Fatalf("first result gained fabricated detail")
	}

	// 重新查询继续反映仓库的真实记录：10/7/3，两条 active 明细原值不变，
	// 没有虚构明细，也没有任何承诺被使用或取消。
	refreshed, err := s.PartStatus(partIsolationPartID, nowOK)
	if err != nil {
		t.Fatalf("refreshed part status: %v", err)
	}
	assertPartIsoAccount(t, refreshed, 10, 7, 3)
	if refreshed.PartID != partIsolationPartID {
		t.Fatalf("refreshed part id = %q, want %q", refreshed.PartID, partIsolationPartID)
	}
	if len(refreshed.Details) != 2 {
		t.Fatalf("refreshed details = %d, want 2", len(refreshed.Details))
	}
	assertDetailFact(t, mustDetailByID(t, refreshed.Details, "c-five"), "req-five", 5, 2, 3, CommitmentActive, expFive)
	assertDetailFact(t, mustDetailByID(t, refreshed.Details, "c-four"), "req-four", 4, 0, 4, CommitmentActive, expFour)
	if _, ok := detailByID(refreshed, "c-fabricated"); ok {
		t.Fatalf("refreshed result contains fabricated detail")
	}

	// 仓库保存的承诺归属、数量与已用数量不能跟着改变。
	for _, tc := range []struct {
		id        string
		requestID string
		quantity  int
		used      int
		canceled  bool
		expired   bool
	}{
		{"c-five", "req-five", 5, 2, false, false},
		{"c-four", "req-four", 4, 0, false, false},
	} {
		c, err := s.Commitment(tc.id)
		if err != nil {
			t.Fatalf("commitment %s: %v", tc.id, err)
		}
		if c.RequestID != tc.requestID || c.Quantity != tc.quantity || c.Used != tc.used ||
			c.Canceled != tc.canceled || c.Expired != tc.expired {
			t.Fatalf("stored commitment %s disturbed: %+v", tc.id, c)
		}
	}
}

// TestPartStatusSnapshotStaysFixedAcrossLaterUseAndCancel 保留一份未改动的
// 查询结果后，从四件那笔承诺再使用一件、再取消五件那笔承诺：最新查询显示
// 实物剩余 9、有效占用 3、可承诺 6；取消明细保留原定 5、已用 2、未用 3，
// 另一笔显示原定 4、已用 1、未用 3。先前保存的结果仍是 10/7/3 与当时两笔
// 承诺的 active 状态和数量，不随仓库变化被改写。取消只释放未用占用，已使用
// 的实物不补回。
func TestPartStatusSnapshotStaysFixedAcrossLaterUseAndCancel(t *testing.T) {
	s := partIsoStore(t, "req-five", "req-four")

	expFive := partIsoExpiry(40)
	expFour := partIsoExpiry(30)
	if _, err := s.Reserve("c-five", "req-five", partIsolationPartID, 5, expFive, nowOK); err != nil {
		t.Fatalf("reserve c-five: %v", err)
	}
	if _, err := s.Reserve("c-four", "req-four", partIsolationPartID, 4, expFour, nowOK); err != nil {
		t.Fatalf("reserve c-four: %v", err)
	}
	if _, err := s.Use("u-five-2", "c-five", 2, nowOK); err != nil {
		t.Fatalf("use 2 from c-five: %v", err)
	}

	saved, err := s.PartStatus(partIsolationPartID, nowOK)
	if err != nil {
		t.Fatalf("saved part status: %v", err)
	}
	assertPartIsoAccount(t, saved, 10, 7, 3)

	// 从四件那笔承诺再使用一件：实物 9，有效占用 6（3+3），可承诺 3。
	later := t0.Add(11 * day)
	if _, err := s.Use("u-four-1", "c-four", 1, later); err != nil {
		t.Fatalf("use 1 from c-four: %v", err)
	}
	// 取消五件那笔承诺：只释放其未用的 3 件，已用 2 件不补回实物。
	if _, err := s.Cancel("c-five", later); err != nil {
		t.Fatalf("cancel c-five: %v", err)
	}

	// 最新查询：实物 9、有效占用 3、可承诺 6。
	latest, err := s.PartStatus(partIsolationPartID, later)
	if err != nil {
		t.Fatalf("latest part status: %v", err)
	}
	assertPartIsoAccount(t, latest, 9, 3, 6)
	if len(latest.Details) != 2 {
		t.Fatalf("latest details = %d, want 2", len(latest.Details))
	}
	// 取消的明细仍保留原定 5、已用 2、未用 3 并显示 canceled。
	assertDetailFact(t, mustDetailByID(t, latest.Details, "c-five"), "req-five", 5, 2, 3, CommitmentCanceled, expFive)
	// 另一笔显示原定 4、已用 1、未用 3，仍有效。
	assertDetailFact(t, mustDetailByID(t, latest.Details, "c-four"), "req-four", 4, 1, 3, CommitmentActive, expFour)

	// 先前保存的结果仍是十、七、三，以及当时两笔承诺的数量与 active 状态。
	assertPartIsoAccount(t, saved, 10, 7, 3)
	if len(saved.Details) != 2 {
		t.Fatalf("saved details = %d, want 2", len(saved.Details))
	}
	assertDetailFact(t, mustDetailByID(t, saved.Details, "c-five"), "req-five", 5, 2, 3, CommitmentActive, expFive)
	assertDetailFact(t, mustDetailByID(t, saved.Details, "c-four"), "req-four", 4, 0, 4, CommitmentActive, expFour)

	// 仓库事实与最新查询一致：取消不抹掉已用数量，实物不补回。
	cFive, _ := s.Commitment("c-five")
	if !cFive.Canceled || cFive.Quantity != 5 || cFive.Used != 2 || cFive.Unused() != 3 {
		t.Fatalf("stored c-five: %+v", cFive)
	}
	cFour, _ := s.Commitment("c-four")
	if cFour.Canceled || cFour.Quantity != 4 || cFour.Used != 1 || cFour.Unused() != 3 {
		t.Fatalf("stored c-four: %+v", cFour)
	}
	p, _ := s.Part(partIsolationPartID)
	if p.Stock != 9 {
		t.Fatalf("physical stock = %d, want 9", p.Stock)
	}
}

// TestPartStatusEmptyDetailsSnapshotStaysEmpty 没有承诺时已登记备件的明细
// 为空，实物剩余与可承诺数量相同；后来正常预留成功，新查询出现承诺明细，
// 先前取得的空结果仍保持为空，其库存数字也不随后续使用或预留改写。
func TestPartStatusEmptyDetailsSnapshotStaysEmpty(t *testing.T) {
	s := partIsoStore(t, "req-later")

	empty, err := s.PartStatus(partIsolationPartID, nowOK)
	if err != nil {
		t.Fatalf("empty part status: %v", err)
	}
	if empty.PhysicalRemaining != 12 || empty.ActiveOccupied != 0 || empty.Committable != 12 {
		t.Fatalf("empty account: %+v, want phys=12 occupied=0 committable=12", empty)
	}
	if len(empty.Details) != 0 {
		t.Fatalf("empty details = %d, want 0", len(empty.Details))
	}

	// 后来正常预留成功（时刻仍早于到期时刻）：新查询出现承诺明细。
	expLater := partIsoExpiry(40)
	if _, err := s.Reserve("c-later", "req-later", partIsolationPartID, 5, expLater, nowOK); err != nil {
		t.Fatalf("reserve c-later: %v", err)
	}
	if _, err := s.Use("u-later-2", "c-later", 2, nowOK); err != nil {
		t.Fatalf("use 2 from c-later: %v", err)
	}
	latest, err := s.PartStatus(partIsolationPartID, nowOK)
	if err != nil {
		t.Fatalf("latest part status: %v", err)
	}
	if latest.PhysicalRemaining != 10 || latest.ActiveOccupied != 3 || latest.Committable != 7 {
		t.Fatalf("latest account: phys=%d occupied=%d committable=%d, want 10/3/7",
			latest.PhysicalRemaining, latest.ActiveOccupied, latest.Committable)
	}
	if len(latest.Details) != 1 {
		t.Fatalf("latest details = %d, want 1", len(latest.Details))
	}
	assertDetailFact(t, latest.Details[0], "req-later", 5, 2, 3, CommitmentActive, expLater)

	// 先前的空结果仍保持为空，三项数字也保留当时内容。
	if len(empty.Details) != 0 {
		t.Fatalf("earlier empty result gained details: %+v", empty.Details)
	}
	if empty.PhysicalRemaining != 12 || empty.ActiveOccupied != 0 || empty.Committable != 12 {
		t.Fatalf("earlier empty result account changed: phys=%d occupied=%d committable=%d, want 12/0/12",
			empty.PhysicalRemaining, empty.ActiveOccupied, empty.Committable)
	}
}
