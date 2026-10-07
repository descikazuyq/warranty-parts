package warranty

import (
	"errors"
	"testing"
	"time"
)

// TestCommitmentFetchAndStatusDisplay 回归主场景：产品在保、故障代码未命中
// 除外清单。初始库存十二件，成功预留七件，到期前领取三件。调用方按编号取回
// 承诺记录后，用手中记录的 Status 按某个时刻查看状态只是按手中资料显示结果：
// 既不确认仓库中的到期，也不释放尚未使用的备件占用。只有现有库存查询入口才
// 确认到期；确认后重新取回的记录带已确认标记，而此前保存的旧记录保持取回时
// 的事实，不随仓库变化自动更新。
func TestCommitmentFetchAndStatusDisplay(t *testing.T) {
	s := expStore(t, 12)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)

	// 成功预留七件，到期前领取三件。
	if _, err := s.Reserve("c1", "r1", "part1", 7, noon, before); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 3, before); err != nil {
		t.Fatalf("use c1: %v", err)
	}

	// 按编号取回记录：原数量七件、已用三件、未用四件，到期确认标记为否。
	rec, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("fetch c1: %v", err)
	}
	if rec.Quantity != 7 || rec.Used != 3 || rec.Unused() != 4 || rec.Expired || rec.Canceled {
		t.Fatalf("fetched record = %+v, want quantity=7 used=3 unused=4 unexpired uncanceled", rec)
	}

	// 对手中记录按时刻查看：到期前一纳秒为 active，恰好到期及之后为 expired。
	if got := rec.Status(noon.Add(-time.Nanosecond)); got != CommitmentActive {
		t.Fatalf("status one nanosecond before expiry = %q, want active", got)
	}
	if got := rec.Status(noon); got != CommitmentExpired {
		t.Fatalf("status at expiry = %q, want expired", got)
	}
	if got := rec.Status(after); got != CommitmentExpired {
		t.Fatalf("status after expiry = %q, want expired", got)
	}

	// 显示操作不改变手中记录。
	if rec.Quantity != 7 || rec.Used != 3 || rec.Unused() != 4 || rec.Expired || rec.Canceled {
		t.Fatalf("display mutated the record in hand: %+v", rec)
	}
	// 也不改变重新取回的仓库记录：到期仍未被确认。
	refetched, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("refetch c1: %v", err)
	}
	if refetched.Expired {
		t.Fatalf("displaying expired confirmed expiry in the store: %+v", refetched)
	}

	// 按到期前的时刻核对库存：实物剩余九件、有效占用四件、可承诺五件；
	// 此前显示过 expired 不得提前释放四件占用。
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status before expiry: %v", err)
	}
	if st.PhysicalRemaining != 9 || st.ActiveOccupied != 4 || st.Committable != 5 {
		t.Fatalf("stock before expiry = phys=%d occupied=%d committable=%d, want 9/4/5",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if d, _ := detailByID(st, "c1"); d.Status != CommitmentActive {
		t.Fatalf("c1 detail before expiry = %q, want active", d.Status)
	}

	// 在承诺恰好到期的时刻通过库存查询确认到期：只释放未用四件，实物仍为
	// 九件，有效占用为零、可承诺九件；明细保留七件原数量、三件已用、四件未用。
	st2, err := s.PartStatus("part1", noon)
	if err != nil {
		t.Fatalf("part status at expiry: %v", err)
	}
	if st2.PhysicalRemaining != 9 || st2.ActiveOccupied != 0 || st2.Committable != 9 {
		t.Fatalf("stock at expiry = phys=%d occupied=%d committable=%d, want 9/0/9",
			st2.PhysicalRemaining, st2.ActiveOccupied, st2.Committable)
	}
	d, ok := detailByID(st2, "c1")
	if !ok || d.Status != CommitmentExpired || d.OriginalQuantity != 7 ||
		d.UsedQuantity != 3 || d.RemainingQuantity != 4 {
		t.Fatalf("c1 detail at expiry = %+v ok=%v, want expired 7/3/4", d, ok)
	}

	// 重新取回的记录带有已确认到期标记，即使按到期前的时刻查看也持续
	// 显示 expired。
	confirmed, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("fetch confirmed c1: %v", err)
	}
	if !confirmed.Expired {
		t.Fatalf("re-fetched record missing confirmed expiry: %+v", confirmed)
	}
	if got := confirmed.Status(before); got != CommitmentExpired {
		t.Fatalf("confirmed record status at earlier time = %q, want expired", got)
	}

	// 此前保存的旧记录仍保持未确认标记，按较早时刻查看仍显示 active，
	// 不随仓库变化自动更新；两份记录的差异体现各自取回时的事实。
	if rec.Expired {
		t.Fatalf("old record auto-updated to confirmed expiry: %+v", rec)
	}
	if got := rec.Status(before); got != CommitmentActive {
		t.Fatalf("old record status at earlier time = %q, want active", got)
	}
	if got := rec.Status(noon.Add(-time.Nanosecond)); got != CommitmentActive {
		t.Fatalf("old record status one nanosecond before expiry = %q, want active", got)
	}
}

// TestCanceledCommitmentStatusDisplay 保留边界：已取消承诺取回后，无论按
// 到期前、到期时还是之后查看，都显示 canceled，取消优先于到期。
func TestCanceledCommitmentStatusDisplay(t *testing.T) {
	s := expStore(t, 12)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)

	if _, err := s.Reserve("c1", "r1", "part1", 7, noon, before); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 3, before); err != nil {
		t.Fatalf("use c1: %v", err)
	}
	if _, err := s.Cancel("c1", before); err != nil {
		t.Fatalf("cancel c1: %v", err)
	}

	rec, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("fetch c1: %v", err)
	}
	if !rec.Canceled || rec.Expired {
		t.Fatalf("canceled record = %+v, want canceled and not expired", rec)
	}
	for _, tm := range []time.Time{before, noon, after} {
		if got := rec.Status(tm); got != CommitmentCanceled {
			t.Fatalf("canceled record status at %v = %q, want canceled", tm, got)
		}
	}

	// 晚于到期时刻的库存查询也不会把已取消记录确认成 expired。
	if _, err := s.PartStatus("part1", after); err != nil {
		t.Fatalf("part status after expiry: %v", err)
	}
	rec2, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("refetch c1: %v", err)
	}
	if !rec2.Canceled || rec2.Expired {
		t.Fatalf("canceled record confirmed expired by query: %+v", rec2)
	}
	for _, tm := range []time.Time{before, noon, after} {
		if got := rec2.Status(tm); got != CommitmentCanceled {
			t.Fatalf("canceled record status at %v after query = %q, want canceled", tm, got)
		}
	}
}

// TestCommitmentFetchNotFound 保留边界：取回不存在的非空承诺编号返回
// ErrNotFound，不产生承诺，也不改变已有库存。
func TestCommitmentFetchNotFound(t *testing.T) {
	s := expStore(t, 12)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)

	if _, err := s.Reserve("c1", "r1", "part1", 7, noon, before); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Use("u1", "c1", 3, before); err != nil {
		t.Fatalf("use c1: %v", err)
	}

	if _, err := s.Commitment("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fetch missing commitment: got %v, want ErrNotFound", err)
	}
	// 再次取回仍不存在：失败的取回没有产生承诺。
	if _, err := s.Commitment("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refetch missing commitment: got %v, want ErrNotFound", err)
	}

	// 已有库存与承诺不受影响：实物九件、占用四件、可承诺五件。
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 9 || st.ActiveOccupied != 4 || st.Committable != 5 {
		t.Fatalf("stock after failed fetch = phys=%d occupied=%d committable=%d, want 9/4/5",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if len(st.Details) != 1 || st.Details[0].CommitmentID != "c1" {
		t.Fatalf("failed fetch created a commitment: %+v", st.Details)
	}
	rec, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("fetch c1: %v", err)
	}
	if rec.Quantity != 7 || rec.Used != 3 || rec.Expired || rec.Canceled {
		t.Fatalf("existing commitment changed by failed fetch: %+v", rec)
	}
}
