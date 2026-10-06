package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障按承诺编号查询成功使用明细的规则：查询只需要承诺编号，返回该
// 承诺全部成功使用记录（使用编号、承诺编号、本次数量）的独立副本，按使用编号
// 字符串升序排列，数量之和等于承诺的已用数量。超量失败、编号冲突等被拒绝的
// 提交不出现在列表中，也不覆盖原成功记录；同一请求或同一备件下其他承诺的
// 使用不混入。承诺全部使用、取消或到期后成功记录仍在，关闭后被拒绝的新使用
// 不追加记录。查询本身不扣库存、不释放占用、不确认到期、不追加历史。

// usageIDs 提取明细列表的使用编号，便于比对次序。
func usageIDs(us []Usage) []string {
	ids := make([]string, len(us))
	for i, u := range us {
		ids[i] = u.ID
	}
	return ids
}

// sumUsages 汇总明细列表的数量。
func sumUsages(us []Usage) int {
	sum := 0
	for _, u := range us {
		sum += u.Quantity
	}
	return sum
}

func TestCommitmentUsagesValidationAndEmpty(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// 空承诺编号 → ErrInvalidParam。
	if _, err := s.CommitmentUsages(""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty commit id: got %v, want ErrInvalidParam", err)
	}
	// 非空但不存在的承诺 → ErrNotFound。
	if _, err := s.CommitmentUsages("cMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown commitment: got %v, want ErrNotFound", err)
	}
	// 已有承诺尚无成功使用：成功且返回空列表（非 nil），不按不存在处理。
	us, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages of fresh commitment: %v", err)
	}
	if us == nil || len(us) != 0 {
		t.Fatalf("expected empty non-nil list, got %v", us)
	}
}

func TestCommitmentUsagesOrderedAndFailuresExcluded(t *testing.T) {
	s := newStore(t)
	// 六件承诺。
	if _, err := s.Reserve("c1", "r1", "part1", 6, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 先用 U-B 领取两件，再用 U-A 领取一件。
	if _, err := s.Use("U-B", "c1", 2, nowOK); err != nil {
		t.Fatalf("use U-B: %v", err)
	}
	if _, err := s.Use("U-A", "c1", 1, nowOK); err != nil {
		t.Fatalf("use U-A: %v", err)
	}
	// 原样重试 U-B：取回旧记录，不新增明细。
	if _, err := s.Use("U-B", "c1", 2, nowOK); err != nil {
		t.Fatalf("retry U-B: %v", err)
	}
	// 超量领取失败：不出现、不覆盖。
	if _, err := s.Use("U-C", "c1", 4, nowOK); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use: got %v, want ErrUsageExceeded", err)
	}
	// 沿用成功编号改数量、改承诺：冲突失败，不出现、不覆盖。
	if _, err := s.Use("U-B", "c1", 3, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict on quantity: got %v, want ErrConflict", err)
	}
	if _, err := s.Use("U-A", "c2", 1, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict on commitment: got %v, want ErrConflict", err)
	}

	// 查询按使用编号字符串升序：U-A 的一件在前，U-B 的两件在后，累计三件。
	us, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages: %v", err)
	}
	if len(us) != 2 {
		t.Fatalf("usages len = %d, want 2: %v", len(us), usageIDs(us))
	}
	if us[0].ID != "U-A" || us[0].CommitmentID != "c1" || us[0].Quantity != 1 {
		t.Fatalf("first usage = %+v, want U-A/c1/1", us[0])
	}
	if us[1].ID != "U-B" || us[1].CommitmentID != "c1" || us[1].Quantity != 2 {
		t.Fatalf("second usage = %+v, want U-B/c1/2", us[1])
	}
	c, _ := s.Commitment("c1")
	if sumUsages(us) != c.Used || c.Used != 3 {
		t.Fatalf("sum = %d, commitment used = %d, want both 3", sumUsages(us), c.Used)
	}
	// 原成功记录未被冲突覆盖：原样重试仍取回旧内容。
	u, err := s.Use("U-B", "c1", 2, nowOK)
	if err != nil || u.Quantity != 2 {
		t.Fatalf("retry after conflicts: %+v err %v", u, err)
	}
}

func TestCommitmentUsagesIsolatedPerCommitment(t *testing.T) {
	s := newStore(t)
	// 同一请求、同一备件下的两笔承诺。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use u1: %v", err)
	}
	if _, err := s.Use("u2", "c2", 1, nowOK); err != nil {
		t.Fatalf("use u2: %v", err)
	}

	us1, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages c1: %v", err)
	}
	if len(us1) != 1 || us1[0].ID != "u1" || us1[0].CommitmentID != "c1" || us1[0].Quantity != 2 {
		t.Fatalf("c1 usages = %+v, want only u1/c1/2", us1)
	}
	us2, err := s.CommitmentUsages("c2")
	if err != nil {
		t.Fatalf("usages c2: %v", err)
	}
	if len(us2) != 1 || us2[0].ID != "u2" || us2[0].CommitmentID != "c2" || us2[0].Quantity != 1 {
		t.Fatalf("c2 usages = %+v, want only u2/c2/1", us2)
	}
}

func TestCommitmentUsagesFailedIdThenSucceeded(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}

	// 尚未成功的使用编号先因数量不合法失败，再因超量失败：都不留记录。
	if _, err := s.Use("u1", "c1", 0, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero quantity: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.Use("u1", "c1", 5, nowOK); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use: got %v, want ErrUsageExceeded", err)
	}
	// 改成合法内容成功：只展示后来成功绑定的承诺与数量。
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use u1: %v", err)
	}
	us, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages c1: %v", err)
	}
	if len(us) != 1 || us[0].ID != "u1" || us[0].Quantity != 2 {
		t.Fatalf("c1 usages = %+v, want only u1/2", us)
	}

	// 另一个编号先失败，之后成功发生在另一笔承诺上：记录只属于那笔承诺。
	if _, err := s.Use("u2", "c1", 9, nowOK); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use u2: got %v, want ErrUsageExceeded", err)
	}
	if _, err := s.Use("u2", "c2", 1, nowOK); err != nil {
		t.Fatalf("use u2 on c2: %v", err)
	}
	us, err = s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages c1 again: %v", err)
	}
	if len(us) != 1 || us[0].ID != "u1" {
		t.Fatalf("c1 usages = %+v, want still only u1", us)
	}
	us2, err := s.CommitmentUsages("c2")
	if err != nil {
		t.Fatalf("usages c2: %v", err)
	}
	if len(us2) != 1 || us2[0].ID != "u2" || us2[0].CommitmentID != "c2" || us2[0].Quantity != 1 {
		t.Fatalf("c2 usages = %+v, want only u2/c2/1", us2)
	}
}

func TestCommitmentUsagesSurviveClose(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use u1: %v", err)
	}
	// 取消已部分使用的承诺：成功记录不消失，释放的未用数量不算领取量。
	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// 关闭后被拒绝的新领取不增加记录。
	if _, err := s.Use("u2", "c1", 1, nowOK); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use after cancel: got %v, want ErrCommitmentClosed", err)
	}
	us, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages after cancel: %v", err)
	}
	if len(us) != 1 || us[0].ID != "u1" || us[0].Quantity != 2 {
		t.Fatalf("usages after cancel = %+v, want only u1/2", us)
	}

	// 全部使用并确认到期的承诺：查询仍成功，成功记录原样保留。
	s2 := newStore(t)
	if _, err := s2.Reserve("c1", "r1", "part1", 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s2.Use("u1", "c1", 3, nowOK); err != nil {
		t.Fatalf("use u1: %v", err)
	}
	// 借库存查询确认到期。
	if _, err := s2.PartStatus("part1", expiryOK); err != nil {
		t.Fatalf("part status: %v", err)
	}
	c, _ := s2.Commitment("c1")
	if !c.Expired {
		t.Fatal("commitment should be confirmed expired")
	}
	// 关闭后被拒绝的新领取不增加记录。
	if _, err := s2.Use("u2", "c1", 1, expiryOK); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use after expiry: got %v, want ErrCommitmentClosed", err)
	}
	us, err = s2.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages after expiry: %v", err)
	}
	if len(us) != 1 || us[0].ID != "u1" || us[0].Quantity != 3 {
		t.Fatalf("usages after expiry = %+v, want only u1/3", us)
	}
}

func TestCommitmentUsagesNoSideEffects(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	before, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status before: %v", err)
	}
	hBefore, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history before: %v", err)
	}

	if _, err := s.CommitmentUsages("c1"); err != nil {
		t.Fatalf("usages: %v", err)
	}

	// 查询不扣减实物库存、不释放占用、不确认到期。
	after, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status after: %v", err)
	}
	if before.PhysicalRemaining != after.PhysicalRemaining ||
		before.ActiveOccupied != after.ActiveOccupied ||
		before.Committable != after.Committable ||
		len(before.Details) != len(after.Details) {
		t.Fatalf("query changed stock account: before %+v after %+v", before, after)
	}
	c, _ := s.Commitment("c1")
	if c.Expired || c.Canceled || c.Used != 2 {
		t.Fatalf("query changed commitment: %+v", c)
	}
	// 查询不追加预留处理历史。
	hAfter, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history after: %v", err)
	}
	if len(hAfter) != len(hBefore) {
		t.Fatalf("query appended history: before %d after %d", len(hBefore), len(hAfter))
	}
}

func TestCommitmentUsagesIndependentResult(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use u1: %v", err)
	}
	if _, err := s.Use("u2", "c1", 1, nowOK); err != nil {
		t.Fatalf("use u2: %v", err)
	}

	us, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages: %v", err)
	}
	// 修改返回副本：改编号、改数量、删减、追加。
	us[0].ID = "hacked"
	us[0].CommitmentID = "hacked"
	us[0].Quantity = 99
	us = append(us[1:], Usage{ID: "u3", CommitmentID: "c1", Quantity: 7})

	// 真实记录、已用数量与后续查询不受影响。
	fresh, err := s.CommitmentUsages("c1")
	if err != nil {
		t.Fatalf("usages again: %v", err)
	}
	if len(fresh) != 2 || fresh[0].ID != "u1" || fresh[0].Quantity != 2 ||
		fresh[1].ID != "u2" || fresh[1].Quantity != 1 {
		t.Fatalf("stored usages changed: %+v", fresh)
	}
	c, _ := s.Commitment("c1")
	if c.Used != 3 {
		t.Fatalf("commitment used changed: %d", c.Used)
	}
	// 成功使用编号原样重试取回的内容不受影响。
	u, err := s.Use("u1", "c1", 2, nowOK)
	if err != nil || u.Quantity != 2 || u.CommitmentID != "c1" {
		t.Fatalf("retry after mutation: %+v err %v", u, err)
	}
}
