package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障取消功能在调用方传入较晚当前时刻时的作用范围：取消只处理
// 指定的那一笔承诺，即使本次时刻已经晚于其他承诺的到期时刻，也不能顺带确认
// 那些承诺到期或释放它们的占用。到期确认仍只由涉及相应承诺的库存查询、请求
// 查询、新预留核算或新使用判断完成；取消失败（空编号、未知编号）同样不借
// 本次时刻触碰任何已知承诺，也不会为不存在的编号生成承诺。

// TestCancelWithLateNowDoesNotExpireOthers 用晚于两笔承诺到期时刻的当前时刻
// 取消甲：取消正常返回并只释放甲的未用 3 件；乙不被顺带确认到期，之后以早于
// 乙到期时刻的当前时刻查看，乙仍占用 4 件，且仍能合法领取。
func TestCancelWithLateNowDoesNotExpireOthers(t *testing.T) {
	s := cancelSharedStore(t)

	expA := t0.Add(40 * day)
	expB := t0.Add(30 * day)
	if _, err := s.Reserve("cA", "rA", "part1", 5, expA, nowOK); err != nil {
		t.Fatalf("reserve cA: %v", err)
	}
	if _, err := s.Reserve("cB", "rB", "part1", 4, expB, nowOK); err != nil {
		t.Fatalf("reserve cB: %v", err)
	}
	// 甲已领取 2 件：实物 10，有效占用 7（甲未用 3 + 乙 4），可承诺 3。
	if _, err := s.Use("uA-used", "cA", 2, nowOK); err != nil {
		t.Fatalf("use 2 from cA: %v", err)
	}
	assertPartAccount(t, s, 10, 7, 3)

	// 用晚于两笔到期时刻的当前时刻取消甲：正常返回已取消记录，原数量 5、
	// 已用 2、未用 3 与原到期时刻都保留。
	late := t0.Add(50 * day)
	canceled, err := s.Cancel("cA", late)
	if err != nil {
		t.Fatalf("cancel cA with late now: %v", err)
	}
	if !canceled.Canceled || canceled.Quantity != 5 || canceled.Used != 2 ||
		canceled.Unused() != 3 || !canceled.Expiry.Equal(expA) {
		t.Fatalf("cancel result: %+v, want qty=5 used=2 unused=3 canceled=true expiry=%v",
			canceled, expA)
	}

	// 取消只释放甲的未用 3 件，已领取的 2 件不回到实物库存；取消的较晚时刻
	// 不能顺带确认乙到期，乙仍占用 4 件：实物 10，有效占用 4，可承诺 6。
	assertPartAccount(t, s, 10, 4, 6)

	// 按编号取回乙：仍未取消、未确认到期，数量、归属和到期时刻不变。
	cB, err := s.Commitment("cB")
	if err != nil {
		t.Fatalf("commitment cB: %v", err)
	}
	if cB.Canceled || cB.Expired || cB.Quantity != 4 || cB.Used != 0 ||
		cB.RequestID != "rB" || cB.PartID != "part1" || !cB.Expiry.Equal(expB) {
		t.Fatalf("cB disturbed by cancel of cA: %+v", cB)
	}

	// 以早于乙到期时刻的当前时刻查看备件：乙仍显示 active 并占用 4 件，
	// 甲仍显示 canceled。
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 4 || st.Committable != 6 {
		t.Fatalf("stock after late-now cancel: phys=%d occupied=%d committable=%d, want 10/4/6",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if d := mustDetailByID(t, st.Details, "cA"); d.Status != CommitmentCanceled {
		t.Fatalf("cA status = %q, want canceled", d.Status)
	}
	if d := mustDetailByID(t, st.Details, "cB"); d.Status != CommitmentActive ||
		d.RemainingQuantity != 4 || !d.Expiry.Equal(expB) {
		t.Fatalf("cB detail after late-now cancel of cA: %+v", d)
	}

	// 以同一较早时刻、用新的使用编号从乙合法领取 1 件：成功。账目变为
	// 实物 9、有效占用 3、可承诺 6，乙的已用数量变为 1。
	if _, err := s.Use("uB-1", "cB", 1, nowOK); err != nil {
		t.Fatalf("use 1 from cB at earlier now: %v", err)
	}
	assertPartAccount(t, s, 9, 3, 6)
	cB, err = s.Commitment("cB")
	if err != nil {
		t.Fatalf("commitment cB after use: %v", err)
	}
	if cB.Used != 1 || cB.Canceled || cB.Expired {
		t.Fatalf("cB after legit use: %+v, want used=1 active", cB)
	}

	// 甲的数量与取消结果保持原样。
	cA, err := s.Commitment("cA")
	if err != nil {
		t.Fatalf("commitment cA: %v", err)
	}
	if !cA.Canceled || cA.Quantity != 5 || cA.Used != 2 || !cA.Expiry.Equal(expA) {
		t.Fatalf("cA changed after cB usage: %+v", cA)
	}
}

// TestCancelFailuresWithLateNowKeepScope 取消失败时保持相同的作用范围：
// 空编号返回 ErrInvalidParam，不存在的非空编号返回 ErrNotFound；即使传入
// 晚于乙到期时刻的当前时刻，失败调用也不改变任何已知承诺、不扣减实物，
// 不存在的编号不会因此生成承诺。
func TestCancelFailuresWithLateNowKeepScope(t *testing.T) {
	s := cancelSharedStore(t)

	expB := t0.Add(30 * day)
	if _, err := s.Reserve("cB", "rB", "part1", 4, expB, nowOK); err != nil {
		t.Fatalf("reserve cB: %v", err)
	}
	// 取消前：实物 12，有效占用 4，可承诺 8。
	assertPartAccount(t, s, 12, 4, 8)

	late := t0.Add(50 * day)
	if _, err := s.Cancel("", late); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("cancel empty id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Cancel("cGhost", late); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown id: got %v, want ErrNotFound", err)
	}

	// 失败的取消不确认乙到期、不释放占用、不扣减实物：以早于乙到期时刻的
	// 当前时刻查看，账目不变，乙仍有效。
	assertPartAccount(t, s, 12, 4, 8)
	cB, err := s.Commitment("cB")
	if err != nil {
		t.Fatalf("commitment cB: %v", err)
	}
	if cB.Canceled || cB.Expired || cB.Quantity != 4 || cB.Used != 0 || !cB.Expiry.Equal(expB) {
		t.Fatalf("cB disturbed by failed cancels: %+v", cB)
	}

	// 不存在的编号不会因取消调用而生成承诺。
	if _, err := s.Commitment("cGhost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost commitment after failed cancel: got %v, want ErrNotFound", err)
	}
}

// TestCancelDoesNotClearConfirmedExpiry 乙在取消甲之前已被正常操作确认到期：
// 取消甲不清除乙的到期结果，之后用新的使用编号在较早时刻领取乙仍返回
// ErrCommitmentClosed。
func TestCancelDoesNotClearConfirmedExpiry(t *testing.T) {
	s := cancelSharedStore(t)

	expA := t0.Add(40 * day)
	expB := t0.Add(30 * day)
	if _, err := s.Reserve("cA", "rA", "part1", 5, expA, nowOK); err != nil {
		t.Fatalf("reserve cA: %v", err)
	}
	if _, err := s.Reserve("cB", "rB", "part1", 4, expB, nowOK); err != nil {
		t.Fatalf("reserve cB: %v", err)
	}
	if _, err := s.Use("uA-used", "cA", 2, nowOK); err != nil {
		t.Fatalf("use 2 from cA: %v", err)
	}

	// 正常操作确认乙到期：以介于两笔到期时刻之间的当前时刻查询备件，
	// 乙（30 天到期）被确认到期，甲（40 天到期）尚未到期仍有效。
	mid := t0.Add(35 * day)
	st, err := s.PartStatus("part1", mid)
	if err != nil {
		t.Fatalf("part status at mid: %v", err)
	}
	if d := mustDetailByID(t, st.Details, "cB"); d.Status != CommitmentExpired {
		t.Fatalf("cB status at mid = %q, want expired", d.Status)
	}
	if d := mustDetailByID(t, st.Details, "cA"); d.Status != CommitmentActive {
		t.Fatalf("cA status at mid = %q, want active", d.Status)
	}
	// 乙确认到期后释放未用 4 件：实物 10，有效占用 3（甲未用），可承诺 7。
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 3 || st.Committable != 7 {
		t.Fatalf("stock after cB expiry confirmed: phys=%d occupied=%d committable=%d, want 10/3/7",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 用晚于两笔到期时刻的当前时刻取消甲：正常返回，甲的未用 3 件释放。
	canceled, err := s.Cancel("cA", t0.Add(50*day))
	if err != nil {
		t.Fatalf("cancel cA: %v", err)
	}
	if !canceled.Canceled || canceled.Quantity != 5 || canceled.Used != 2 || canceled.Unused() != 3 {
		t.Fatalf("cancel result: %+v", canceled)
	}

	// 取消甲不清除乙已确认的到期结果。
	cB, err := s.Commitment("cB")
	if err != nil {
		t.Fatalf("commitment cB: %v", err)
	}
	if !cB.Expired || cB.Canceled || cB.Quantity != 4 || cB.Used != 0 {
		t.Fatalf("cB expiry cleared by cancel of cA: %+v", cB)
	}

	// 之后用新的使用编号、以早于乙到期时刻的当前时刻领取乙：到期确认不可
	// 逆，仍返回 ErrCommitmentClosed，实物与已用数量不变。
	if _, err := s.Use("uB-after-expiry", "cB", 1, nowOK); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use expired cB at earlier now: got %v, want ErrCommitmentClosed", err)
	}
	assertPartAccount(t, s, 10, 0, 10)
	cB, err = s.Commitment("cB")
	if err != nil {
		t.Fatalf("commitment cB after rejected use: %v", err)
	}
	if cB.Used != 0 || !cB.Expired {
		t.Fatalf("cB after rejected use: %+v", cB)
	}
}
