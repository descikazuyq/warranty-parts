package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障“承诺余量已经全部用完，但承诺既未取消也未到期”时的边界：
// active 只表示承诺未取消、未到期，并不表示仍有数量可领；用完余量不会把承诺
// 自动变成 canceled 或 expired。因此余量归零后再以新使用编号领取正数量，拒绝
// 原因必须是数量不足（ErrUsageExceeded），而不是承诺关闭
// （ErrCommitmentClosed），调用方需要能区分这两种不可互换的结果。
//
// 主例数据（与用户描述一致）：产品在保、故障代码 NOISE 未命中除外清单；
// 备件 part1 初始库存 10 件；请求 r1 的承诺 c1 原定 5 件，到期时刻 expiryOK
// 晚于领取时刻 nowOK。先成功领取 2 件、再成功领取 3 件，把 c1 的未用数量
// 恰好耗尽：实物剩余 5 件、有效占用 0（c1 未用为 0）、可承诺 5 件。

// exhaustedActiveStore 构造“五件承诺已全部成功领取、但承诺仍有效”的仓库：
// part1 初始 10 件；c1（属于 r1）预留 5 件，u1 领取 2 件、u2 领取 3 件，
// 此后 c1 已用 5、未用 0，未取消、未到期，实物剩 5 件。
func exhaustedActiveStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request r1: %v", err)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("seed use 2 from c1: %v", err)
	}
	if _, err := s.Use("u2", "c1", 3, nowOK); err != nil {
		t.Fatalf("seed use 3 from c1: %v", err)
	}
	return s
}

// assertExhaustedActive 断言“已全部用完但仍有效”的完整状态：
//   - 承诺记录仍是原定 5、已用 5、未用 0，未取消、未确认到期，按领取时刻
//     查看状态为 active，归属 r1、备件 part1、到期时刻 expiryOK 保持原值；
//   - 备件账目为实物 5、有效占用 0、可承诺 5（余量为零的 active 承诺不占
//     占用，但实物不会因“用完”而多扣）；
//   - 备件明细与请求视图都仍能找到 c1，不因子余量归零而被移除；
//   - 成功使用明细只含 u1=2、u2=3，合计 5。
func assertExhaustedActive(t *testing.T, s *Store) {
	t.Helper()

	c := mustCommitment(t, s, "c1")
	if c.Quantity != 5 || c.Used != 5 || c.Unused() != 0 {
		t.Fatalf("c1 quantities = qty=%d used=%d unused=%d, want 5/5/0",
			c.Quantity, c.Used, c.Unused())
	}
	if c.Canceled || c.Expired {
		t.Fatalf("exhausted commitment flipped to closed: canceled=%v expired=%v",
			c.Canceled, c.Expired)
	}
	if c.Status(nowOK) != CommitmentActive {
		t.Fatalf("exhausted commitment status = %q, want active", c.Status(nowOK))
	}
	if c.RequestID != "r1" || c.PartID != "part1" || !c.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 ownership/expiry changed: request=%s part=%s expiry=%v, want r1/part1/%v",
			c.RequestID, c.PartID, c.Expiry, expiryOK)
	}

	// 实物 5（10 减已领 5）、有效占用 0（未用为 0）、可承诺 5。
	assertPartAccount(t, s, 5, 0, 5)

	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	d, ok := detailByID(st, "c1")
	if !ok {
		t.Fatalf("exhausted c1 missing from part details: %+v", st.Details)
	}
	if d.Status != CommitmentActive || d.RequestID != "r1" || d.PartID != "part1" ||
		d.OriginalQuantity != 5 || d.UsedQuantity != 5 || d.RemainingQuantity != 0 ||
		!d.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 part detail = %+v, want active r1/part1 5/5/0 expiry kept", d)
	}

	rv, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view r1: %v", err)
	}
	rd := mustDetailByID(t, rv.Commitments, "c1")
	if rd.Status != CommitmentActive || rd.OriginalQuantity != 5 ||
		rd.UsedQuantity != 5 || rd.RemainingQuantity != 0 || !rd.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 request detail = %+v, want active 5/5/0 expiry kept", rd)
	}

	us, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("commitment usages c1: %v", err)
	}
	want := []Usage{
		{ID: "u1", CommitmentID: "c1", Quantity: 2},
		{ID: "u2", CommitmentID: "c1", Quantity: 3},
	}
	if len(us) != len(want) {
		t.Fatalf("usages = %+v, want %+v", us, want)
	}
	total := 0
	for i, u := range us {
		if u != want[i] {
			t.Fatalf("usages[%d] = %+v, want %+v", i, u, want[i])
		}
		total += u.Quantity
	}
	if total != 5 {
		t.Fatalf("usages total = %d, want 5", total)
	}
}

// TestExhaustedActiveCommitmentRejectsFurtherUseAsUsageExceeded 是用户给定的
// 主例：五件承诺已分两次（2 件、3 件）全部成功领取，承诺仍未取消、未到期。
// 尽管备件实物还剩 5 件，用尚未成功过的编号再领 1 件必须整次失败并返回
// ErrUsageExceeded——不能继续从库存余量扣减，更不能返回 ErrCommitmentClosed。
// 失败后承诺、请求视图、备件账目与使用明细都保持“已用完但仍 active”的原样，
// 被拒绝的 1 件既不成为成功记录，也不覆盖已有的两次成功记录。
func TestExhaustedActiveCommitmentRejectsFurtherUseAsUsageExceeded(t *testing.T) {
	s := exhaustedActiveStore(t)
	assertExhaustedActive(t, s)

	// 新编号领取 1 件：c1 自己的未用数量为 0。即使实物还剩 5 件也不能从
	// 库存余量继续扣；数量不足与承诺关闭是两种不同的拒绝原因，这里只能是
	// ErrUsageExceeded，不能被报告成 ErrCommitmentClosed。
	u, err := s.Use("u3", "c1", 1, nowOK)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use 1 on exhausted active commitment: got %v, want ErrUsageExceeded", err)
	}
	if errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("quantity exhaustion must not be reported as ErrCommitmentClosed: %v", err)
	}
	if u != (Usage{}) {
		t.Fatalf("rejected use returned non-empty record: %+v", u)
	}

	// 整次失败不留痕迹：承诺仍是 5/5/0 active，归属与到期时刻不变；备件仍是
	// 实物 5、有效占用 0、可承诺 5；使用明细仍只有前面两次成功领取。
	assertExhaustedActive(t, s)

	// 被拒绝的编号没有成为成功记录：沿用同一编号、同样内容仍按一笔新的
	// 数量不足使用被拒绝（而不是按编号冲突取回或成功），再次锁定失败不占编号。
	if again, err := s.Use("u3", "c1", 1, nowOK); !errors.Is(err, ErrUsageExceeded) || again != (Usage{}) {
		t.Fatalf("rejected id resubmit = %+v err %v, want ErrUsageExceeded with empty result", again, err)
	}
	assertExhaustedActive(t, s)
}

// TestExhaustedCommitmentOnceCanceledRejectsNewUseAsClosed 衔接数量规则与关闭
// 边界：已经用完（5/5/0、仍 active）的承诺一旦被取消，再以新使用编号领取
// 正数量，返回的必须是 ErrCommitmentClosed（显示 canceled），而不是
// ErrUsageExceeded——取消优先于数量判断。取消不补回实物，已用 5 件的事实
// 保留，也不产生新的成功使用记录。
func TestExhaustedCommitmentOnceCanceledRejectsNewUseAsClosed(t *testing.T) {
	s := exhaustedActiveStore(t)

	canceled, err := s.Cancel("c1", nowOK)
	if err != nil {
		t.Fatalf("cancel exhausted commitment: %v", err)
	}
	if !canceled.Canceled || canceled.Expired || canceled.Quantity != 5 || canceled.Used != 5 {
		t.Fatalf("cancel result = %+v, want canceled qty=5 used=5 not expired", canceled)
	}

	// 新使用编号领取 1 件：承诺已关闭，按 ErrCommitmentClosed 拒绝，
	// 不能与数量不足混为 ErrUsageExceeded；整次不扣减、不留记录。
	u, err := s.Use("uAfterCancel", "c1", 1, nowOK)
	if !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new use on canceled exhausted commitment: got %v, want ErrCommitmentClosed", err)
	}
	if errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("closed commitment must not be reported as ErrUsageExceeded: %v", err)
	}
	if u != (Usage{}) {
		t.Fatalf("rejected use on canceled commitment returned record: %+v", u)
	}

	// 承诺记录：5/5/0、canceled，未被改成 expired，归属与到期时刻保持原值。
	c := mustCommitment(t, s, "c1")
	if c.Quantity != 5 || c.Used != 5 || c.Unused() != 0 || !c.Canceled || c.Expired {
		t.Fatalf("c1 after canceled rejection: %+v, want 5/5/0 canceled not expired", c)
	}
	if c.Status(nowOK) != CommitmentCanceled || c.Status(expiryOK) != CommitmentCanceled {
		t.Fatalf("canceled status not stable across times: now=%q at-expiry=%q",
			c.Status(nowOK), c.Status(expiryOK))
	}
	if c.RequestID != "r1" || !c.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 ownership/expiry changed after cancel: request=%s expiry=%v",
			c.RequestID, c.Expiry)
	}

	// 不补回实物：仍是实物 5；取消承诺不占有效占用，可承诺 5。
	assertPartAccount(t, s, 5, 0, 5)
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	d := mustDetailByID(t, st.Details, "c1")
	if d.Status != CommitmentCanceled || d.OriginalQuantity != 5 ||
		d.UsedQuantity != 5 || d.RemainingQuantity != 0 || !d.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 part detail after cancel = %+v, want canceled 5/5/0 expiry kept", d)
	}
	rv, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if rd := mustDetailByID(t, rv.Commitments, "c1"); rd.Status != CommitmentCanceled ||
		rd.UsedQuantity != 5 || rd.RemainingQuantity != 0 {
		t.Fatalf("c1 request detail after cancel = %+v, want canceled 5/5/0", rd)
	}

	// 已用 5 件的事实保留，被拒绝的新使用不进明细，也不覆盖已有记录。
	us, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages after cancel: %v", err)
	}
	want := []Usage{
		{ID: "u1", CommitmentID: "c1", Quantity: 2},
		{ID: "u2", CommitmentID: "c1", Quantity: 3},
	}
	if len(us) != len(want) {
		t.Fatalf("usages after cancel = %+v, want %+v", us, want)
	}
	for i, got := range us {
		if got != want[i] {
			t.Fatalf("usages after cancel[%d] = %+v, want %+v", i, got, want[i])
		}
	}

	// 已成功的旧使用原样重试仍取回首次记录，不再次扣减，不是取消后又领走备件。
	if got, err := s.Use("u1", "c1", 2, nowOK); err != nil || got != want[0] {
		t.Fatalf("previous successful use after cancel = %+v err %v, want %+v", got, err, want[0])
	}
	assertPartAccount(t, s, 5, 0, 5)
}

// TestExhaustedCommitmentAtExpiryRejectsNewUseAsClosed 是关闭边界的另一种
// 情况：未取消的同类承诺（5/5/0、active）恰好到达自身到期时刻时，以新使用
// 编号领取 1 件，必须返回 ErrCommitmentClosed（显示 expired），而不是
// ErrUsageExceeded。到期确认由这次新使用完成且不可逆：保留 5 件已用的事实，
// 不补回实物，不产生新的成功使用记录；之后即使传入更早时刻也仍被关闭。
func TestExhaustedCommitmentAtExpiryRejectsNewUseAsClosed(t *testing.T) {
	s := exhaustedActiveStore(t)

	// 恰好到期时刻的新使用：确认到期并按 ErrCommitmentClosed 拒绝，
	// 不能与数量不足混为 ErrUsageExceeded；整次不扣减、不留记录。
	u, err := s.Use("uAfterExpiry", "c1", 1, expiryOK)
	if !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new use exactly at expiry of exhausted commitment: got %v, want ErrCommitmentClosed", err)
	}
	if errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("expired commitment must not be reported as ErrUsageExceeded: %v", err)
	}
	if u != (Usage{}) {
		t.Fatalf("rejected use at expiry returned record: %+v", u)
	}

	// 承诺记录：5/5/0、Expired 已确认，未取消，归属与到期时刻保持原值。
	c := mustCommitment(t, s, "c1")
	if c.Quantity != 5 || c.Used != 5 || c.Unused() != 0 || c.Canceled || !c.Expired {
		t.Fatalf("c1 after expiry rejection: %+v, want 5/5/0 expired not canceled", c)
	}
	if c.Status(expiryOK) != CommitmentExpired {
		t.Fatalf("status at expiry = %q, want expired", c.Status(expiryOK))
	}
	// 到期确认不可逆：按更早的领取时刻查看也持续 expired。
	if c.Status(nowOK) != CommitmentExpired {
		t.Fatalf("status at earlier time = %q, want expired", c.Status(nowOK))
	}
	if c.RequestID != "r1" || !c.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 ownership/expiry changed after expiry: request=%s expiry=%v",
			c.RequestID, c.Expiry)
	}

	// 不补回实物：仍是实物 5；到期承诺不占有效占用，可承诺 5（按更早时刻
	// 查询也不回落）。
	assertPartAccount(t, s, 5, 0, 5)
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	if st.PhysicalRemaining != 5 || st.ActiveOccupied != 0 || st.Committable != 5 {
		t.Fatalf("stock at earlier time after expiry = %+v, want 5/0/5", st)
	}
	d := mustDetailByID(t, st.Details, "c1")
	if d.Status != CommitmentExpired || d.OriginalQuantity != 5 ||
		d.UsedQuantity != 5 || d.RemainingQuantity != 0 || !d.Expiry.Equal(expiryOK) {
		t.Fatalf("c1 part detail after expiry = %+v, want expired 5/5/0 expiry kept", d)
	}
	rv, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view at earlier time: %v", err)
	}
	if rd := mustDetailByID(t, rv.Commitments, "c1"); rd.Status != CommitmentExpired ||
		rd.UsedQuantity != 5 || rd.RemainingQuantity != 0 {
		t.Fatalf("c1 request detail after expiry = %+v, want expired 5/5/0", rd)
	}

	// 已用 5 件的事实保留，到期时被拒绝的新使用不进明细，也不覆盖已有记录。
	us, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages after expiry: %v", err)
	}
	want := []Usage{
		{ID: "u1", CommitmentID: "c1", Quantity: 2},
		{ID: "u2", CommitmentID: "c1", Quantity: 3},
	}
	if len(us) != len(want) {
		t.Fatalf("usages after expiry = %+v, want %+v", us, want)
	}
	for i, got := range us {
		if got != want[i] {
			t.Fatalf("usages after expiry[%d] = %+v, want %+v", i, got, want[i])
		}
	}

	// 回退到到期前的时刻，新使用仍被关闭：到期一经确认不可逆。
	if earlier, err := s.Use("uRollback", "c1", 1, expiryOK.Add(-time.Second)); !errors.Is(err, ErrCommitmentClosed) || earlier != (Usage{}) {
		t.Fatalf("new use at earlier time after confirmed expiry = %+v err %v, want ErrCommitmentClosed", earlier, err)
	}
	// 已成功的旧使用原样重试仍取回首次记录，不再次扣减。
	if got, err := s.Use("u2", "c1", 3, nowOK); err != nil || got != want[1] {
		t.Fatalf("previous successful use after expiry = %+v err %v, want %+v", got, err, want[1])
	}
	assertPartAccount(t, s, 5, 0, 5)
}
