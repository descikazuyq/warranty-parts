package warranty

import (
	"errors"
	"testing"
)

// 多笔承诺共同占用同一种备件时，取消其中一笔只释放这笔承诺的未用数量：
// 已用部分不回补实物库存，其他承诺（同请求或不同请求）的占用不受影响；
// 释放出的可承诺数量随后可被新请求预留，且取消结果幂等、取消后不可再使用。
func newSharedPartStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
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

func TestCancelOneOfSharedCommitments(t *testing.T) {
	s := newSharedPartStore(t)

	// 请求甲两笔承诺（5 件、2 件），请求乙一笔承诺（4 件），同一备件初始库存 12。
	if _, err := s.Reserve("cA1", "rA", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cA1: %v", err)
	}
	if _, err := s.Reserve("cA2", "rA", "part1", 2, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cA2: %v", err)
	}
	if _, err := s.Reserve("cB1", "rB", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cB1: %v", err)
	}

	// 从 cA1 使用 2 件：实物 12-2=10，有效占用 3+2+4=9，可承诺 1。
	if _, err := s.Use("u1", "cA1", 2, nowOK); err != nil {
		t.Fatalf("use 2 from cA1: %v", err)
	}
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 9 || st.Committable != 1 {
		t.Fatalf("after use: phys=%d occupied=%d committable=%d, want 10/9/1",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 取消 cA1：只释放未用的 3 件，已用的 2 件不回补实物库存，
	// 请求甲的另一笔承诺和请求乙的承诺不受影响。
	cancelled, err := s.Cancel("cA1", nowOK)
	if err != nil {
		t.Fatalf("cancel cA1: %v", err)
	}
	if !cancelled.Canceled || cancelled.Quantity != 5 || cancelled.Used != 2 || cancelled.Unused() != 3 {
		t.Fatalf("cancel result: %+v, want quantity=5 used=2 unused=3 canceled", cancelled)
	}
	st, err = s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status after cancel: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 6 || st.Committable != 4 {
		t.Fatalf("after cancel: phys=%d occupied=%d committable=%d, want 10/6/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 取消记录仍保留在按备件的明细中：原定 5、已用 2、未用 3、状态 canceled，
	// 未用 3 件可追查但不再计入有效占用；其余承诺的数量、所属请求和到期时刻保持原值。
	if len(st.Details) != 3 {
		t.Fatalf("details = %d, want 3 (canceled record still traceable)", len(st.Details))
	}
	byID := map[string]CommitmentDetail{}
	for _, d := range st.Details {
		byID[d.CommitmentID] = d
	}
	dA1 := byID["cA1"]
	if dA1.Status != CommitmentCanceled || dA1.RequestID != "rA" || dA1.PartID != "part1" ||
		dA1.OriginalQuantity != 5 || dA1.UsedQuantity != 2 || dA1.RemainingQuantity != 3 ||
		!dA1.Expiry.Equal(expiryOK) {
		t.Fatalf("canceled detail cA1: %+v", dA1)
	}
	dA2 := byID["cA2"]
	if dA2.Status != CommitmentActive || dA2.RequestID != "rA" ||
		dA2.OriginalQuantity != 2 || dA2.UsedQuantity != 0 || dA2.RemainingQuantity != 2 ||
		!dA2.Expiry.Equal(expiryOK) {
		t.Fatalf("detail cA2 changed by cancel: %+v", dA2)
	}
	dB1 := byID["cB1"]
	if dB1.Status != CommitmentActive || dB1.RequestID != "rB" ||
		dB1.OriginalQuantity != 4 || dB1.UsedQuantity != 0 || dB1.RemainingQuantity != 4 ||
		!dB1.Expiry.Equal(expiryOK) {
		t.Fatalf("detail cB1 changed by cancel: %+v", dB1)
	}

	// 按请求查看：请求甲仍看到自己的两笔承诺（含已取消的 cA1），请求乙只看到自己的。
	viewA, err := s.RequestView("rA", nowOK)
	if err != nil {
		t.Fatalf("request view rA: %v", err)
	}
	if len(viewA.Commitments) != 2 ||
		viewA.Commitments[0].CommitmentID != "cA1" || viewA.Commitments[1].CommitmentID != "cA2" {
		t.Fatalf("rA commitments: %+v", viewA.Commitments)
	}
	if viewA.Commitments[0].Status != CommitmentCanceled || viewA.Commitments[1].Status != CommitmentActive {
		t.Fatalf("rA statuses: %q, %q", viewA.Commitments[0].Status, viewA.Commitments[1].Status)
	}
	viewB, err := s.RequestView("rB", nowOK)
	if err != nil {
		t.Fatalf("request view rB: %v", err)
	}
	if len(viewB.Commitments) != 1 || viewB.Commitments[0].CommitmentID != "cB1" ||
		viewB.Commitments[0].Status != CommitmentActive {
		t.Fatalf("rB commitments: %+v", viewB.Commitments)
	}

	// 取消后再次查询承诺本体：已用 2 件保留，未用 3 件不计入占用但仍可追查。
	c, err := s.Commitment("cA1")
	if err != nil {
		t.Fatalf("commitment cA1: %v", err)
	}
	if !c.Canceled || c.Used != 2 || c.Unused() != 3 {
		t.Fatalf("commitment cA1 after cancel: %+v", c)
	}

	// 新请求预留释放出的全部 4 件可承诺数量：成功；实物仍 10，占用变 10，可承诺 0。
	if _, err := s.Reserve("cC1", "rC", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cC1 with released stock: %v", err)
	}
	st, err = s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status after re-reserve: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 10 || st.Committable != 0 {
		t.Fatalf("after re-reserve: phys=%d occupied=%d committable=%d, want 10/10/0",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 再次取消同一笔承诺：正常返回已取消结果，不重复释放，任何有效承诺不变。
	again, err := s.Cancel("cA1", nowOK)
	if err != nil {
		t.Fatalf("repeat cancel cA1: %v", err)
	}
	if !again.Canceled || again.Quantity != 5 || again.Used != 2 || again.Unused() != 3 {
		t.Fatalf("repeat cancel result: %+v", again)
	}
	st, err = s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status after repeat cancel: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 10 || st.Committable != 0 {
		t.Fatalf("repeat cancel released again: phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 对已取消的承诺提交新使用：ErrCommitmentClosed；实物与各笔已用数量保持原值，
	// 其他请求已取得的占用不受影响。
	if _, err := s.Use("u2", "cA1", 1, nowOK); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use on canceled commitment: got %v, want ErrCommitmentClosed", err)
	}
	st, err = s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status after rejected use: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 10 || st.Committable != 0 {
		t.Fatalf("rejected use changed stock: phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	for id, wantUsed := range map[string]int{"cA1": 2, "cA2": 0, "cB1": 0, "cC1": 0} {
		c, err := s.Commitment(id)
		if err != nil {
			t.Fatalf("commitment %s: %v", id, err)
		}
		if c.Used != wantUsed {
			t.Fatalf("commitment %s used = %d, want %d", id, c.Used, wantUsed)
		}
	}
}
