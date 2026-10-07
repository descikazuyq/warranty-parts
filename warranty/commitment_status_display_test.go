package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障“按编号取回承诺记录（Commitment）后查看状态（Status）”与
// “仓库确认承诺到期、释放未用占用”的区别：
//
//   - Commitment 只取回仓库中该承诺当时的值副本，Status(now) 只是按手中副本
//     和调用方给定时刻给出显示结果：时刻达到到期时刻即显示 expired，但不回写
//     副本与仓库记录的 Expired 标记，也不释放任何未用占用。
//   - 只有库存查询（PartStatus）、请求明细查询（RequestView）、合法新预留的
//     库存核算或针对已知承诺的新使用判断，才会在本次时刻达到到期时刻时确认
//     本次涉及的承诺到期；一经确认不可逆，重新取回的记录即使按较早时刻查看
//     也持续 expired，而此前保存的旧副本不随之更新。
//   - 已取消记录无论传入什么时刻都显示 canceled（取消优先于到期）；取回不
//     存在的非空承诺编号返回 ErrNotFound，不产生承诺、不改变已有库存。
//
// 主场景固定为：产品始终在保、故障代码不命中除外清单；初始库存十二件，成功
// 预留七件，到期前领取三件——实物剩余九件、未用四件、到期前可承诺五件。

// snapshotScenarioStore 构造主场景仓库，并在到期前完成七件预留与三件领取：
// 实物剩余九件、承诺为原数量七件/已用三件/未用四件、尚未确认到期。
func snapshotScenarioStore(t *testing.T) (s *Store, expiry, beforeExpiry time.Time) {
	t.Helper()
	purchase := t0
	reserveAt := purchase.Add(10 * day)
	expiry = purchase.Add(40 * day)
	beforeExpiry = expiry.Add(-time.Nanosecond)

	s = NewStore()
	// 保修三百六十五天：示例各时刻（含到期之后）产品始终在保；
	// 除外清单只有 FAULTX，NOISE 始终不命中。
	if err := s.RegisterProduct("p-snap", purchase, 365, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part-snap", 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r-snap", "p-snap", "NOISE"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	if _, err := s.Reserve("c-snap", "r-snap", "part-snap", 7, expiry, reserveAt); err != nil {
		t.Fatalf("reserve seven: %v", err)
	}
	if _, err := s.Use("u-snap-1", "c-snap", 3, reserveAt); err != nil {
		t.Fatalf("use three before expiry: %v", err)
	}
	return s, expiry, beforeExpiry
}

// assertSnapshotSevenThreeFour 断言手中承诺副本保持取回时的数量事实：
// 原数量七件、已用三件、未用四件，且未取消、未确认到期。
func assertSnapshotSevenThreeFour(t *testing.T, c Commitment, where string) {
	t.Helper()
	if c.Quantity != 7 || c.Used != 3 || c.Unused() != 4 {
		t.Fatalf("%s: snapshot quantities = %d/%d/%d, want 7/3/4",
			where, c.Quantity, c.Used, c.Unused())
	}
	if c.Expired {
		t.Fatalf("%s: snapshot Expired became true, want false", where)
	}
	if c.Canceled {
		t.Fatalf("%s: snapshot Canceled became true, want false", where)
	}
}

// assertRepoSnapshotUnconfirmed 断言仓库中的 c-snap 仍未确认到期，
// 原数量七件、已用三件、未用四件保持原值。
func assertRepoSnapshotUnconfirmed(t *testing.T, s *Store, where string) {
	t.Helper()
	got, err := s.Commitment("c-snap")
	if err != nil {
		t.Fatalf("%s: re-fetch commitment: %v", where, err)
	}
	assertSnapshotSevenThreeFour(t, got, where+" repository record")
}

// assertSnapshotStock 断言备件账目的三项数量：实物剩余 / 有效占用 / 可承诺。
func assertSnapshotStock(t *testing.T, s *Store, now time.Time, physical, occupied, committable int, where string) {
	t.Helper()
	st, err := s.PartStatus("part-snap", now)
	if err != nil {
		t.Fatalf("%s: part status: %v", where, err)
	}
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("%s: stock = %d/%d/%d, want %d/%d/%d",
			where, st.PhysicalRemaining, st.ActiveOccupied, st.Committable,
			physical, occupied, committable)
	}
}

// TestCommitmentStatusDisplayDoesNotConfirmExpiry 主例：取回的记录保留
// 七件原数量、三件已用、四件未用、到期确认标记为否；按到期前一纳秒查看为
// active，恰好到期及之后查看为 expired。这些显示操作既不改变手中副本，也
// 不改变重新取回的仓库记录；随后按到期前时刻核对库存仍是九件实物、四件
// 有效占用、五件可承诺，不能因为显示过 expired 就提前释放四件占用。
func TestCommitmentStatusDisplayDoesNotConfirmExpiry(t *testing.T) {
	s, expiry, beforeExpiry := snapshotScenarioStore(t)
	afterExpiry := expiry.Add(time.Nanosecond)

	// 通过 Commitment 取回记录：只读副本，不确认到期。
	snapshot, err := s.Commitment("c-snap")
	if err != nil {
		t.Fatalf("fetch commitment: %v", err)
	}
	assertSnapshotSevenThreeFour(t, snapshot, "initial fetch")

	// 到期前一纳秒：active；恰好到期与到期之后：仅按手中资料显示 expired。
	if got := snapshot.Status(beforeExpiry); got != CommitmentActive {
		t.Fatalf("status one nanosecond before expiry = %q, want active", got)
	}
	if got := snapshot.Status(expiry); got != CommitmentExpired {
		t.Fatalf("status at expiry = %q, want expired", got)
	}
	if got := snapshot.Status(afterExpiry); got != CommitmentExpired {
		t.Fatalf("status after expiry = %q, want expired", got)
	}

	// 显示操作不回写手中副本：仍是未确认标记与 7/3/4 的数量事实。
	assertSnapshotSevenThreeFour(t, snapshot, "after status displays")

	// 显示操作也不确认仓库中的到期：重新取回的记录仍是未确认的 7/3/4。
	assertRepoSnapshotUnconfirmed(t, s, "after status displays")

	// 按到期前的时刻核对库存：实物九件、有效占用四件、可承诺五件。
	// 先前的 expired 显示没有释放任何占用。
	assertSnapshotStock(t, s, beforeExpiry, 9, 4, 5, "before expiry after displays")

	// 再做一轮晚时刻显示与早时刻显示的交错，结论不变：Status 不要求调用
	// 时刻递增，也不在副本上留下任何状态。
	if got := snapshot.Status(afterExpiry); got != CommitmentExpired {
		t.Fatalf("late status = %q, want expired", got)
	}
	if got := snapshot.Status(beforeExpiry); got != CommitmentActive {
		t.Fatalf("back to early status = %q, want active", got)
	}
	assertSnapshotSevenThreeFour(t, snapshot, "after interleaved displays")
	assertRepoSnapshotUnconfirmed(t, s, "after interleaved displays")
	assertSnapshotStock(t, s, beforeExpiry, 9, 4, 5, "before expiry after interleaved displays")
}

// TestPartStatusConfirmsExpiryAfterDisplayOnly 承接主例：先通过手中副本显示
// 过 expired（不发生任何仓库变化），再在承诺恰好到期的时刻通过现有备件库存
// 查询确认到期。此次确认只释放未用四件：实物仍为九件、有效占用为零、可
// 承诺九件；承诺明细仍保留七件原数量、三件已用、四件未用。重新取回的记录
// 带已确认到期标记，即使按到期前时刻查看也持续 expired；此前保存的旧副本
// 仍保持未确认标记，按较早时刻查看仍显示 active，不随仓库变化自动更新。
func TestPartStatusConfirmsExpiryAfterDisplayOnly(t *testing.T) {
	s, expiry, beforeExpiry := snapshotScenarioStore(t)

	// 先取回旧副本，并按到期时刻显示 expired——只显示，不确认。
	oldSnapshot, err := s.Commitment("c-snap")
	if err != nil {
		t.Fatalf("fetch commitment: %v", err)
	}
	if got := oldSnapshot.Status(expiry); got != CommitmentExpired {
		t.Fatalf("old snapshot at expiry displays %q, want expired", got)
	}
	// 仓库尚未确认：到期前库存仍是 9/4/5。
	assertSnapshotStock(t, s, beforeExpiry, 9, 4, 5, "before warehouse confirmation")

	// 在承诺恰好到期的时刻通过备件库存查询确认到期。
	atExpiry, err := s.PartStatus("part-snap", expiry)
	if err != nil {
		t.Fatalf("part status at expiry: %v", err)
	}
	// 只释放未用四件：实物仍九件，有效占用清零，可承诺九件。
	if atExpiry.PhysicalRemaining != 9 || atExpiry.ActiveOccupied != 0 || atExpiry.Committable != 9 {
		t.Fatalf("stock at expiry = %d/%d/%d, want 9/0/9",
			atExpiry.PhysicalRemaining, atExpiry.ActiveOccupied, atExpiry.Committable)
	}
	d, ok := detailByID(atExpiry, "c-snap")
	if !ok {
		t.Fatalf("c-snap missing from details: %+v", atExpiry.Details)
	}
	// 明细保留 7/3/4 的数量事实，状态 expired。
	if d.OriginalQuantity != 7 || d.UsedQuantity != 3 ||
		d.RemainingQuantity != 4 || d.Status != CommitmentExpired {
		t.Fatalf("c-snap detail = %+v, want 7/3/4 expired", d)
	}

	// 重新取回的记录带有已确认到期标记：即使按到期前的时刻查看也持续 expired。
	confirmed, err := s.Commitment("c-snap")
	if err != nil {
		t.Fatalf("re-fetch after confirmation: %v", err)
	}
	if !confirmed.Expired || confirmed.Canceled {
		t.Fatalf("re-fetched record flags = expired:%v canceled:%v, want expired=true canceled=false",
			confirmed.Expired, confirmed.Canceled)
	}
	if confirmed.Quantity != 7 || confirmed.Used != 3 || confirmed.Unused() != 4 {
		t.Fatalf("re-fetched quantities = %d/%d/%d, want 7/3/4",
			confirmed.Quantity, confirmed.Used, confirmed.Unused())
	}
	if got := confirmed.Status(beforeExpiry); got != CommitmentExpired {
		t.Fatalf("confirmed record viewed before expiry = %q, want expired", got)
	}

	// 到期确认不可逆：到期前时刻核对库存也保持 9/0/9，四件占用不会回占。
	assertSnapshotStock(t, s, beforeExpiry, 9, 0, 9, "after confirmation viewed early")
	earlyView, err := s.PartStatus("part-snap", beforeExpiry)
	if err != nil {
		t.Fatalf("early part status: %v", err)
	}
	if d, ok := detailByID(earlyView, "c-snap"); !ok || d.Status != CommitmentExpired {
		t.Fatalf("detail viewed early = %+v ok=%v, want expired", d, ok)
	}

	// 此前保存的旧副本不随仓库确认更新：仍是未确认标记，按到期前时刻查看
	// 仍显示 active；两份记录各自体现取回时的事实。
	if oldSnapshot.Expired {
		t.Fatal("old snapshot Expired mutated by warehouse confirmation")
	}
	if got := oldSnapshot.Status(beforeExpiry); got != CommitmentActive {
		t.Fatalf("old snapshot viewed early = %q, want active", got)
	}
	// 旧副本按到期及之后时刻仍只做显示：结果 expired，但标记依旧为否。
	if got := oldSnapshot.Status(expiry); got != CommitmentExpired {
		t.Fatalf("old snapshot at expiry = %q, want expired display", got)
	}
	if oldSnapshot.Expired {
		t.Fatal("old snapshot Expired mutated by displaying expired again")
	}
}

// TestCanceledCommitmentStatusAlwaysCanceled 边界一：已取消承诺取回后，无论
// 按到期前、到期时还是到期之后查看，都显示 canceled——取消优先于到期；
// 仓库也不会把它确认成 expired，其未用数量始终不计入有效占用。
func TestCanceledCommitmentStatusAlwaysCanceled(t *testing.T) {
	purchase := t0
	reserveAt := purchase.Add(10 * day)
	expiry := purchase.Add(40 * day)

	s := NewStore()
	if err := s.RegisterProduct("p-snap-cancel", purchase, 365, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part-snap-cancel", 5); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r-snap-cancel", "p-snap-cancel", "NOISE"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	canceledResult, err := s.Reserve("c-snap-cancel", "r-snap-cancel", "part-snap-cancel", 2, expiry, reserveAt)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	canceledResult, err = s.Cancel("c-snap-cancel", reserveAt)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Cancel 返回的手中副本：到期前、到期时、到期之后一律 canceled。
	for name, now := range map[string]time.Time{
		"before expiry": expiry.Add(-time.Nanosecond),
		"at expiry":     expiry,
		"after expiry":  expiry.Add(time.Nanosecond),
	} {
		if got := canceledResult.Status(now); got != CommitmentCanceled {
			t.Fatalf("canceled result Status %s = %q, want canceled", name, got)
		}
	}

	// 重新取回的记录也是如此，且仓库未把取消确认成到期。
	fromRepo, err := s.Commitment("c-snap-cancel")
	if err != nil {
		t.Fatalf("fetch canceled commitment: %v", err)
	}
	if !fromRepo.Canceled || fromRepo.Expired {
		t.Fatalf("repo record flags = canceled:%v expired:%v, want canceled=true expired=false",
			fromRepo.Canceled, fromRepo.Expired)
	}
	for name, now := range map[string]time.Time{
		"before expiry": expiry.Add(-time.Nanosecond),
		"at expiry":     expiry,
		"after expiry":  expiry.Add(time.Nanosecond),
	} {
		if got := fromRepo.Status(now); got != CommitmentCanceled {
			t.Fatalf("repo record Status %s = %q, want canceled", name, got)
		}
	}

	// 到期时刻的库存查询不改变取消记录：未用两件不计占用，库存 5/0/5，
	// 明细仍为 2/0/2 canceled。
	st, err := s.PartStatus("part-snap-cancel", expiry)
	if err != nil {
		t.Fatalf("part status at expiry: %v", err)
	}
	if st.PhysicalRemaining != 5 || st.ActiveOccupied != 0 || st.Committable != 5 {
		t.Fatalf("canceled stock at expiry = %d/%d/%d, want 5/0/5",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	d, ok := detailByID(st, "c-snap-cancel")
	if !ok || d.Status != CommitmentCanceled || d.RemainingQuantity != 2 {
		t.Fatalf("canceled detail = %+v ok=%v, want canceled remaining=2", d, ok)
	}
	again, err := s.Commitment("c-snap-cancel")
	if err != nil {
		t.Fatalf("re-fetch after part status: %v", err)
	}
	if again.Expired || !again.Canceled {
		t.Fatalf("canceled record confirmed as expired: %+v", again)
	}

	// 到期前的新使用同样一律关闭：取消优先，不扣实物、不改数量。
	if _, err := s.Use("u-snap-cancel-1", "c-snap-cancel", 1, expiry.Add(-time.Nanosecond)); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use canceled commitment: got %v, want ErrCommitmentClosed", err)
	}
}

// TestCommitmentUnknownIDReturnsNotFound 边界二：取回不存在的非空承诺编号
// 返回 ErrNotFound；不产生承诺，也不改变已有库存。
func TestCommitmentUnknownIDReturnsNotFound(t *testing.T) {
	s, expiry, beforeExpiry := snapshotScenarioStore(t)

	if _, err := s.Commitment("c-snap-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fetch unknown commitment: got %v, want ErrNotFound", err)
	}

	// 失败的取回不占用编号：再次取回仍是 ErrNotFound，且该编号始终无法
	// “因为被取回过”而成为一笔承诺。
	if _, err := s.Commitment("c-snap-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-fetch unknown commitment: got %v, want ErrNotFound", err)
	}

	// 已有承诺与库存均不变化：到期前仍是 9/4/5，c-snap 仍未确认到期。
	assertRepoSnapshotUnconfirmed(t, s, "after unknown fetches")
	assertSnapshotStock(t, s, beforeExpiry, 9, 4, 5, "after unknown fetches")

	// 即使未知编号被反复“取回”，仓库确认到期只作用于真实承诺：
	// 在到期时刻核对库存，释放的仍只是 c-snap 的四件，9/0/9。
	atExpiry, err := s.PartStatus("part-snap", expiry)
	if err != nil {
		t.Fatalf("part status at expiry: %v", err)
	}
	if atExpiry.PhysicalRemaining != 9 || atExpiry.ActiveOccupied != 0 || atExpiry.Committable != 9 {
		t.Fatalf("stock at expiry = %d/%d/%d, want 9/0/9",
			atExpiry.PhysicalRemaining, atExpiry.ActiveOccupied, atExpiry.Committable)
	}
	if _, err := s.Commitment("c-snap-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown commitment created by later operations: got %v, want ErrNotFound", err)
	}
}
