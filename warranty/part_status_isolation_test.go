package warranty

import (
	"testing"
	"time"
)

// 本文件回归保障按备件查询（PartStatus）返回结果相互独立：每次查询反映
// 查询当时仓库中的真实记录，调用方保留一份结果用于核对、为了展示而修改
// 手里的库存数字、承诺数量、状态或明细列表时，这些修改只能影响这份结果，
// 不能变成仓库里的库存调整、承诺使用或取消，也不能改写先前已取得的另一份
// 结果或随后重新查询的结果。范围只涉及 PartStatus 查询结果，不引入新操作。

// partSnapshotStore 构造同一备件被两个请求占用的初始局面：
// 初始库存 12 件；c-five 为 r-five 预留 5 件并已使用 2 件，c-four 为
// r-four 预留 4 件尚未使用。两笔承诺到期时刻相同且晚于全部操作与查询时刻。
// 此时实物剩余 10 件、有效占用 7 件（3+4）、可承诺 3 件。
func partSnapshotStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r-five", "p1", "NOISE"); err != nil {
		t.Fatalf("submit r-five: %v", err)
	}
	if err := s.SubmitRequest("r-four", "p1", "NOISE"); err != nil {
		t.Fatalf("submit r-four: %v", err)
	}
	if _, err := s.Reserve("c-five", "r-five", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c-five: %v", err)
	}
	if _, err := s.Reserve("c-four", "r-four", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c-four: %v", err)
	}
	if _, err := s.Use("u-five-2", "c-five", 2, nowOK); err != nil {
		t.Fatalf("use 2 from c-five: %v", err)
	}
	return s
}

// assertInitialPartSnapshot 断言一份查询结果正是 10/7/3 的初始局面，
// 两条明细分别保留各自的请求归属与原定、已用、未用数量，且均为 active。
func assertInitialPartSnapshot(t *testing.T, st *PartStatus) {
	t.Helper()
	if st.PartID != "part1" {
		t.Fatalf("part id = %q, want part1", st.PartID)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 7 || st.Committable != 3 {
		t.Fatalf("stock account = %+v, want phys=10 occupied=7 committable=3", st)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details len = %d, want 2", len(st.Details))
	}
	d5 := mustDetailByID(t, st.Details, "c-five")
	if d5.RequestID != "r-five" || d5.PartID != "part1" ||
		d5.OriginalQuantity != 5 || d5.UsedQuantity != 2 || d5.RemainingQuantity != 3 ||
		d5.Status != CommitmentActive || !d5.Expiry.Equal(expiryOK) {
		t.Fatalf("c-five detail = %+v, want request r-five 5/2/3 active", d5)
	}
	d4 := mustDetailByID(t, st.Details, "c-four")
	if d4.RequestID != "r-four" || d4.PartID != "part1" ||
		d4.OriginalQuantity != 4 || d4.UsedQuantity != 0 || d4.RemainingQuantity != 4 ||
		d4.Status != CommitmentActive || !d4.Expiry.Equal(expiryOK) {
		t.Fatalf("c-four detail = %+v, want request r-four 4/0/4 active", d4)
	}
}

// TestPartStatusSnapshotsAreIndependent 取得两份结果后，改动其中一份的
// 库存数字、承诺数量与状态，并替换、删除、补入明细：另一份已取得的结果
// 与随后重新查询的结果都继续反映原有事实，仓库保存的承诺归属、已用数量
// 与实物库存也不跟着改变。
func TestPartStatusSnapshotsAreIndependent(t *testing.T) {
	s := partSnapshotStore(t)

	first, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("first part status: %v", err)
	}
	second, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("second part status: %v", err)
	}
	assertInitialPartSnapshot(t, first)
	assertInitialPartSnapshot(t, second)

	// 调用方为了展示，就地篡改手里第一份结果：库存与占用数字、可承诺量、
	// 备件编号，以及明细中的承诺数量、已用数量、请求归属和状态。
	first.PartID = "part-mutated"
	first.PhysicalRemaining = 999
	first.ActiveOccupied = 111
	first.Committable = -5
	for i := range first.Details {
		d := &first.Details[i]
		d.RequestID = "req-mutated"
		d.OriginalQuantity += 50
		d.UsedQuantity += 20
		d.RemainingQuantity += 30
		d.Status = CommitmentCanceled
	}
	// 替换一条明细、删除一条明细、再补入一条仓库里不存在的明细。
	first.Details[0] = CommitmentDetail{CommitmentID: "c-fabricated", OriginalQuantity: 7}
	first.Details = append(first.Details[:1], CommitmentDetail{CommitmentID: "c-ghost", OriginalQuantity: 9})

	// 先前已取得的另一份结果不被污染。
	assertInitialPartSnapshot(t, second)

	// 随后重新查询仍反映查询当时的真实记录。
	again, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("re-query part status: %v", err)
	}
	assertInitialPartSnapshot(t, again)

	// 仓库保存的承诺归属、原定与已用数量不变。
	c5, err := s.Commitment("c-five")
	if err != nil {
		t.Fatalf("commitment c-five: %v", err)
	}
	if c5.RequestID != "r-five" || c5.Quantity != 5 || c5.Used != 2 || c5.Canceled || c5.Expired {
		t.Fatalf("c-five repository record mutated: %+v", c5)
	}
	c4, err := s.Commitment("c-four")
	if err != nil {
		t.Fatalf("commitment c-four: %v", err)
	}
	if c4.RequestID != "r-four" || c4.Quantity != 4 || c4.Used != 0 || c4.Canceled || c4.Expired {
		t.Fatalf("c-four repository record mutated: %+v", c4)
	}
	if p, err := s.Part("part1"); err != nil || p.Stock != 10 {
		t.Fatalf("physical stock mutated: %+v err %v", p, err)
	}
}

// TestPartStatusSnapshotSurvivesLaterUseAndCancel 保留一份未改动的查询结果后，
// 正常使用与取消继续按既有规则生效，最新查询反映新事实，旧结果保留当时内容。
// 从 c-four 再使用 1 件后取消 c-five：取消只释放 c-five 的未用 3 件，已使用的
// 2 件实物不补回；最新查询为实物 9 件、有效占用 3 件、可承诺 6 件。
func TestPartStatusSnapshotSurvivesLaterUseAndCancel(t *testing.T) {
	s := partSnapshotStore(t)

	saved, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("saved part status: %v", err)
	}
	assertInitialPartSnapshot(t, saved)

	// 之后的正常操作：从四件那笔再使用一件，然后取消五件那笔。
	// 时刻均早于承诺到期时刻，到期确认规则不参与本局面。
	later := nowOK.Add(time.Hour)
	if _, err := s.Use("u-four-1", "c-four", 1, later); err != nil {
		t.Fatalf("use 1 from c-four: %v", err)
	}
	if _, err := s.Cancel("c-five", later); err != nil {
		t.Fatalf("cancel c-five: %v", err)
	}

	latest, err := s.PartStatus("part1", later)
	if err != nil {
		t.Fatalf("latest part status: %v", err)
	}
	// 实物 10-1=9；有效占用只剩 c-four 未用 3；可承诺 9-3=6。
	if latest.PhysicalRemaining != 9 || latest.ActiveOccupied != 3 || latest.Committable != 6 {
		t.Fatalf("latest stock = %+v, want phys=9 occupied=3 committable=6", latest)
	}
	if len(latest.Details) != 2 {
		t.Fatalf("latest details len = %d, want 2", len(latest.Details))
	}
	// 取消的明细仍保留原定 5、已用 2、未用 3，状态 canceled。
	d5 := mustDetailByID(t, latest.Details, "c-five")
	if d5.RequestID != "r-five" || d5.OriginalQuantity != 5 ||
		d5.UsedQuantity != 2 || d5.RemainingQuantity != 3 || d5.Status != CommitmentCanceled {
		t.Fatalf("latest c-five detail = %+v, want r-five 5/2/3 canceled", d5)
	}
	// 另一笔显示原定 4、已用 1、未用 3，状态仍 active。
	d4 := mustDetailByID(t, latest.Details, "c-four")
	if d4.RequestID != "r-four" || d4.OriginalQuantity != 4 ||
		d4.UsedQuantity != 1 || d4.RemainingQuantity != 3 || d4.Status != CommitmentActive {
		t.Fatalf("latest c-four detail = %+v, want r-four 4/1/3 active", d4)
	}

	// 先前保存的结果仍是十件、七件、三件，以及当时两笔承诺的数量与 active
	// 状态，不随仓库变化被改写。
	assertInitialPartSnapshot(t, saved)

	// 仓库记录与数量核算规则一致：取消只释放未用占用，已用实物不补回。
	c5, _ := s.Commitment("c-five")
	if !c5.Canceled || c5.Quantity != 5 || c5.Used != 2 {
		t.Fatalf("c-five record = %+v, want canceled 5/2", c5)
	}
	c4, _ := s.Commitment("c-four")
	if c4.Canceled || c4.Quantity != 4 || c4.Used != 1 {
		t.Fatalf("c-four record = %+v, want active 4/1", c4)
	}
	if p, _ := s.Part("part1"); p.Stock != 9 {
		t.Fatalf("physical stock = %d, want 9", p.Stock)
	}
}

// TestPartStatusEmptySnapshotStaysEmptyAfterReserve 没有承诺时，已登记备件的
// 明细为空，实物剩余与可承诺数量相同；后来正常预留成功，新查询出现承诺明细，
// 先前的空结果仍保持为空。
func TestPartStatusEmptySnapshotStaysEmptyAfterReserve(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "NOISE"); err != nil {
		t.Fatalf("submit r1: %v", err)
	}

	empty, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("empty part status: %v", err)
	}
	if empty.PhysicalRemaining != 12 || empty.ActiveOccupied != 0 || empty.Committable != 12 {
		t.Fatalf("empty stock = %+v, want phys=12 occupied=0 committable=12", empty)
	}
	if len(empty.Details) != 0 {
		t.Fatalf("empty details = %+v, want none", empty.Details)
	}

	// 调用方往自己手里的空结果补入一条明细，不能变成仓库里的预留。
	empty.Details = append(empty.Details, CommitmentDetail{CommitmentID: "c-display-only"})

	// 后来正常预留成功（时刻早于到期时刻）。
	if _, err := s.Reserve("c1", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}

	// 新查询出现一条真实承诺明细，账目随之变化。
	latest, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("latest part status: %v", err)
	}
	if latest.PhysicalRemaining != 12 || latest.ActiveOccupied != 3 || latest.Committable != 9 {
		t.Fatalf("latest stock = %+v, want phys=12 occupied=3 committable=9", latest)
	}
	if len(latest.Details) != 1 {
		t.Fatalf("latest details len = %d, want 1", len(latest.Details))
	}
	d := latest.Details[0]
	if d.CommitmentID != "c1" || d.RequestID != "r1" ||
		d.OriginalQuantity != 3 || d.UsedQuantity != 0 || d.RemainingQuantity != 3 ||
		d.Status != CommitmentActive {
		t.Fatalf("latest detail = %+v, want c1/r1 3/0/3 active", d)
	}

	// 先前保存的空结果仍保持为空（只有调用方自己补入的展示条目），不含 c1。
	if _, ok := detailByID(empty, "c1"); ok {
		t.Fatalf("saved empty snapshot gained real commitment: %+v", empty.Details)
	}
	if len(empty.Details) != 1 || empty.Details[0].CommitmentID != "c-display-only" {
		t.Fatalf("saved snapshot changed: %+v", empty.Details)
	}
	if empty.PhysicalRemaining != 12 || empty.Committable != 12 {
		t.Fatalf("saved empty snapshot account changed: %+v", empty)
	}
}
