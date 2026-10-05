package warranty

import (
	"errors"
	"testing"
	"time"
)

// TestUseConfirmsOnlyItsOwnCommitment 是承诺使用功能的回归保障：一次新使用
// 只能确认它指定的那笔承诺到期。同一种备件、同一个请求、同一到期时刻的其他
// 承诺不受牵连。
//
// 场景：备件初始十二件，两个合格请求取得三笔同一时刻到期的承诺——r1 预留
// 五件（c5）和三件（c3），r2 预留四件（c4）。c5 到期前成功使用两件后，实物
// 剩十件、有效占用十件、可承诺为零。到期时刻用新使用编号向 c5 申请一件：
// 只确认 c5 到期并拒绝，c3、c4 保持可用。
func TestUseConfirmsOnlyItsOwnCommitment(t *testing.T) {
	s := expStore(t, 12)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)

	// 三笔承诺同一时刻到期：r1 五件、r1 三件、r2 四件。
	if _, err := s.Reserve("c5", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve c5: %v", err)
	}
	if _, err := s.Reserve("c3", "r1", "part1", 3, noon, before); err != nil {
		t.Fatalf("reserve c3: %v", err)
	}
	if _, err := s.Reserve("c4", "r2", "part1", 4, noon, before); err != nil {
		t.Fatalf("reserve c4: %v", err)
	}
	// c5 到期前成功使用两件：实物 12-2=10，占用 3+3+4=10，可承诺为零。
	if _, err := s.Use("u1", "c5", 2, before); err != nil {
		t.Fatalf("use c5: %v", err)
	}
	st0, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st0.PhysicalRemaining != 10 || st0.ActiveOccupied != 10 || st0.Committable != 0 {
		t.Fatalf("stock after partial use = %+v, want phys=10 occupied=10 committable=0", st0)
	}

	// 到期时刻用尚未成功过的使用编号向 c5 申请一件：失败且没有成功使用结果，
	// 已用数量仍为两件；本次只确认 c5 一笔到期。
	if _, err := s.Use("u-exp", "c5", 1, noon); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use c5 at expiry: got %v, want ErrCommitmentClosed", err)
	}
	c5, err := s.Commitment("c5")
	if err != nil {
		t.Fatalf("commitment c5: %v", err)
	}
	// 原定数量、请求归属和到期时刻仍可查询，状态是 expired 而非 canceled。
	if c5.Quantity != 5 || c5.Used != 2 || c5.RequestID != "r1" || !c5.Expiry.Equal(noon) {
		t.Fatalf("c5 record mutated by failed use: %+v", c5)
	}
	if !c5.Expired || c5.Canceled {
		t.Fatalf("c5 flags = %+v, want expired and not canceled", c5)
	}
	if c5.Status(before) != CommitmentExpired {
		t.Fatalf("c5 status at earlier time = %q, want expired", c5.Status(before))
	}

	// 以到期前时刻查看：c5 仍是 expired，同备件、同请求、同到期时刻的另外
	// 两笔仍是 active；实物十件，占用 3+4=7，可承诺三件。
	st1, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	if st1.PhysicalRemaining != 10 || st1.ActiveOccupied != 7 || st1.Committable != 3 {
		t.Fatalf("stock after c5 expiry = %+v, want phys=10 occupied=7 committable=3", st1)
	}
	if d, _ := detailByID(st1, "c5"); d.Status != CommitmentExpired ||
		d.OriginalQuantity != 5 || d.UsedQuantity != 2 || d.RemainingQuantity != 3 {
		t.Fatalf("c5 detail = %+v, want expired 5/2/3", d)
	}
	if d, _ := detailByID(st1, "c3"); d.Status != CommitmentActive || d.RemainingQuantity != 3 {
		t.Fatalf("c3 closed by sibling use: %+v, want active/remaining=3", d)
	}
	if d, _ := detailByID(st1, "c4"); d.Status != CommitmentActive || d.RemainingQuantity != 4 {
		t.Fatalf("c4 closed by sibling use: %+v, want active/remaining=4", d)
	}
	view, err := s.RequestView("r1", before)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	statusByID := map[string]CommitmentStatus{}
	for _, d := range view.Commitments {
		statusByID[d.CommitmentID] = d.Status
	}
	if statusByID["c5"] != CommitmentExpired || statusByID["c3"] != CommitmentActive {
		t.Fatalf("r1 view statuses = %v, want c5 expired and c3 active", statusByID)
	}

	// c3 与已到期承诺属于同一请求，仍可用：到期前时刻新使用一件成功，实物与
	// 有效占用各减一件，可承诺仍为三件。沿用刚才失败的使用编号，也证明那次
	// 失败没有留下成功使用结果、没有占用编号。
	if _, err := s.Use("u-exp", "c3", 1, before); err != nil {
		t.Fatalf("use c3 before expiry: %v", err)
	}
	st2, _ := s.PartStatus("part1", before)
	if st2.PhysicalRemaining != 9 || st2.ActiveOccupied != 6 || st2.Committable != 3 {
		t.Fatalf("stock after c3 use = %+v, want phys=9 occupied=6 committable=3", st2)
	}

	// 再向已确认到期的 c5 提出新使用，即使时刻早于到期也继续返回关闭错误，
	// 不扣库存。
	if _, err := s.Use("u-late", "c5", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use confirmed-expired c5 at earlier time: got %v, want ErrCommitmentClosed", err)
	}
	st3, _ := s.PartStatus("part1", before)
	if st3.PhysicalRemaining != 9 || st3.ActiveOccupied != 6 || st3.Committable != 3 {
		t.Fatalf("rejected use changed stock: %+v", st3)
	}
	if c, _ := s.Commitment("c5"); c.Used != 2 {
		t.Fatalf("rejected use changed c5 used: %+v", c)
	}

	// 数量无效而提前失败的区别：尚未确认到期的 c4 在到期时刻收到零数量的新
	// 使用，返回 ErrInvalidParam，且不借本次时刻确认任何承诺到期。
	if _, err := s.Use("u-zero", "c4", 0, noon); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero-quantity use at expiry: got %v, want ErrInvalidParam", err)
	}
	st4, _ := s.PartStatus("part1", before)
	if st4.PhysicalRemaining != 9 || st4.ActiveOccupied != 6 || st4.Committable != 3 {
		t.Fatalf("invalid-param use changed stock: %+v, want phys=9 occupied=6 committable=3", st4)
	}
	if d, _ := detailByID(st4, "c4"); d.Status != CommitmentActive || d.RemainingQuantity != 4 {
		t.Fatalf("c4 confirmed by invalid-param use: %+v, want active/remaining=4", d)
	}
	if d, _ := detailByID(st4, "c3"); d.Status != CommitmentActive || d.RemainingQuantity != 2 {
		t.Fatalf("c3 confirmed by invalid-param use: %+v, want active/remaining=2", d)
	}
	// 零数量失败也不占用使用编号：修正内容后同一编号仍可提交。
	if _, err := s.Use("u-zero", "c4", 1, before); err != nil {
		t.Fatalf("retry corrected use after invalid param: %v", err)
	}
	st5, _ := s.PartStatus("part1", before)
	if st5.PhysicalRemaining != 8 || st5.ActiveOccupied != 5 || st5.Committable != 3 {
		t.Fatalf("stock after corrected c4 use = %+v, want phys=8 occupied=5 committable=3", st5)
	}
}
