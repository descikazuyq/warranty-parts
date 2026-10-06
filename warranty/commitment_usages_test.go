package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障按承诺编号查询成功使用明细（CommitmentUsages）的规则：
// 只收录绑定到该承诺的成功使用记录，按使用编号字符串升序，数量之和等于承诺
// 已用数量；失败提交不产生记录，取消、到期、全部使用不清除记录；查询只读，
// 返回独立副本。

// usagesMainStore 构造主例：part1 初始 20 件，请求 r1 的承诺 cMain 预留 6 件，
// 同一请求下另有承诺 cOther 预留 3 件，请求 r2 的承诺 cRival 预留 2 件（同一
// 备件）。三笔承诺均未取消、未到期。
func usagesMainStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 20); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request r1: %v", err)
	}
	if err := s.SubmitRequest("r2", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request r2: %v", err)
	}
	if _, err := s.Reserve("cMain", "r1", "part1", 6, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cMain: %v", err)
	}
	if _, err := s.Reserve("cOther", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cOther: %v", err)
	}
	if _, err := s.Reserve("cRival", "r2", "part1", 2, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve cRival: %v", err)
	}
	return s
}

// TestCommitmentUsagesMainExample 是用户给定的完整主例：六件承诺 cMain 先用
// U-B 领取两件，再用 U-A 领取一件，随后原样重试 U-B；查询应依次返回 U-A 的
// 一件和 U-B 的两件（按使用编号字符串升序，与领取先后无关），累计三件，与
// 承诺已用数量一致。同一请求的 cOther 与同一备件的 cRival 上的使用不能混入。
func TestCommitmentUsagesMainExample(t *testing.T) {
	s := usagesMainStore(t)

	if _, err := s.Use("U-B", "cMain", 2, nowOK); err != nil {
		t.Fatalf("use U-B 2 from cMain: %v", err)
	}
	if _, err := s.Use("U-A", "cMain", 1, nowOK); err != nil {
		t.Fatalf("use U-A 1 from cMain: %v", err)
	}
	// 原样重试 U-B：取回首次结果，不重复扣减，也不追加明细。
	first, err := s.Use("U-B", "cMain", 2, nowOK)
	if err != nil {
		t.Fatalf("identical retry U-B: %v", err)
	}
	if first != (Usage{ID: "U-B", CommitmentID: "cMain", Quantity: 2}) {
		t.Fatalf("retry U-B = %+v, want {U-B cMain 2}", first)
	}
	// 其他承诺的使用：同请求的 cOther、同备件他请求的 cRival，都不能混入 cMain。
	if _, err := s.Use("U-0", "cOther", 3, nowOK); err != nil {
		t.Fatalf("use U-0 from cOther: %v", err)
	}
	if _, err := s.Use("U-9", "cRival", 2, nowOK); err != nil {
		t.Fatalf("use U-9 from cRival: %v", err)
	}

	us, err := s.CommitmentUsages("cMain")
	if err != nil {
		t.Fatalf("commitment usages cMain: %v", err)
	}
	want := []Usage{
		{ID: "U-A", CommitmentID: "cMain", Quantity: 1},
		{ID: "U-B", CommitmentID: "cMain", Quantity: 2},
	}
	if len(us) != len(want) {
		t.Fatalf("usages = %+v, want %+v", us, want)
	}
	sum := 0
	for i, u := range us {
		if u != want[i] {
			t.Fatalf("usages[%d] = %+v, want %+v", i, u, want[i])
		}
		sum += u.Quantity
	}
	if c := mustCommitment(t, s, "cMain"); sum != c.Used || c.Used != 3 {
		t.Fatalf("sum = %d, cMain used = %d, want both 3", sum, c.Used)
	}

	// 其他承诺的明细只含各自的成功记录。
	if got, err := s.CommitmentUsages("cOther"); err != nil ||
		len(got) != 1 || got[0] != (Usage{ID: "U-0", CommitmentID: "cOther", Quantity: 3}) {
		t.Fatalf("cOther usages = %+v err %v, want only {U-0 cOther 3}", got, err)
	}
	if got, err := s.CommitmentUsages("cRival"); err != nil ||
		len(got) != 1 || got[0] != (Usage{ID: "U-9", CommitmentID: "cRival", Quantity: 2}) {
		t.Fatalf("cRival usages = %+v err %v, want only {U-9 cRival 2}", got, err)
	}
}

// TestCommitmentUsagesFailuresLeaveNoTrace 锁定失败提交不进明细：超量领取、
// 沿用成功编号改数量或改承诺的冲突提交，都不能出现在成功明细中，也不能覆盖
// 原成功记录；尚未成功的编号先失败后成功时，只展示后来成功绑定的承诺与数量。
func TestCommitmentUsagesFailuresLeaveNoTrace(t *testing.T) {
	s := usagesMainStore(t)

	if _, err := s.Use("U-B", "cMain", 2, nowOK); err != nil {
		t.Fatalf("use U-B 2 from cMain: %v", err)
	}
	// 超量失败：cMain 未用 4 件，申请 5 件整次失败，不产生记录。
	if _, err := s.Use("U-X", "cMain", 5, nowOK); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use 5 over unused 4: got %v, want ErrUsageExceeded", err)
	}
	// 沿用成功编号 U-B 改数量、改承诺：均按冲突处理，不覆盖原记录。
	if _, err := s.Use("U-B", "cMain", 3, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("U-B with changed quantity: got %v, want ErrConflict", err)
	}
	if _, err := s.Use("U-B", "cOther", 2, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("U-B with changed commitment: got %v, want ErrConflict", err)
	}
	// 尚未成功的编号 U-X 先因数量不合法失败，再超量失败，最后合法成功：
	// 明细只展示成功绑定的内容。
	if _, err := s.Use("U-X", "cMain", 0, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("U-X zero quantity: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Use("U-X", "cMain", 5, nowOK); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("U-X over use: got %v, want ErrUsageExceeded", err)
	}
	if _, err := s.Use("U-X", "cMain", 1, nowOK); err != nil {
		t.Fatalf("U-X legal use: %v", err)
	}
	// 尚未成功的编号 U-Y 在 cMain 上超量失败后，成功发生在 cOther 上：
	// 记录只属于 cOther，cMain 不得出现 U-Y。
	if _, err := s.Use("U-Y", "cMain", 5, nowOK); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("U-Y over use on cMain: got %v, want ErrUsageExceeded", err)
	}
	if _, err := s.Use("U-Y", "cOther", 1, nowOK); err != nil {
		t.Fatalf("U-Y success on cOther: %v", err)
	}

	us, err := s.CommitmentUsages("cMain")
	if err != nil {
		t.Fatalf("commitment usages cMain: %v", err)
	}
	want := []Usage{
		{ID: "U-B", CommitmentID: "cMain", Quantity: 2},
		{ID: "U-X", CommitmentID: "cMain", Quantity: 1},
	}
	if len(us) != len(want) {
		t.Fatalf("usages = %+v, want %+v", us, want)
	}
	for i, u := range us {
		if u != want[i] {
			t.Fatalf("usages[%d] = %+v, want %+v", i, u, want[i])
		}
	}
	if got, err := s.CommitmentUsages("cOther"); err != nil ||
		len(got) != 1 || got[0] != (Usage{ID: "U-Y", CommitmentID: "cOther", Quantity: 1}) {
		t.Fatalf("cOther usages = %+v err %v, want only {U-Y cOther 1}", got, err)
	}
	// 原成功记录未被失败提交覆盖：U-B 原样重试仍取回两件。
	if got, err := s.Use("U-B", "cMain", 2, nowOK); err != nil || got.Quantity != 2 {
		t.Fatalf("U-B retry after conflicts = %+v err %v, want two-piece record", got, err)
	}
}

// TestCommitmentUsagesSurviveCloseAndCancel 锁定明细记录的是已领取的事实：
// 全部使用、取消、已确认到期都不清除成功记录，释放的未用数量不算领取量；
// 查询已关闭承诺仍成功，关闭后被拒绝的新领取不增加记录。
func TestCommitmentUsagesSurviveCloseAndCancel(t *testing.T) {
	s := usagesMainStore(t)

	if _, err := s.Use("U-1", "cMain", 2, nowOK); err != nil {
		t.Fatalf("use U-1: %v", err)
	}
	// 取消 cMain：释放未用 4 件，但已领取的 2 件记录保留。
	if _, err := s.Cancel("cMain", nowOK); err != nil {
		t.Fatalf("cancel cMain: %v", err)
	}
	// 取消后新领取被拒绝，不增加记录。
	if _, err := s.Use("U-2", "cMain", 1, nowOK); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use on canceled cMain: got %v, want ErrCommitmentClosed", err)
	}
	us, err := s.CommitmentUsages("cMain")
	if err != nil {
		t.Fatalf("usages of canceled cMain: %v", err)
	}
	if len(us) != 1 || us[0] != (Usage{ID: "U-1", CommitmentID: "cMain", Quantity: 2}) {
		t.Fatalf("canceled cMain usages = %+v, want only {U-1 cMain 2}", us)
	}

	// cOther 用满 3 件后确认到期：全部使用与到期都不清除记录。
	if _, err := s.Use("U-3", "cOther", 3, nowOK); err != nil {
		t.Fatalf("use U-3: %v", err)
	}
	// 借新使用按当前时刻确认 cRival 到期（cRival 尚未使用，到期后无记录）。
	if _, err := s.Use("U-4", "cRival", 1, expiryOK); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use on expiring cRival: got %v, want ErrCommitmentClosed", err)
	}
	// cOther 也已到到期时刻，借库存查询确认其到期。
	if _, err := s.PartStatus("part1", expiryOK); err != nil {
		t.Fatalf("part status at expiry: %v", err)
	}
	if c := mustCommitment(t, s, "cOther"); !c.Expired {
		t.Fatalf("cOther should be confirmed expired: %+v", c)
	}
	got, err := s.CommitmentUsages("cOther")
	if err != nil {
		t.Fatalf("usages of expired cOther: %v", err)
	}
	if len(got) != 1 || got[0] != (Usage{ID: "U-3", CommitmentID: "cOther", Quantity: 3}) {
		t.Fatalf("expired cOther usages = %+v, want only {U-3 cOther 3}", got)
	}
	// cRival 到期释放的是未用数量，没有成功领取，明细为空而非把释放量算成领取。
	got, err = s.CommitmentUsages("cRival")
	if err != nil {
		t.Fatalf("usages of expired cRival: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expired cRival usages = %+v, want empty", got)
	}
}

// TestCommitmentUsagesParamAndEmpty 锁定参数与空结果规则：空承诺编号返回
// ErrInvalidParam，非空但不存在的承诺返回 ErrNotFound，已有承诺尚无成功
// 使用时返回成功和空列表。
func TestCommitmentUsagesParamAndEmpty(t *testing.T) {
	s := usagesMainStore(t)

	if _, err := s.CommitmentUsages(""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty commit id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.CommitmentUsages("cMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown commit id: got %v, want ErrNotFound", err)
	}
	us, err := s.CommitmentUsages("cMain")
	if err != nil {
		t.Fatalf("usages of fresh cMain: %v", err)
	}
	if len(us) != 0 {
		t.Fatalf("fresh cMain usages = %+v, want empty list", us)
	}
}

// TestCommitmentUsagesReadOnlyAndIsolated 锁定查询只读且结果独立：查询不扣减
// 实物库存、不释放占用、不按时刻确认到期、不追加预留处理历史；调用方修改
// 返回列表中的编号或数量、删减或追加内容，都不影响真实记录、已用数量、后续
// 查询与原样重试取回的内容。
func TestCommitmentUsagesReadOnlyAndIsolated(t *testing.T) {
	s := usagesMainStore(t)

	if _, err := s.Use("U-1", "cMain", 2, nowOK); err != nil {
		t.Fatalf("use U-1: %v", err)
	}
	before, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("request history r1: %v", err)
	}

	// 查询本身不接收当前时刻，也就不能借查询确认任何承诺到期；查询前后账目、
	// 到期标记与预留历史必须保持原样。
	us, err := s.CommitmentUsages("cMain")
	if err != nil {
		t.Fatalf("commitment usages cMain: %v", err)
	}
	if len(us) != 1 || us[0] != (Usage{ID: "U-1", CommitmentID: "cMain", Quantity: 2}) {
		t.Fatalf("usages = %+v, want only {U-1 cMain 2}", us)
	}

	// 篡改返回结果：改编号、改数量、删首条、追加假记录。
	us[0].ID = "U-hacked"
	us[0].Quantity = 99
	us = append(us[1:], Usage{ID: "U-fake", CommitmentID: "cMain", Quantity: 4})

	// 真实记录、已用数量与后续查询不受篡改影响。
	again, err := s.CommitmentUsages("cMain")
	if err != nil {
		t.Fatalf("commitment usages again: %v", err)
	}
	if len(again) != 1 || again[0] != (Usage{ID: "U-1", CommitmentID: "cMain", Quantity: 2}) {
		t.Fatalf("usages after caller mutation = %+v, want only {U-1 cMain 2}", again)
	}
	if c := mustCommitment(t, s, "cMain"); c.Used != 2 {
		t.Fatalf("cMain used = %d after caller mutation, want 2", c.Used)
	}
	if got, err := s.Use("U-1", "cMain", 2, nowOK); err != nil || got.Quantity != 2 {
		t.Fatalf("U-1 retry after caller mutation = %+v err %v, want two-piece record", got, err)
	}

	// 只读性：实物、占用、可承诺不变；cMain 未因查询被确认到期；历史未追加。
	assertPartAccount(t, s, 18, 9, 9)
	if c := mustCommitment(t, s, "cMain"); c.Expired || c.Canceled {
		t.Fatalf("query confirmed expiry or cancel: %+v", c)
	}
	after, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("request history r1 after query: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("history grew from %d to %d records due to query", len(before), len(after))
	}
}
