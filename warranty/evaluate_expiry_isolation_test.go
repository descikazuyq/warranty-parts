package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障直接资格查询 Evaluate 与请求明细查询 RequestView 的职责区别：
// Evaluate 只按调用方给定的时刻返回资格依据与拒绝原因，即使该时刻已经晚于关联
// 承诺的到期时刻，也不确认承诺到期、不释放预留占用、不改变已用数量或写入取消
// 标记；RequestView 查看请求及其承诺明细时才会确认相关承诺到期。失败的直接
// 资格查询（空编号、未知请求）同样不借传入的较晚时刻改动任何承诺或库存。

// evalScenarioStore 构造用户给出的主例：产品保修三十天、故障 NOISE 未被除外
// （除外清单含另一代码 FAULTX）；备件初始库存十件；购买后第十天预留六件并于
// 当天领取两件，承诺在购买后第二十天到期。
func evalScenarioStore(t *testing.T) (s *Store, deadline, commitExpiry time.Time) {
	t.Helper()
	purchase := t0
	deadline = purchase.Add(30 * day)
	commitExpiry = purchase.Add(20 * day)

	s = NewStore()
	if err := s.RegisterProduct("p1", purchase, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	day10 := purchase.Add(10 * day)
	if _, err := s.Reserve("c1", "r1", "part1", 6, commitExpiry, day10); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, day10); err != nil {
		t.Fatalf("use u1: %v", err)
	}
	return s, deadline, commitExpiry
}

// assertEvaluateExpiredBasis 断言过保时刻的资格依据：不合格、未命中除外，
// 拒绝原因只有 warranty_expired；购买时刻、保修天数与保修截止时刻仍是登记资料。
func assertEvaluateExpiredBasis(t *testing.T, e *Eligibility, deadline time.Time) {
	t.Helper()
	if e == nil {
		t.Fatal("eligibility is nil")
	}
	if e.RequestID != "r1" || e.ProductID != "p1" || e.FaultCode != "NOISE" {
		t.Fatalf("basis identity = %q/%q/%q, want r1/p1/NOISE", e.RequestID, e.ProductID, e.FaultCode)
	}
	if e.Eligible {
		t.Fatalf("eligible = true, want false: %+v", e)
	}
	if e.Excluded {
		t.Fatalf("excluded = true, want false: %+v", e)
	}
	if len(e.Reasons) != 1 || e.Reasons[0] != ReasonWarrantyExpired {
		t.Fatalf("reasons = %v, want only [warranty_expired]", e.Reasons)
	}
	if !e.PurchaseTime.Equal(t0) {
		t.Fatalf("purchase time = %v, want registered %v", e.PurchaseTime, t0)
	}
	if e.WarrantyDays != 30 {
		t.Fatalf("warranty days = %d, want registered 30", e.WarrantyDays)
	}
	if !e.WarrantyExpiry.Equal(deadline) {
		t.Fatalf("warranty expiry = %v, want %v", e.WarrantyExpiry, deadline)
	}
}

// assertC1Unconfirmed 断言仓库里的 c1 仍是尚未确认到期、未取消的原记录：
// 原数量六件、已用两件，归属与到期时刻不变。
func assertC1Unconfirmed(t *testing.T, s *Store, used int, commitExpiry time.Time) {
	t.Helper()
	c, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("commitment c1: %v", err)
	}
	if c.Quantity != 6 || c.Used != used {
		t.Fatalf("c1 quantity/used = %d/%d, want 6/%d", c.Quantity, c.Used, used)
	}
	if c.Expired {
		t.Fatalf("c1 confirmed expired by eligibility query: %+v", c)
	}
	if c.Canceled {
		t.Fatalf("c1 canceled by eligibility query: %+v", c)
	}
	if c.RequestID != "r1" || c.PartID != "part1" || !c.Expiry.Equal(commitExpiry) {
		t.Fatalf("c1 identity altered: %+v", c)
	}
}

// assertStock 断言备件账目三项数量：实物剩余 / 有效占用 / 可承诺。
func assertStock(t *testing.T, s *Store, now time.Time, physical, occupied, committable int) {
	t.Helper()
	st, err := s.PartStatus("part1", now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// TestEvaluateAtLateTimeDoesNotConfirmCommitment 主例：承诺到期（第二十天）后
// 尚未通过其他操作确认到期时，直接在第三十一天查询资格，只返回该时刻的资格
// 依据与 warranty_expired 一项拒绝原因；查询之后取回承诺，原数量六件、已用
// 两件及尚未确认到期的记录保持原样，不能因这次查询写入取消或到期确认，库存
// 占用也不释放。
func TestEvaluateAtLateTimeDoesNotConfirmCommitment(t *testing.T) {
	s, deadline, commitExpiry := evalScenarioStore(t)
	day31 := t0.Add(31 * day)

	// 直接资格查询：第三十一天已过保（保修截止为第三十天），正常返回不合格，
	// 拒绝原因只有 warranty_expired，依据仍是登记资料。
	e, err := s.Evaluate("r1", day31)
	if err != nil {
		t.Fatalf("evaluate at day 31: %v", err)
	}
	assertEvaluateExpiredBasis(t, e, deadline)

	// 查询不确认承诺到期：第三十一天虽已超过承诺到期时刻（第二十天），c1 仍是
	// 原数量六件、已用两件、未取消、未确认到期的记录。
	assertC1Unconfirmed(t, s, 2, commitExpiry)

	// 资格查询曾使用较晚时刻不能替代本次时刻：回到保修期内的第十五天再查，
	// 资格仍按第十五天判断为合格、无拒绝原因，登记依据不变。
	day15 := t0.Add(15 * day)
	e15, err := s.Evaluate("r1", day15)
	if err != nil {
		t.Fatalf("evaluate at day 15: %v", err)
	}
	if !e15.Eligible || e15.Excluded || len(e15.Reasons) != 0 {
		t.Fatalf("day 15 eligibility = %+v, want eligible with no reasons", e15)
	}
	if !e15.PurchaseTime.Equal(t0) || e15.WarrantyDays != 30 || !e15.WarrantyExpiry.Equal(deadline) {
		t.Fatalf("day 15 basis altered: %+v", e15)
	}

	// 以第十五天继续操作：承诺尚未确认到期且本次时刻早于到期时刻，库存仍显示
	// 实物剩余八件、有效占用四件、可承诺四件。
	assertStock(t, s, day15, 8, 4, 4)

	// 用新的使用编号再领取一件仍能成功：承诺未用四件尚在，较早时刻不受早先
	// 那次较晚时刻资格查询影响。
	if _, err := s.Use("u2", "c1", 1, day15); err != nil {
		t.Fatalf("use u2 at day 15: %v", err)
	}
	assertC1Unconfirmed(t, s, 3, commitExpiry)
	// 账目变为实物七件、有效占用三件、可承诺四件。
	assertStock(t, s, day15, 7, 3, 4)
}

// TestRequestViewConfirmsExpiryAfterLateEvaluate 承接上一例的状态：直接资格
// 查询从不确认到期，而第三十一天查看该请求的明细时应确认原承诺到期——保留
// 六件原数量和三件已用数量，只释放剩余三件占用，实物七件不回补；一经确认，
// 之后即使查看较早时刻或用新编号在较早时刻领取，都不能重新开放这笔承诺。
func TestRequestViewConfirmsExpiryAfterLateEvaluate(t *testing.T) {
	s, deadline, commitExpiry := evalScenarioStore(t)
	day15 := t0.Add(15 * day)
	day31 := t0.Add(31 * day)

	// 第三十一天直接查资格：不确认到期。
	e, err := s.Evaluate("r1", day31)
	if err != nil {
		t.Fatalf("evaluate at day 31: %v", err)
	}
	assertEvaluateExpiredBasis(t, e, deadline)
	assertC1Unconfirmed(t, s, 2, commitExpiry)

	// 第十五天再领取一件，证明承诺仍开放。
	if _, err := s.Use("u2", "c1", 1, day15); err != nil {
		t.Fatalf("use u2 at day 15: %v", err)
	}
	assertStock(t, s, day15, 7, 3, 4)

	// 第三十一天查看请求明细：这次查询确认原承诺到期。
	v, err := s.RequestView("r1", day31)
	if err != nil {
		t.Fatalf("request view at day 31: %v", err)
	}
	if v.Eligibility == nil || v.Eligibility.Eligible ||
		len(v.Eligibility.Reasons) != 1 || v.Eligibility.Reasons[0] != ReasonWarrantyExpired {
		t.Fatalf("view eligibility = %+v, want only warranty_expired", v.Eligibility)
	}
	if len(v.Commitments) != 1 {
		t.Fatalf("commitments = %+v, want exactly c1", v.Commitments)
	}
	d := v.Commitments[0]
	// 保留六件原数量和三件已用数量，只释放剩余三件占用，状态 expired。
	want := CommitmentDetail{
		CommitmentID:      "c1",
		RequestID:         "r1",
		PartID:            "part1",
		OriginalQuantity:  6,
		UsedQuantity:      3,
		RemainingQuantity: 3,
		Expiry:            commitExpiry,
		Status:            CommitmentExpired,
	}
	if d != want {
		t.Fatalf("c1 detail = %+v, want %+v", d, want)
	}

	// 仓库记录已被确认到期；只释放三件未用占用，已领走的三件实物不回补。
	c, _ := s.Commitment("c1")
	if !c.Expired || c.Canceled || c.Quantity != 6 || c.Used != 3 {
		t.Fatalf("c1 after request view: %+v, want expired 6/3", c)
	}
	// 实物七件、有效占用为零、可承诺七件。
	assertStock(t, s, day31, 7, 0, 7)

	// 之后即使查看较早时刻，也不能重新开放这笔已确认到期的承诺：
	// 请求明细仍 expired，备件账目在较早时刻也不回退占用。
	vEarly, err := s.RequestView("r1", day15)
	if err != nil {
		t.Fatalf("request view at day 15 after confirmation: %v", err)
	}
	if len(vEarly.Commitments) != 1 || vEarly.Commitments[0].Status != CommitmentExpired ||
		vEarly.Commitments[0].OriginalQuantity != 6 || vEarly.Commitments[0].UsedQuantity != 3 ||
		vEarly.Commitments[0].RemainingQuantity != 3 {
		t.Fatalf("c1 reopened in early view: %+v", vEarly.Commitments)
	}
	assertStock(t, s, day15, 7, 0, 7)

	// 较早时刻用新使用编号领取：一律关闭，不扣库存、不改已用数量。
	if _, err := s.Use("u3", "c1", 1, day15); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use after confirmed expiry at earlier time: got %v, want ErrCommitmentClosed", err)
	}
	assertStock(t, s, day15, 7, 0, 7)
	c, _ = s.Commitment("c1")
	if c.Used != 3 {
		t.Fatalf("closed use changed c1 used: %+v", c)
	}
}

// TestEvaluateFailuresDoNotConfirmExpiry 承诺尚未确认到期时，失败的直接资格
// 查询也保持记录：空请求编号返回 ErrInvalidParam，未登记的请求编号返回
// ErrNotFound；即使传入晚于承诺到期的时刻，也不借失败查询确认任何承诺到期或
// 改变库存，承诺在到期前时刻仍可领取。
func TestEvaluateFailuresDoNotConfirmExpiry(t *testing.T) {
	s, _, commitExpiry := evalScenarioStore(t)
	day15 := t0.Add(15 * day)
	day31 := t0.Add(31 * day)

	// 传入晚于承诺到期（第二十天）的时刻：提前失败的查询不确认任何承诺。
	if _, err := s.Evaluate("", day31); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Evaluate("rMissing", day31); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v, want ErrNotFound", err)
	}

	// c1 未被确认：原数量六件、已用两件、未取消，仍在有效期内可领取。
	assertC1Unconfirmed(t, s, 2, commitExpiry)
	assertStock(t, s, day15, 8, 4, 4)
	if _, err := s.Use("u2", "c1", 1, day15); err != nil {
		t.Fatalf("c1 closed by failed eligibility queries: %v", err)
	}
	// 领取成功后账目为七件、三件、四件。
	assertStock(t, s, day15, 7, 3, 4)
	assertC1Unconfirmed(t, s, 3, commitExpiry)

	// 再次以晚时刻对未知请求失败查询，仍不改动已开放的承诺。
	if _, err := s.Evaluate("rMissing", day31); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request again: got %v, want ErrNotFound", err)
	}
	c, _ := s.Commitment("c1")
	if c.Expired || c.Canceled || c.Used != 3 {
		t.Fatalf("c1 changed by repeated failed query: %+v", c)
	}
	assertStock(t, s, day15, 7, 3, 4)
}
