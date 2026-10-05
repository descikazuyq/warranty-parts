package warranty

import (
	"errors"
	"testing"
	"time"
)

// TestUseAtExpiryConfirmsOnlyItsOwnCommitment 验证一次新使用只确认它指定的那笔
// 承诺到期：同一备件、同一请求、同一到期时刻的其他承诺不被顺带关闭。主例：
// 备件十二件，请求一预留五件和三件、请求二预留四件，三笔同一时刻到期；五件
// 承诺到期前用掉两件后，到期时刻对它的新使用失败并只释放它的三件未用占用，
// 其余两笔在到期前时刻仍 active、可继续使用。
func TestUseAtExpiryConfirmsOnlyItsOwnCommitment(t *testing.T) {
	s := expStore(t, 12)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)

	// 三笔承诺同一时刻到期：r1 五件与三件，r2 四件。
	if _, err := s.Reserve("c5", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve c5: %v", err)
	}
	if _, err := s.Reserve("c3", "r1", "part1", 3, noon, before); err != nil {
		t.Fatalf("reserve c3: %v", err)
	}
	if _, err := s.Reserve("c4", "r2", "part1", 4, noon, before); err != nil {
		t.Fatalf("reserve c4: %v", err)
	}
	// 五件承诺到期前成功使用两件：实物十件，有效占用十件，可承诺为零。
	if _, err := s.Use("u1", "c5", 2, before); err != nil {
		t.Fatalf("use c5 before expiry: %v", err)
	}
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 10 || st.Committable != 0 {
		t.Fatalf("after partial use: phys=%d occupied=%d committable=%d, want 10/10/0",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 到期时刻用新使用编号向五件承诺再申请一件：失败且没有成功使用结果。
	u, err := s.Use("u-late", "c5", 1, noon)
	if !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use c5 at expiry: got %v, want ErrCommitmentClosed", err)
	}
	if u != (Usage{}) {
		t.Fatalf("failed use returned a usage: %+v", u)
	}
	// 已用数量仍为两件，三件未用占用永久释放；原定数量、请求归属与到期时刻
	// 仍可查询，状态 expired 而非 canceled。
	c5, err := s.Commitment("c5")
	if err != nil {
		t.Fatalf("commitment c5: %v", err)
	}
	if c5.Used != 2 || !c5.Expired || c5.Canceled ||
		c5.Quantity != 5 || c5.RequestID != "r1" || !c5.Expiry.Equal(noon) {
		t.Fatalf("c5 after failed use: %+v, want used=2 expired quantity=5 request=r1", c5)
	}
	// 同备件、同请求、同到期时刻的另外两笔不被顺带确认。
	for _, id := range []string{"c3", "c4"} {
		c, _ := s.Commitment(id)
		if c.Expired || c.Canceled || c.Used != 0 {
			t.Fatalf("%s closed by c5's use: %+v", id, c)
		}
	}

	// 回退到到期前时刻查看：c5 仍 expired，c3、c4 仍 active；
	// 实物十件，有效占用七件（3+4），可承诺三件。
	st, err = s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 7 || st.Committable != 3 {
		t.Fatalf("rollback stock: phys=%d occupied=%d committable=%d, want 10/7/3",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	d5, _ := detailByID(st, "c5")
	if d5.Status != CommitmentExpired || d5.OriginalQuantity != 5 ||
		d5.UsedQuantity != 2 || d5.RemainingQuantity != 3 || !d5.Expiry.Equal(noon) {
		t.Fatalf("c5 detail at rollback: %+v, want expired 5/2/3", d5)
	}
	for _, id := range []string{"c3", "c4"} {
		d, _ := detailByID(st, id)
		if d.Status != CommitmentActive {
			t.Fatalf("%s at rollback = %q, want active", id, d.Status)
		}
	}
	// 请求视图同样：r1 下 c5 expired、c3 active，r2 下 c4 active。
	v1, err := s.RequestView("r1", before)
	if err != nil {
		t.Fatalf("request view r1: %v", err)
	}
	statusByID := map[string]CommitmentStatus{}
	for _, d := range v1.Commitments {
		statusByID[d.CommitmentID] = d.Status
	}
	if statusByID["c5"] != CommitmentExpired || statusByID["c3"] != CommitmentActive {
		t.Fatalf("r1 view at rollback: %v, want c5 expired c3 active", statusByID)
	}
	v2, err := s.RequestView("r2", before)
	if err != nil {
		t.Fatalf("request view r2: %v", err)
	}
	if len(v2.Commitments) != 1 || v2.Commitments[0].Status != CommitmentActive {
		t.Fatalf("r2 view at rollback: %+v, want c4 active", v2.Commitments)
	}

	// 三件承诺与已到期承诺属于同一请求，仍可在到期前时刻新使用一件：
	// 实物与有效占用各减一件，可承诺数量不变。
	if _, err := s.Use("u-c3", "c3", 1, before); err != nil {
		t.Fatalf("use c3 (same request as expired c5): %v", err)
	}
	st, err = s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status after c3 use: %v", err)
	}
	if st.PhysicalRemaining != 9 || st.ActiveOccupied != 6 || st.Committable != 3 {
		t.Fatalf("after c3 use: phys=%d occupied=%d committable=%d, want 9/6/3",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 再向已确认到期的 c5 提出新使用，即使时刻早于到期也继续返回关闭错误，
	// 不扣库存；沿用失败编号 u-late 的提交也没有成功结果可取，同样被拒绝。
	for _, usageID := range []string{"u-again", "u-late"} {
		if _, err := s.Use(usageID, "c5", 1, before); !errors.Is(err, ErrCommitmentClosed) {
			t.Fatalf("use %q on confirmed-expired c5 at earlier time: got %v, want ErrCommitmentClosed",
				usageID, err)
		}
	}
	st, err = s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status after rejected uses: %v", err)
	}
	if st.PhysicalRemaining != 9 || st.ActiveOccupied != 6 || st.Committable != 3 {
		t.Fatalf("rejected uses changed stock: phys=%d occupied=%d committable=%d, want 9/6/3",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	c5, _ = s.Commitment("c5")
	if c5.Used != 2 {
		t.Fatalf("rejected uses changed c5 used: %+v", c5)
	}
}

// TestZeroQuantityUseAtExpiryConfirmsNothing 验证数量无效而提前失败的新使用
// 不确认任何承诺到期：尚未确认到期的承诺在到期时刻收到零数量使用，返回
// ErrInvalidParam，随后按到期前时刻查看仍保留原有占用、承诺仍可使用。
func TestZeroQuantityUseAtExpiryConfirmsNothing(t *testing.T) {
	s := expStore(t, 12)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)

	if _, err := s.Reserve("c1", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r2", "part1", 4, noon, before); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}

	// 到期时刻对尚未确认到期的承诺提交零数量新使用：参数非法，提前失败。
	if _, err := s.Use("u-zero", "c1", 0, noon); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero-quantity use at expiry: got %v, want ErrInvalidParam", err)
	}
	// 不因此确认任何承诺到期：同备件两笔承诺都保持未确认。
	for _, id := range []string{"c1", "c2"} {
		c, _ := s.Commitment(id)
		if c.Expired || c.Canceled {
			t.Fatalf("%s confirmed by invalid-param use: %+v", id, c)
		}
	}
	// 按到期前时刻查看：原有占用保留，两笔均 active，可正常新使用。
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	if st.PhysicalRemaining != 12 || st.ActiveOccupied != 9 || st.Committable != 3 {
		t.Fatalf("invalid-param use released occupancy: phys=%d occupied=%d committable=%d, want 12/9/3",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	for _, id := range []string{"c1", "c2"} {
		if d, _ := detailByID(st, id); d.Status != CommitmentActive {
			t.Fatalf("%s at rollback = %q, want active", id, d.Status)
		}
	}
	if _, err := s.Use("u-ok", "c1", 1, before); err != nil {
		t.Fatalf("c1 closed by zero-quantity use at expiry: %v", err)
	}
}
