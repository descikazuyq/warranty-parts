package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障取消功能在同一备件被多笔承诺共同占用时的行为：取消一笔只
// 释放这笔承诺自身的未用数量，已使用的部分不回补实物库存，其他请求或同一
// 请求的其他有效承诺一律不受影响；释放出的可承诺数量随后可以被新的合格请求
// 全部预留，且对已取消承诺的重复取消与新使用都不能再次改变库存。

// cancelSharedStore 构造一个保修 60 天的产品、初始库存 12 件的备件和三个
// 保修资格请求（请求甲 rA、请求乙 rB、取消后新到的请求 rC）。
func cancelSharedStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 60, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, id := range []string{"rA", "rB", "rC"} {
		if err := s.SubmitRequest(id, "p1", "FAULTY"); err != nil {
			t.Fatalf("submit request %s: %v", id, err)
		}
	}
	return s
}

// mustDetailByID 在承诺明细列表中按编号查找，找不到时失败。
func mustDetailByID(t *testing.T, details []CommitmentDetail, commitID string) CommitmentDetail {
	t.Helper()
	for _, d := range details {
		if d.CommitmentID == commitID {
			return d
		}
	}
	t.Fatalf("commitment %q not found in details %+v", commitID, details)
	return CommitmentDetail{}
}

// assertPartAccount 断言备件账目三项数值与调用方可见的库存规则一致：
// 实物剩余只扣成功使用量，有效占用只计有效承诺的未用数量。
func assertPartAccount(t *testing.T, s *Store, physical, occupied, committable int) {
	t.Helper()
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock account: phys=%d occupied=%d committable=%d, want phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// TestCancelReleasesOnlyThisCommitmentUnused 以同一备件初始库存 12 件为例：
// 请求甲两笔承诺分别预留 5 件和 2 件，请求乙一笔承诺预留 4 件。从甲的 5 件
// 承诺使用 2 件后取消该承诺，只能释放其未用的 3 件；取消记录仍可追查但不再
// 计入有效占用，甲的另一笔承诺与乙的承诺数量、归属和到期时刻保持原值。
func TestCancelReleasesOnlyThisCommitmentUnused(t *testing.T) {
	s := cancelSharedStore(t)

	// 三笔承诺各给不同的到期时刻（均晚于全部操作时刻），以便回归保障取消
	// 不会篡改其他承诺的到期时刻。
	expA5 := t0.Add(40 * day)
	expA2 := t0.Add(50 * day)
	expB4 := t0.Add(30 * day)

	if _, err := s.Reserve("cA5", "rA", "part1", 5, expA5, nowOK); err != nil {
		t.Fatalf("reserve cA5: %v", err)
	}
	if _, err := s.Reserve("cA2", "rA", "part1", 2, expA2, nowOK); err != nil {
		t.Fatalf("reserve cA2: %v", err)
	}
	if _, err := s.Reserve("cB4", "rB", "part1", 4, expB4, nowOK); err != nil {
		t.Fatalf("reserve cB4: %v", err)
	}

	// 从请求甲的 5 件承诺中使用 2 件：实物 10，有效占用 9（3+2+4），可承诺 1。
	if _, err := s.Use("uA-used", "cA5", 2, nowOK); err != nil {
		t.Fatalf("use 2 from cA5: %v", err)
	}
	assertPartAccount(t, s, 10, 9, 1)

	// 取消这笔 5 件承诺：返回结果仍保留原定 5、已用 2、未用 3，并显示已取消。
	canceled, err := s.Cancel("cA5", nowOK)
	if err != nil {
		t.Fatalf("cancel cA5: %v", err)
	}
	if !canceled.Canceled || canceled.Quantity != 5 || canceled.Used != 2 || canceled.Unused() != 3 {
		t.Fatalf("cancel result: %+v, want qty=5 used=2 unused=3 canceled=true", canceled)
	}

	// 实物仍是 10（已用 2 件不回补），有效占用降为 6（2+4），可承诺变为 4。
	assertPartAccount(t, s, 10, 6, 4)

	// 直接取回的承诺同样保留原定/已用/未用事实并显示已取消。
	got, err := s.Commitment("cA5")
	if err != nil {
		t.Fatalf("commitment cA5: %v", err)
	}
	if !got.Canceled || got.Quantity != 5 || got.Used != 2 || got.Unused() != 3 {
		t.Fatalf("stored canceled commitment: %+v", got)
	}

	// 按请求查看：请求甲仍能看到自己的两笔承诺。
	viewA, err := s.RequestView("rA", nowOK)
	if err != nil {
		t.Fatalf("request view rA: %v", err)
	}
	if len(viewA.Commitments) != 2 {
		t.Fatalf("rA commitments = %d, want 2 (canceled record still visible)", len(viewA.Commitments))
	}
	dA5 := mustDetailByID(t, viewA.Commitments, "cA5")
	if dA5.Status != CommitmentCanceled || dA5.OriginalQuantity != 5 ||
		dA5.UsedQuantity != 2 || dA5.RemainingQuantity != 3 ||
		dA5.RequestID != "rA" || dA5.PartID != "part1" || !dA5.Expiry.Equal(expA5) {
		t.Fatalf("rA view of cA5: %+v", dA5)
	}
	// 甲的另一笔承诺继续有效，数量、归属与到期时刻保持原值。
	dA2 := mustDetailByID(t, viewA.Commitments, "cA2")
	if dA2.Status != CommitmentActive || dA2.OriginalQuantity != 2 ||
		dA2.UsedQuantity != 0 || dA2.RemainingQuantity != 2 ||
		dA2.RequestID != "rA" || !dA2.Expiry.Equal(expA2) {
		t.Fatalf("rA view of cA2 changed after cancel: %+v", dA2)
	}

	// 请求乙只看到自己的承诺：4 件有效，原值不变。
	viewB, err := s.RequestView("rB", nowOK)
	if err != nil {
		t.Fatalf("request view rB: %v", err)
	}
	if len(viewB.Commitments) != 1 {
		t.Fatalf("rB commitments = %d, want 1", len(viewB.Commitments))
	}
	dB4 := mustDetailByID(t, viewB.Commitments, "cB4")
	if dB4.Status != CommitmentActive || dB4.OriginalQuantity != 4 ||
		dB4.UsedQuantity != 0 || dB4.RemainingQuantity != 4 ||
		dB4.RequestID != "rB" || dB4.PartID != "part1" || !dB4.Expiry.Equal(expB4) {
		t.Fatalf("rB view of cB4 changed after other request's cancel: %+v", dB4)
	}

	// 按备件查看：取消记录仍保留在明细中，各笔状态与库存总数一致。
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if len(st.Details) != 3 {
		t.Fatalf("part details = %d, want 3 (canceled record retained)", len(st.Details))
	}
	pdA5 := mustDetailByID(t, st.Details, "cA5")
	if pdA5.Status != CommitmentCanceled || pdA5.OriginalQuantity != 5 ||
		pdA5.UsedQuantity != 2 || pdA5.RemainingQuantity != 3 {
		t.Fatalf("part detail cA5: %+v", pdA5)
	}
	if mustDetailByID(t, st.Details, "cA2").Status != CommitmentActive {
		t.Fatal("part detail cA2 not active")
	}
	if mustDetailByID(t, st.Details, "cB4").Status != CommitmentActive {
		t.Fatal("part detail cB4 not active")
	}
	// 明细中有效承诺的未用数量之和必须等于有效占用，已取消明细不参与。
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

// TestCanceledUnusedCanBeReservedThenCancelStaysInert 取消释放出的 4 件可
// 承诺数量随后被一个有保修资格的新请求全部预留；此后再次取消原承诺不能二次
// 释放，原承诺上的新使用返回 ErrCommitmentClosed，其他请求已取得的占用不受
// 影响。
func TestCanceledUnusedCanBeReservedThenCancelStaysInert(t *testing.T) {
	s := cancelSharedStore(t)

	if _, err := s.Reserve("cA5", "rA", "part1", 5, t0.Add(40*day), nowOK); err != nil {
		t.Fatalf("reserve cA5: %v", err)
	}
	if _, err := s.Reserve("cA2", "rA", "part1", 2, t0.Add(50*day), nowOK); err != nil {
		t.Fatalf("reserve cA2: %v", err)
	}
	if _, err := s.Reserve("cB4", "rB", "part1", 4, t0.Add(30*day), nowOK); err != nil {
		t.Fatalf("reserve cB4: %v", err)
	}
	if _, err := s.Use("uA-used", "cA5", 2, nowOK); err != nil {
		t.Fatalf("use 2 from cA5: %v", err)
	}
	if _, err := s.Cancel("cA5", nowOK); err != nil {
		t.Fatalf("cancel cA5: %v", err)
	}
	// 取消后：实物 10，有效占用 6，可承诺 4。
	assertPartAccount(t, s, 10, 6, 4)

	// 新请求在晚于取消的操作时刻预留释放后的全部 4 件：成功。
	later := t0.Add(11 * day)
	cC4, err := s.Reserve("cC4", "rC", "part1", 4, t0.Add(35*day), later)
	if err != nil {
		t.Fatalf("reserve freed 4 for rC: %v", err)
	}
	if cC4.Quantity != 4 || cC4.Used != 0 || cC4.RequestID != "rC" || cC4.Canceled || cC4.Expired {
		t.Fatalf("new commitment for rC: %+v", cC4)
	}
	// 实物仍为 10，有效占用变为 10（2+4+4），可承诺为 0。
	st, err := s.PartStatus("part1", later)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 10 || st.Committable != 0 {
		t.Fatalf("stock after freed quantity re-reserved: phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	// 可承诺确认为 0：再要 1 件的新合格请求无法预留。
	if _, err := s.Reserve("cC-extra", "rC", "part1", 1, t0.Add(35*day), later); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve beyond zero committable: got %v, want ErrInsufficientStock", err)
	}

	// 再次取消原来的 5 件承诺：正常返回已取消结果，但不能再次释放 3 件。
	again, err := s.Cancel("cA5", later)
	if err != nil {
		t.Fatalf("repeat cancel cA5: %v", err)
	}
	if !again.Canceled || again.Quantity != 5 || again.Used != 2 || again.Unused() != 3 {
		t.Fatalf("repeat cancel result: %+v", again)
	}
	st2, err := s.PartStatus("part1", later)
	if err != nil {
		t.Fatalf("part status after repeat cancel: %v", err)
	}
	if st2.PhysicalRemaining != 10 || st2.ActiveOccupied != 10 || st2.Committable != 0 {
		t.Fatalf("repeat cancel changed stock: phys=%d occupied=%d committable=%d",
			st2.PhysicalRemaining, st2.ActiveOccupied, st2.Committable)
	}

	// 对原承诺用新的使用编号提交 1 件使用：返回 ErrCommitmentClosed。
	if _, err := s.Use("uA-after-cancel", "cA5", 1, later); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new usage on canceled commitment: got %v, want ErrCommitmentClosed", err)
	}

	// 实物库存与各笔已用数量保持原值。
	st3, err := s.PartStatus("part1", later)
	if err != nil {
		t.Fatalf("part status after rejected usage: %v", err)
	}
	if st3.PhysicalRemaining != 10 || st3.ActiveOccupied != 10 || st3.Committable != 0 {
		t.Fatalf("rejected usage changed stock: phys=%d occupied=%d committable=%d",
			st3.PhysicalRemaining, st3.ActiveOccupied, st3.Committable)
	}
	cA5, _ := s.Commitment("cA5")
	if cA5.Used != 2 || !cA5.Canceled {
		t.Fatalf("cA5 after rejected usage: %+v", cA5)
	}
	for _, tc := range []struct {
		id   string
		want Commitment
	}{
		{"cA2", Commitment{ID: "cA2", RequestID: "rA", PartID: "part1", Quantity: 2, Used: 0}},
		{"cB4", Commitment{ID: "cB4", RequestID: "rB", PartID: "part1", Quantity: 4, Used: 0}},
		{"cC4", Commitment{ID: "cC4", RequestID: "rC", PartID: "part1", Quantity: 4, Used: 0}},
	} {
		c, err := s.Commitment(tc.id)
		if err != nil {
			t.Fatalf("commitment %s: %v", tc.id, err)
		}
		if c.Canceled || c.Expired || c.Quantity != tc.want.Quantity || c.Used != 0 || c.RequestID != tc.want.RequestID {
			t.Fatalf("active commitment %s disturbed: %+v", tc.id, c)
		}
	}
}
