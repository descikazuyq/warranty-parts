package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障直接资格查询 Evaluate 与请求明细查询 RequestView 的区别：
// Evaluate 只返回当次时刻的资格依据与拒绝原因，即使传入的时刻已经超过关联
// 承诺的到期时刻，也不确认承诺到期、不释放预留占用、不改变已用数量；确认
// 到期仍是 RequestView（及库存查询、新预留、新使用）的职责。空编号与未知
// 请求的失败查询同样不改变任何承诺与库存。

// TestEvaluateDoesNotConfirmCommitmentExpiry 对应用户给出的主例：保修三十天、
// 初始库存十件，第十天预留六件并领取两件，承诺第二十天到期。尚未通过其他
// 操作确认到期时，第三十一天直接查询资格返回不合格且唯一原因是过保，资格
// 依据与登记资料一致；这次查询不确认承诺到期。随后以第十五天继续操作，库存
// 账目与领取行为不受那次较晚时刻查询影响；直到以第三十一天查看请求明细才
// 确认到期，且确认不可逆。
func TestEvaluateDoesNotConfirmCommitmentExpiry(t *testing.T) {
	s := newStore(t)
	day10 := t0.Add(10 * day)
	day15 := t0.Add(15 * day)
	day20 := t0.Add(20 * day)
	day31 := t0.Add(31 * day)

	// 第十天预留六件、当天领取两件，承诺第二十天到期。
	if _, err := s.Reserve("c1", "r1", "part1", 6, day20, day10); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, day10); err != nil {
		t.Fatalf("use u1: %v", err)
	}

	// 尚未确认到期时，第三十一天直接查询资格：不合格，拒绝原因只有过保，
	// 购买时刻、保修天数与保修截止时刻仍与登记资料一致。
	elig, err := s.Evaluate("r1", day31)
	if err != nil {
		t.Fatalf("evaluate at day 31: %v", err)
	}
	if elig.Eligible {
		t.Fatalf("eligible at day 31: %+v", elig)
	}
	if len(elig.Reasons) != 1 || elig.Reasons[0] != ReasonWarrantyExpired {
		t.Fatalf("reasons = %v, want only %q", elig.Reasons, ReasonWarrantyExpired)
	}
	if !elig.PurchaseTime.Equal(t0) || elig.WarrantyDays != 30 ||
		!elig.WarrantyExpiry.Equal(t0.Add(30*day)) {
		t.Fatalf("basis changed: purchase=%v days=%d expiry=%v, want %v/30/%v",
			elig.PurchaseTime, elig.WarrantyDays, elig.WarrantyExpiry, t0, t0.Add(30*day))
	}
	if elig.Excluded || elig.RequestID != "r1" || elig.ProductID != "p1" || elig.FaultCode != "FAULTY" {
		t.Fatalf("eligibility identity: %+v", elig)
	}

	// 这次查询不确认承诺到期：原数量六件、已用两件，未取消、未确认到期。
	c, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("get c1: %v", err)
	}
	if c.Expired || c.Canceled || c.Quantity != 6 || c.Used != 2 {
		t.Fatalf("c1 changed by evaluate: %+v, want unconfirmed 6/2", c)
	}

	// 以第十五天继续操作：实物八件、有效占用四件、可承诺四件；资格查询
	// 曾使用较晚时刻不能替代本次时刻，新领取一件仍成功，账目变为 7/3/4。
	st, err := s.PartStatus("part1", day15)
	if err != nil {
		t.Fatalf("part status at day 15: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("stock at day 15: phys=%d occupied=%d committable=%d, want 8/4/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if _, err := s.Use("u2", "c1", 1, day15); err != nil {
		t.Fatalf("use u2 at day 15 closed by earlier evaluate: %v", err)
	}
	st, err = s.PartStatus("part1", day15)
	if err != nil {
		t.Fatalf("part status after u2: %v", err)
	}
	if st.PhysicalRemaining != 7 || st.ActiveOccupied != 3 || st.Committable != 4 {
		t.Fatalf("stock after u2: phys=%d occupied=%d committable=%d, want 7/3/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 第三十一天查看请求明细：确认原承诺到期，保留原数量六件、已用三件，
	// 只释放剩余三件占用，实物七件不回补。
	v, err := s.RequestView("r1", day31)
	if err != nil {
		t.Fatalf("view r1 at day 31: %v", err)
	}
	if len(v.Commitments) != 1 {
		t.Fatalf("r1 view contains %d commitments %+v, want only c1", len(v.Commitments), v.Commitments)
	}
	d := v.Commitments[0]
	if d.CommitmentID != "c1" || d.Status != CommitmentExpired ||
		d.OriginalQuantity != 6 || d.UsedQuantity != 3 || d.RemainingQuantity != 3 {
		t.Fatalf("c1 detail at day 31: %+v, want expired 6/3/3", d)
	}
	st, err = s.PartStatus("part1", day15)
	if err != nil {
		t.Fatalf("part status after view: %v", err)
	}
	if st.PhysicalRemaining != 7 || st.ActiveOccupied != 0 || st.Committable != 7 {
		t.Fatalf("stock after expiry confirmation: phys=%d occupied=%d committable=%d, want 7/0/7",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 到期一经确认不可逆：之后即使以较早时刻查看也保持 expired，不能再领取。
	v, err = s.RequestView("r1", day15)
	if err != nil {
		t.Fatalf("view r1 at day 15: %v", err)
	}
	if d, ok := rvDetail(v, "c1"); !ok || d.Status != CommitmentExpired ||
		d.OriginalQuantity != 6 || d.UsedQuantity != 3 || d.RemainingQuantity != 3 {
		t.Fatalf("c1 rolled back at day 15: %+v ok=%v, want expired 6/3/3", d, ok)
	}
	if _, err := s.Use("u3", "c1", 1, day15); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use after confirmation: got %v, want ErrCommitmentClosed", err)
	}
}

// TestEvaluateFailuresDoNotChangeState 保障承诺尚未确认到期时，失败的直接
// 资格查询也保持记录：空请求编号返回 ErrInvalidParam，未登记的请求编号返回
// ErrNotFound；即使传入晚于承诺到期的时刻，失败查询也不确认任何承诺到期、
// 不改变库存账目。
func TestEvaluateFailuresDoNotChangeState(t *testing.T) {
	s := newStore(t)
	day10 := t0.Add(10 * day)
	day15 := t0.Add(15 * day)
	day20 := t0.Add(20 * day)
	day31 := t0.Add(31 * day)

	if _, err := s.Reserve("c1", "r1", "part1", 6, day20, day10); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, day10); err != nil {
		t.Fatalf("use u1: %v", err)
	}

	// 即便传入晚于承诺到期的时刻，提前失败的查询也不确认任何承诺。
	if _, err := s.Evaluate("", day31); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Evaluate("rMissing", day31); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v, want ErrNotFound", err)
	}

	// 承诺与库存保持原样：未确认到期、未取消，已用两件。
	c, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("get c1: %v", err)
	}
	if c.Expired || c.Canceled || c.Quantity != 6 || c.Used != 2 {
		t.Fatalf("c1 changed by failed evaluates: %+v, want unconfirmed 6/2", c)
	}
	st, err := s.PartStatus("part1", day15)
	if err != nil {
		t.Fatalf("part status at day 15: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("stock changed by failed evaluates: phys=%d occupied=%d committable=%d, want 8/4/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 承诺未被失败查询关闭：到期前时刻仍可领取。
	if _, err := s.Use("u2", "c1", 1, day15); err != nil {
		t.Fatalf("c1 closed by failed evaluate: %v", err)
	}
}
