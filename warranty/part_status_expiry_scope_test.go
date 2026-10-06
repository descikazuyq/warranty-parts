package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障按备件查看库存与承诺明细（PartStatus）确认到期的范围：查询
// 只确认被查看备件自己的承诺，同一保修请求、同一到期时刻落在其他备件上的
// 承诺不能被一起关闭；被查看备件下所有已到期的承诺（无论归属哪个请求）要
// 一次全部确认，到期更晚的保持有效。空编号与未知备件的失败查询不确认任何
// 承诺、不改变数量、不创建备件记录。既有按请求查询（RequestView）与使用
// （Use）的到期确认范围由各自测试保障，不在本文件重复。

// psxPartA 等是本文件专用的编号，避免与其他测试文件的编号混用。
const (
	psxPartA = "psx-partA"
	psxPartB = "psx-partB"
)

// 共同到期时刻为第二十天，更晚的到期时刻为第四十天，均在六十天保修期内。
var (
	psxExpiry = t0.Add(20 * day)
	psxLater  = t0.Add(40 * day)
	psxBefore = psxExpiry.Add(-time.Minute)
)

// psxStore 构造一个产品（保修 60 天）、甲备件库存十二件、乙备件库存六件和
// 两个在保修期内始终合格的请求。
func psxStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("psx-p1", t0, 60, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(psxPartA, 12); err != nil {
		t.Fatalf("register part A: %v", err)
	}
	if err := s.RegisterPart(psxPartB, 6); err != nil {
		t.Fatalf("register part B: %v", err)
	}
	for _, id := range []string{"psx-r1", "psx-r2"} {
		if err := s.SubmitRequest(id, "psx-p1", "FAULTY"); err != nil {
			t.Fatalf("submit %s: %v", id, err)
		}
	}
	return s
}

// psxReserveAll 按用户给出的主例布设承诺：甲备件上第一张请求预留五件（已
// 使用两件）、第二张请求预留三件，两笔同一时刻到期，另有一笔两件到期更晚；
// 乙备件上第一张请求还预留四件（已使用一件），与甲的前两笔同时到期。所有
// 预留与使用都在到期前、保修期内完成，承诺均未取消。
func psxReserveAll(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Reserve("psx-c1", "psx-r1", psxPartA, 5, psxExpiry, psxBefore); err != nil {
		t.Fatalf("reserve psx-c1: %v", err)
	}
	if _, err := s.Use("psx-u1", "psx-c1", 2, psxBefore); err != nil {
		t.Fatalf("use psx-c1: %v", err)
	}
	if _, err := s.Reserve("psx-c2", "psx-r2", psxPartA, 3, psxExpiry, psxBefore); err != nil {
		t.Fatalf("reserve psx-c2: %v", err)
	}
	if _, err := s.Reserve("psx-c3", "psx-r2", psxPartA, 2, psxLater, psxBefore); err != nil {
		t.Fatalf("reserve psx-c3: %v", err)
	}
	// 与 psx-c1 同属第一张请求、与甲的前两笔同一时刻到期，但在乙备件上。
	if _, err := s.Reserve("psx-c4", "psx-r1", psxPartB, 4, psxExpiry, psxBefore); err != nil {
		t.Fatalf("reserve psx-c4: %v", err)
	}
	if _, err := s.Use("psx-u2", "psx-c4", 1, psxBefore); err != nil {
		t.Fatalf("use psx-c4: %v", err)
	}
}

// psxAccount 断言某次 PartStatus 的库存三项为期望值。
func psxAccount(t *testing.T, st *PartStatus, physical, occupied, committable int) {
	t.Helper()
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock account phys=%d occupied=%d committable=%d, want phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// psxDetail 断言指定承诺的明细完整反映归属、原定、已用、未用、状态与到期时刻。
func psxDetail(t *testing.T, st *PartStatus, commitID, requestID, partID string,
	original, used, remaining int, status CommitmentStatus, expiry time.Time) {
	t.Helper()
	d, ok := detailByID(st, commitID)
	if !ok {
		t.Fatalf("detail %q missing from %s view", commitID, st.PartID)
	}
	if d.RequestID != requestID || d.PartID != partID ||
		d.OriginalQuantity != original || d.UsedQuantity != used || d.RemainingQuantity != remaining ||
		d.Status != status || !d.Expiry.Equal(expiry) {
		t.Fatalf("detail %q: req=%s part=%s orig=%d used=%d rem=%d status=%s expiry=%v; "+
			"want req=%s part=%s orig=%d used=%d rem=%d status=%s expiry=%v",
			commitID, d.RequestID, d.PartID, d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status, d.Expiry,
			requestID, partID, original, used, remaining, status, expiry)
	}
}

// TestPartStatusConfirmsOnlyViewedPartExpiries 对应用户给出的主例：恰好到达
// 共同到期时刻只查看甲备件，甲的两笔到期承诺被确认（各释放三件未用占用，
// 已使用的两件实物不回库），到期更晚的一笔保持有效；乙备件上同属第一张
// 请求、同一时刻到期的承诺不被一起关闭。
func TestPartStatusConfirmsOnlyViewedPartExpiries(t *testing.T) {
	s := psxStore(t)
	psxReserveAll(t, s)

	// 恰好到达共同到期时刻，只查看甲：实物剩余十件（已用两件不回库），
	// 有效占用只剩到期更晚的两件，可承诺八件。
	st, err := s.PartStatus(psxPartA, psxExpiry)
	if err != nil {
		t.Fatalf("part A status at expiry: %v", err)
	}
	psxAccount(t, st, 10, 2, 8)
	// 明细只含甲的三笔承诺，按承诺编号排序，各自归属、数量与状态齐全。
	if len(st.Details) != 3 {
		t.Fatalf("part A details = %d %+v, want 3", len(st.Details), st.Details)
	}
	for i, id := range []string{"psx-c1", "psx-c2", "psx-c3"} {
		if st.Details[i].CommitmentID != id {
			t.Fatalf("details order at %d = %q, want %q", i, st.Details[i].CommitmentID, id)
		}
		if st.Details[i].PartID != psxPartA {
			t.Fatalf("detail %q leaked part %q into part A view",
				st.Details[i].CommitmentID, st.Details[i].PartID)
		}
	}
	psxDetail(t, st, "psx-c1", "psx-r1", psxPartA, 5, 2, 3, CommitmentExpired, psxExpiry)
	psxDetail(t, st, "psx-c2", "psx-r2", psxPartA, 3, 0, 3, CommitmentExpired, psxExpiry)
	psxDetail(t, st, "psx-c3", "psx-r2", psxPartA, 2, 0, 2, CommitmentActive, psxLater)
	if _, ok := detailByID(st, "psx-c4"); ok {
		t.Fatalf("part B commitment leaked into part A view: %+v", st.Details)
	}

	// 仓库事实：甲的前两笔已确认到期，到期更晚的一笔与乙的承诺都未确认；
	// 实物库存不因确认到期而回补。
	for _, tc := range []struct {
		id      string
		expired bool
	}{
		{"psx-c1", true}, {"psx-c2", true}, {"psx-c3", false}, {"psx-c4", false},
	} {
		c, err := s.Commitment(tc.id)
		if err != nil {
			t.Fatalf("commitment %s: %v", tc.id, err)
		}
		if c.Expired != tc.expired || c.Canceled {
			t.Fatalf("%s after part A view: Expired=%v Canceled=%v, want Expired=%v",
				tc.id, c.Expired, c.Canceled, tc.expired)
		}
	}
	if p, _ := s.Part(psxPartA); p.Stock != 10 {
		t.Fatalf("part A stock = %d, want 10 (used pieces must not return)", p.Stock)
	}
	if p, _ := s.Part(psxPartB); p.Stock != 5 {
		t.Fatalf("part B stock = %d, want 5 (untouched by part A view)", p.Stock)
	}

	// 这次查询之后，即使传入到期前的时刻，甲的前两笔仍已到期：不再计入
	// 占用、持续显示 expired，也不能接受新的使用；到期更晚的一笔保持有效。
	rollback, err := s.PartStatus(psxPartA, psxBefore)
	if err != nil {
		t.Fatalf("part A status at earlier time: %v", err)
	}
	psxAccount(t, rollback, 10, 2, 8)
	psxDetail(t, rollback, "psx-c1", "psx-r1", psxPartA, 5, 2, 3, CommitmentExpired, psxExpiry)
	psxDetail(t, rollback, "psx-c2", "psx-r2", psxPartA, 3, 0, 3, CommitmentExpired, psxExpiry)
	psxDetail(t, rollback, "psx-c3", "psx-r2", psxPartA, 2, 0, 2, CommitmentActive, psxLater)
	for _, id := range []string{"psx-c1", "psx-c2"} {
		if _, err := s.Use("psx-u-closed-"+id, id, 1, psxBefore); !errors.Is(err, ErrCommitmentClosed) {
			t.Fatalf("use %s after confirmation: got %v, want ErrCommitmentClosed", id, err)
		}
	}
	// 到期更晚的一笔在到期前时刻仍可使用：用一件后实物九件、占用一件、
	// 可承诺八件。
	if _, err := s.Use("psx-u3", "psx-c3", 1, psxBefore); err != nil {
		t.Fatalf("later-expiring commitment closed by part A view: %v", err)
	}
	afterUse, err := s.PartStatus(psxPartA, psxBefore)
	if err != nil {
		t.Fatalf("part A status after using later commitment: %v", err)
	}
	psxAccount(t, afterUse, 9, 1, 8)
	psxDetail(t, afterUse, "psx-c3", "psx-r2", psxPartA, 2, 1, 1, CommitmentActive, psxLater)

	// 乙没有被查看甲的查询确认到期：传入共同到期前的时刻查看乙，仍看到
	// 实物五件、占用三件、可承诺两件，原承诺有效；明细只含乙自己的承诺。
	stB, err := s.PartStatus(psxPartB, psxBefore)
	if err != nil {
		t.Fatalf("part B status at earlier time: %v", err)
	}
	psxAccount(t, stB, 5, 3, 2)
	if len(stB.Details) != 1 {
		t.Fatalf("part B details = %d %+v, want only psx-c4", len(stB.Details), stB.Details)
	}
	psxDetail(t, stB, "psx-c4", "psx-r1", psxPartB, 4, 1, 3, CommitmentActive, psxExpiry)
	if c, _ := s.Commitment("psx-c4"); c.Expired {
		t.Fatalf("part B commitment confirmed by part A view: %+v", c)
	}
	// 此时用新的使用编号领取一件应成功：账目变为四件、两件、两件。
	if _, err := s.Use("psx-u4", "psx-c4", 1, psxBefore); err != nil {
		t.Fatalf("part B commitment closed by part A view: %v", err)
	}
	stB2, err := s.PartStatus(psxPartB, psxBefore)
	if err != nil {
		t.Fatalf("part B status after use: %v", err)
	}
	psxAccount(t, stB2, 4, 2, 2)
	psxDetail(t, stB2, "psx-c4", "psx-r1", psxPartB, 4, 2, 2, CommitmentActive, psxExpiry)
}

// TestPartStatusFailuresDoNotConfirmExpiry 保障查询提前失败不确认到期：备件
// 编号为空返回 ErrInvalidParam，编号尚未登记返回 ErrNotFound；即使传入的
// 时刻已达到承诺的到期时刻，这两种失败也不确认任何已有承诺、不改变数量、
// 不创建备件记录，之后按到期前的时刻仍保持原有占用并可正常使用。
func TestPartStatusFailuresDoNotConfirmExpiry(t *testing.T) {
	s := psxStore(t)
	psxReserveAll(t, s)

	// 传入的时刻恰好达到共同到期时刻，但两种提前失败都不得确认任何承诺。
	if _, err := s.PartStatus("", psxExpiry); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty part id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.PartStatus("psx-missing", psxExpiry); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown part id: got %v, want ErrNotFound", err)
	}
	// 失败的查询不创建备件记录。
	if _, err := s.Part("psx-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing part record created by failed query: got %v, want ErrNotFound", err)
	}

	// 没有任何承诺被确认到期，数量保持不变。
	for _, id := range []string{"psx-c1", "psx-c2", "psx-c3", "psx-c4"} {
		if c, _ := s.Commitment(id); c.Expired || c.Canceled {
			t.Fatalf("%s confirmed by failed query: %+v", id, c)
		}
	}
	if p, _ := s.Part(psxPartA); p.Stock != 10 {
		t.Fatalf("part A stock = %d after failed queries, want 10", p.Stock)
	}
	if p, _ := s.Part(psxPartB); p.Stock != 5 {
		t.Fatalf("part B stock = %d after failed queries, want 5", p.Stock)
	}

	// 按到期前的时刻查看：甲保持原有占用（三加三加二共八件），全部承诺
	// 仍 active；乙同样保持五件实物、三件占用。
	stA, err := s.PartStatus(psxPartA, psxBefore)
	if err != nil {
		t.Fatalf("part A status at earlier time: %v", err)
	}
	psxAccount(t, stA, 10, 8, 2)
	psxDetail(t, stA, "psx-c1", "psx-r1", psxPartA, 5, 2, 3, CommitmentActive, psxExpiry)
	psxDetail(t, stA, "psx-c2", "psx-r2", psxPartA, 3, 0, 3, CommitmentActive, psxExpiry)
	psxDetail(t, stA, "psx-c3", "psx-r2", psxPartA, 2, 0, 2, CommitmentActive, psxLater)
	stB, err := s.PartStatus(psxPartB, psxBefore)
	if err != nil {
		t.Fatalf("part B status at earlier time: %v", err)
	}
	psxAccount(t, stB, 5, 3, 2)
	psxDetail(t, stB, "psx-c4", "psx-r1", psxPartB, 4, 1, 3, CommitmentActive, psxExpiry)

	// 未被有效操作确认到期的承诺之后仍可正常使用。
	if _, err := s.Use("psx-u5", "psx-c1", 1, psxBefore); err != nil {
		t.Fatalf("commitment closed by failed queries: %v", err)
	}
	if _, err := s.Use("psx-u6", "psx-c4", 1, psxBefore); err != nil {
		t.Fatalf("part B commitment closed by failed queries: %v", err)
	}
}
