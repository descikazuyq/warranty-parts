package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障“承诺余量已经全部用完，但承诺既未取消也未到期”这一边界：
// active 只表示承诺未取消、未到期，并不表示仍有数量可以领取；用完余量不会
// 自动把承诺变成 canceled 或 expired。此状态下用尚未成功过的使用编号领取
// 正数量，必须按数量规则整次失败并返回 ErrUsageExceeded——即使备件实物仍有
// 库存，也不能继续扣减，更不能把它报成 ErrCommitmentClosed。调用方据此区分
// “数量不足”与“承诺关闭”，两种拒绝不能互换。
//
// 主例数据（与用户描述一致）：产品 p1 在保、故障代码 FAULTY 未命中除外清单
// FAULTX，备件 part1 初始库存 10 件；承诺 c1（属于 r1）原定 5 件，到期时刻
// expiryOK 晚于全部领取时刻。先用 u1 领取 2 件、再用 u2 领取 3 件，承诺恰好
// 用完：原定 5、已用 5、未用 0，仍为 active；备件实物剩 5 件、有效占用 0 件
// （该承诺未用为 0）、可承诺 5 件。

// exhaustedActiveStore 构造主例初始状态：part1 初始 10 件，c1（属于 r1）原定
// 5 件，已通过两次成功使用 u1=2、u2=3 恰好全部用完；c1 未取消、未到期。
func exhaustedActiveStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t) // p1 在保（保修 30 天、除外 FAULTX），part1 库存 10，r1 故障 FAULTY
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1 for 5: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("first use 2: %v", err)
	}
	if _, err := s.Use("u2", "c1", 3, nowOK); err != nil {
		t.Fatalf("second use 3: %v", err)
	}
	return s
}

// assertExhaustedActive 固定主例在“用完但仍 active”时的全部可见事实：
// 承诺记录原定 5、已用 5、未用 0，未取消、未确认到期，归属 r1/part1 与到期
// 时刻保持原值；备件账目为实物 5、有效占用 0、可承诺 5；请求查询与备件查询
// 都仍能找到这笔 active 承诺（余量归零不移除明细）；使用明细只有 u1=2、
// u2=3 两次成功记录，合计 5。
func assertExhaustedActive(t *testing.T, s *Store) {
	t.Helper()

	c, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("commitment c1: %v", err)
	}
	if c.Quantity != 5 || c.Used != 5 || c.Unused() != 0 {
		t.Fatalf("c1 quantities = %d/%d/%d, want 5/5/0", c.Quantity, c.Used, c.Unused())
	}
	if c.Canceled || c.Expired {
		t.Fatalf("c1 closed flags = canceled=%v expired=%v, want both false", c.Canceled, c.Expired)
	}
	if c.RequestID != "r1" || c.PartID != "part1" || !c.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 identity changed: request=%q part=%q expiry=%v", c.RequestID, c.PartID, c.Expiry)
	}
	if got := c.Status(nowOK); got != CommitmentActive {
		t.Fatalf("c1 status at nowOK = %q, want active", got)
	}

	// 备件账目：实物只剩 5 件（10-2-3），有效占用为 0（active 承诺未用为 0），
	// 可承诺 5 件。
	assertPartAccount(t, s, 5, 0, 5)

	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if len(st.Details) != 1 {
		t.Fatalf("part details = %d, want 1 (exhausted commitment must stay listed)", len(st.Details))
	}
	d := mustDetailByID(t, st.Details, "c1")
	if d.Status != CommitmentActive || d.RequestID != "r1" || d.PartID != "part1" ||
		d.OriginalQuantity != 5 || d.UsedQuantity != 5 || d.RemainingQuantity != 0 ||
		!d.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 part detail = %+v, want active r1/part1 5/5/0 with original expiry", d)
	}

	// 请求查询同样保留这笔承诺，不得因为余量归零就从明细中移除。
	rv, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view r1: %v", err)
	}
	if len(rv.Commitments) != 1 {
		t.Fatalf("r1 commitments = %d, want 1", len(rv.Commitments))
	}
	rd := mustDetailByID(t, rv.Commitments, "c1")
	if rd.Status != CommitmentActive || rd.OriginalQuantity != 5 ||
		rd.UsedQuantity != 5 || rd.RemainingQuantity != 0 {
		t.Fatalf("r1 view detail = %+v, want active 5/5/0", rd)
	}

	// 使用明细只保留两次成功领取，合计 5；任何被拒绝的提交都不能出现在这里。
	list, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("commitment usages: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("usages len = %d, want 2: %+v", len(list), list)
	}
	if list[0] != (Usage{ID: "u1", CommitmentID: "c1", Quantity: 2}) ||
		list[1] != (Usage{ID: "u2", CommitmentID: "c1", Quantity: 3}) {
		t.Fatalf("usages = %+v, want [u1=2 u2=3] sorted by id", list)
	}
}

// TestFullyUsedActiveCommitmentRejectsUsageExceededNotClosed 是用户给定的完整
// 主例：c1 原定 5 件、已分两次成功领取 2 件和 3 件，备件实物还剩 5 件。用一个
// 从未成功过的使用编号再领 1 件，必须整次失败并返回 ErrUsageExceeded，不能从
// 库存余量继续扣，也不能返回 ErrCommitmentClosed——active 不等于还有数量可领。
func TestFullyUsedActiveCommitmentRejectsUsageExceededNotClosed(t *testing.T) {
	s := exhaustedActiveStore(t)
	assertExhaustedActive(t, s)

	// 新编号领取 1 件：承诺自己的未用数量为 0。虽然备件实物还剩 5 件，也不能
	// 继续扣；必须恰好是 ErrUsageExceeded，而不是任何一种关闭错误。
	u, err := s.Use("u3", "c1", 1, nowOK)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use 1 on fully-used active commitment: got %v, want ErrUsageExceeded", err)
	}
	if errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("quantity shortage must not be reported as ErrCommitmentClosed: %v", err)
	}
	if u != (Usage{}) {
		t.Fatalf("rejected use returned non-empty record: %+v", u)
	}

	// 整次失败后全部事实保持：承诺仍 5/5/0 active，备件仍 5/0/5，使用明细仍
	// 只有 u1、u2 两条；被拒绝的 1 件不能成为成功记录，也不能覆盖已有记录。
	assertExhaustedActive(t, s)

	// 被拒绝的编号从未成功过、不绑定承诺：沿用它仍指向 c1 但没有合法数量可领
	// （未用为 0），再次领取 1 件仍按数量不足处理，错误类别不漂移为关闭。
	if again, err := s.Use("u3", "c1", 1, nowOK); !errors.Is(err, ErrUsageExceeded) ||
		errors.Is(err, ErrCommitmentClosed) || again != (Usage{}) {
		t.Fatalf("repeat rejected use: usage=%+v err=%v, want ErrUsageExceeded only", again, err)
	}
	assertExhaustedActive(t, s)
}

// TestFullyUsedCommitmentCanceledNewUseClosed 衔接数量规则与取消边界：用完后
// active 的承诺一旦被取消，再以新使用编号领取正数量，返回 ErrCommitmentClosed
// 且显示 canceled；5 件已用事实保留，不补回实物，不产生新的成功使用记录。
func TestFullyUsedCommitmentCanceledNewUseClosed(t *testing.T) {
	s := exhaustedActiveStore(t)

	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("cancel c1: %v", err)
	}

	// 新使用编号领取 1 件：承诺已取消，必须恰好是 ErrCommitmentClosed，
	// 不能退回成数量不足。
	u, err := s.Use("uCanceled", "c1", 1, nowOK)
	if !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new use after cancel: got %v, want ErrCommitmentClosed", err)
	}
	if errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("closed commitment must not be reported as ErrUsageExceeded: %v", err)
	}
	if u != (Usage{}) {
		t.Fatalf("closed use returned non-empty record: %+v", u)
	}

	// 关闭后保留 5 件原定 / 5 件已用 / 0 件未用，状态 canceled；归属与到期时刻
	// 保持原值。
	c := mustCommitment(t, s, "c1")
	if c.Quantity != 5 || c.Used != 5 || c.Unused() != 0 || !c.Canceled || c.Expired {
		t.Fatalf("c1 after cancel = %+v, want 5/5/0 canceled not expired", c)
	}
	if c.RequestID != "r1" || c.PartID != "part1" || !c.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 identity changed after cancel: %+v", c)
	}
	if got := c.Status(nowOK); got != CommitmentCanceled {
		t.Fatalf("c1 status = %q, want canceled", got)
	}

	// 不补回实物：仍为实物 5、有效占用 0、可承诺 5。
	assertPartAccount(t, s, 5, 0, 5)
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	d := mustDetailByID(t, st.Details, "c1")
	if d.Status != CommitmentCanceled || d.OriginalQuantity != 5 ||
		d.UsedQuantity != 5 || d.RemainingQuantity != 0 {
		t.Fatalf("c1 part detail = %+v, want canceled 5/5/0", d)
	}
	rv, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if rd := mustDetailByID(t, rv.Commitments, "c1"); rd.Status != CommitmentCanceled ||
		rd.UsedQuantity != 5 {
		t.Fatalf("r1 view detail = %+v, want canceled used=5", rd)
	}

	// 不产生新的成功使用记录：仍只有 u1=2、u2=3，合计 5。
	list, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("commitment usages: %v", err)
	}
	if len(list) != 2 || list[0].ID != "u1" || list[1].ID != "u2" ||
		list[0].Quantity != 2 || list[1].Quantity != 3 {
		t.Fatalf("usages after cancel = %+v, want [u1=2 u2=3]", list)
	}
}

// TestFullyUsedCommitmentExpiryNewUseClosed 衔接数量规则与到期边界：未取消的
// 同类承诺（原定 5、已用 5、未用 0）在当前时刻恰好达到自身到期时刻时，以新
// 使用编号领取 1 件，返回 ErrCommitmentClosed 且显示 expired；5 件已用事实
// 保留，不补回实物，不产生新的成功使用记录。
func TestFullyUsedCommitmentExpiryNewUseClosed(t *testing.T) {
	s := exhaustedActiveStore(t)

	// 恰好到达到期时刻（不是晚于）：新使用判断先确认到期，再以关闭拒绝。
	u, err := s.Use("uExpired", "c1", 1, expiryOK)
	if !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new use exactly at expiry: got %v, want ErrCommitmentClosed", err)
	}
	if errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("expired commitment must not be reported as ErrUsageExceeded: %v", err)
	}
	if u != (Usage{}) {
		t.Fatalf("expired use returned non-empty record: %+v", u)
	}

	// 到期确认不可逆：5 件原定 / 5 件已用 / 0 件未用保留，状态 expired。
	c := mustCommitment(t, s, "c1")
	if c.Quantity != 5 || c.Used != 5 || c.Unused() != 0 || c.Canceled || !c.Expired {
		t.Fatalf("c1 after expiry = %+v, want 5/5/0 expired not canceled", c)
	}
	if c.RequestID != "r1" || c.PartID != "part1" || !c.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 identity changed after expiry: %+v", c)
	}
	if got := c.Status(nowOK); got != CommitmentExpired {
		t.Fatalf("c1 status even at earlier time = %q, want expired (irreversible)", got)
	}

	// 不补回实物：未用本就为 0，账目仍为实物 5、有效占用 0、可承诺 5。
	assertPartAccount(t, s, 5, 0, 5)
	st, err := s.PartStatus("part1", expiryOK)
	if err != nil {
		t.Fatalf("part status at expiry: %v", err)
	}
	d := mustDetailByID(t, st.Details, "c1")
	if d.Status != CommitmentExpired || d.OriginalQuantity != 5 ||
		d.UsedQuantity != 5 || d.RemainingQuantity != 0 {
		t.Fatalf("c1 part detail = %+v, want expired 5/5/0", d)
	}
	rv, err := s.RequestView("r1", expiryOK)
	if err != nil {
		t.Fatalf("request view at expiry: %v", err)
	}
	if rd := mustDetailByID(t, rv.Commitments, "c1"); rd.Status != CommitmentExpired ||
		rd.UsedQuantity != 5 {
		t.Fatalf("r1 view detail = %+v, want expired used=5", rd)
	}

	// 不产生新的成功使用记录：仍只有 u1=2、u2=3，合计 5。
	list, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("commitment usages: %v", err)
	}
	if len(list) != 2 || list[0].ID != "u1" || list[1].ID != "u2" ||
		list[0].Quantity != 2 || list[1].Quantity != 3 {
		t.Fatalf("usages after expiry = %+v, want [u1=2 u2=3]", list)
	}
}
