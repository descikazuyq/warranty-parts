package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障按备件查看库存与承诺明细（PartStatus）确认到期的范围：查询
// 只确认被查看备件自己的承诺，同一保修请求挂在其他备件上的承诺即使到期时刻
// 相同也不能被一起关闭；被查看备件下所有已到期的承诺要一次全部确认，到期更晚
// 的保持有效。空编号与未知备件的失败查询不确认任何承诺、不改变数量、不创建
// 备件记录。按请求查询（RequestView）与使用（Use）的既有处理范围不受影响，
// 由 request_view_expiry_test.go、use_expiry_isolation_test.go 各自保障。

// psScopePartA 等是本文件专用的编号，避免与其他测试文件的编号混用。
const (
	psScopePartA   = "ps-part-a"
	psScopePartB   = "ps-part-b"
	psScopeMissing = "ps-part-missing"
)

// psScopeTimes 给出本文件共用的三个时刻：两笔甲承诺与乙承诺的共同到期时刻
// noon、到期前的 before、甲第三笔承诺更晚的到期时刻 laterExpiry。
func psScopeTimes() (noon, before, laterExpiry time.Time) {
	noon = t0.Add(20 * day)
	return noon, noon.Add(-time.Minute), t0.Add(25 * day)
}

// psScopeStore 构造用户给出的主例：产品保修 30 天（全部操作与查询时刻都在
// 保修期内），甲备件初始库存十二件，乙备件初始库存六件，两张始终合格的请求。
// 承诺布局：
//   - c-a1：请求一预留甲五件（已使用两件），noon 到期；
//   - c-a2：请求二预留甲三件，noon 到期；
//   - c-a3：请求一预留甲两件，到期更晚（laterExpiry），尚未使用；
//   - c-b1：请求一预留乙四件（已使用一件），noon 到期。
// 所有预留均在资格合格时成功，承诺均未取消。
func psScopeStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("ps-p1", t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(psScopePartA, 12); err != nil {
		t.Fatalf("register part A: %v", err)
	}
	if err := s.RegisterPart(psScopePartB, 6); err != nil {
		t.Fatalf("register part B: %v", err)
	}
	for _, id := range []string{"ps-r1", "ps-r2"} {
		if err := s.SubmitRequest(id, "ps-p1", "FAULTY"); err != nil {
			t.Fatalf("submit %s: %v", id, err)
		}
	}
	noon, before, laterExpiry := psScopeTimes()
	if _, err := s.Reserve("c-a1", "ps-r1", psScopePartA, 5, noon, before); err != nil {
		t.Fatalf("reserve c-a1: %v", err)
	}
	if _, err := s.Use("ps-u-a1", "c-a1", 2, before); err != nil {
		t.Fatalf("use 2 from c-a1: %v", err)
	}
	if _, err := s.Reserve("c-a2", "ps-r2", psScopePartA, 3, noon, before); err != nil {
		t.Fatalf("reserve c-a2: %v", err)
	}
	if _, err := s.Reserve("c-a3", "ps-r1", psScopePartA, 2, laterExpiry, before); err != nil {
		t.Fatalf("reserve c-a3: %v", err)
	}
	if _, err := s.Reserve("c-b1", "ps-r1", psScopePartB, 4, noon, before); err != nil {
		t.Fatalf("reserve c-b1: %v", err)
	}
	if _, err := s.Use("ps-u-b1", "c-b1", 1, before); err != nil {
		t.Fatalf("use 1 from c-b1: %v", err)
	}
	return s
}

// assertScopeAccount 断言某次 PartStatus 的库存三项为期望值。
func assertScopeAccount(t *testing.T, st *PartStatus, physical, occupied, committable int) {
	t.Helper()
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock account phys=%d occupied=%d committable=%d, want phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// assertScopeDetail 断言一条明细完整反映一笔承诺的事实：归属请求、原定、
// 已用、未用、状态与到期时刻。
func assertScopeDetail(t *testing.T, d CommitmentDetail, requestID string, original, used, remaining int, status CommitmentStatus, expiry time.Time) {
	t.Helper()
	if d.RequestID != requestID || d.OriginalQuantity != original || d.UsedQuantity != used ||
		d.RemainingQuantity != remaining || d.Status != status || !d.Expiry.Equal(expiry) {
		t.Fatalf("detail %q: req=%s orig=%d used=%d rem=%d status=%s expiry=%v; "+
			"want req=%s orig=%d used=%d rem=%d status=%s expiry=%v",
			d.CommitmentID, d.RequestID, d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status, d.Expiry,
			requestID, original, used, remaining, status, expiry)
	}
}

// assertScopeExpired 断言各承诺的到期确认标记与期望一致。
func assertScopeExpired(t *testing.T, s *Store, want map[string]bool) {
	t.Helper()
	for id, expired := range want {
		c, err := s.Commitment(id)
		if err != nil {
			t.Fatalf("commitment %s: %v", id, err)
		}
		if c.Expired != expired {
			t.Fatalf("%s Expired=%v, want %v", id, c.Expired, expired)
		}
	}
}

// TestPartStatusExpiryScopedToViewedPart 对应用户给出的主例：恰好到达共同
// 到期时刻只查看甲，甲下面两笔同时到期的承诺被一次全部确认，释放各三件未用
// 占用（已使用的两件实物不回库存），到期更晚的一笔保持有效；实物剩余十件、
// 有效占用两件、可承诺八件。明细只含甲的三笔承诺及各自归属，不混入乙的承诺；
// 同一请求挂在乙上的承诺到期时刻相同也不被一起关闭。
func TestPartStatusExpiryScopedToViewedPart(t *testing.T) {
	s := psScopeStore(t)
	noon, before, laterExpiry := psScopeTimes()

	// 恰好到达共同到期时刻，只查看甲。
	st, err := s.PartStatus(psScopePartA, noon)
	if err != nil {
		t.Fatalf("part A status at expiry: %v", err)
	}
	// 实物剩余十件（十二件初始库存只扣除已使用的两件），有效占用只剩到期
	// 更晚的两件，可承诺八件。
	assertScopeAccount(t, st, 10, 2, 8)
	if st.PartID != psScopePartA {
		t.Fatalf("part id = %q, want %q", st.PartID, psScopePartA)
	}
	// 明细保留甲的三笔承诺，按承诺编号排序，各自归属、原数量、已用和未用
	// 数量完整：前两笔 expired，后一笔 active；不混入乙的 c-b1。
	if len(st.Details) != 3 {
		t.Fatalf("part A details = %d %+v, want 3", len(st.Details), st.Details)
	}
	wantOrder := []string{"c-a1", "c-a2", "c-a3"}
	for i, id := range wantOrder {
		if st.Details[i].CommitmentID != id {
			t.Fatalf("order at %d = %q, want %q (full order %v)",
				i, st.Details[i].CommitmentID, id, wantOrder)
		}
		if st.Details[i].PartID != psScopePartA {
			t.Fatalf("detail %s part = %q, want %q", id, st.Details[i].PartID, psScopePartA)
		}
	}
	assertScopeDetail(t, st.Details[0], "ps-r1", 5, 2, 3, CommitmentExpired, noon)
	assertScopeDetail(t, st.Details[1], "ps-r2", 3, 0, 3, CommitmentExpired, noon)
	assertScopeDetail(t, st.Details[2], "ps-r1", 2, 0, 2, CommitmentActive, laterExpiry)

	// 仓库记录与查询结果一致：前两笔已确认到期，后一笔与乙的承诺都未确认；
	// 已使用的两件实物不回到库存。
	assertScopeExpired(t, s, map[string]bool{
		"c-a1": true, "c-a2": true, "c-a3": false, "c-b1": false,
	})
	if p, _ := s.Part(psScopePartA); p.Stock != 10 {
		t.Fatalf("part A stock = %d, want 10 (used quantity must not return)", p.Stock)
	}

	// 这次查询之后，即使传入到期前的时刻，甲的前两笔仍已到期：不再计入
	// 有效占用（可承诺数量不回落）、持续显示 expired，也不能接受新的使用；
	// 到期更晚的一笔保持有效。
	rollback, err := s.PartStatus(psScopePartA, before)
	if err != nil {
		t.Fatalf("part A status at earlier time: %v", err)
	}
	assertScopeAccount(t, rollback, 10, 2, 8)
	assertScopeDetail(t, mustDetailByID(t, rollback.Details, "c-a1"), "ps-r1", 5, 2, 3, CommitmentExpired, noon)
	assertScopeDetail(t, mustDetailByID(t, rollback.Details, "c-a2"), "ps-r2", 3, 0, 3, CommitmentExpired, noon)
	assertScopeDetail(t, mustDetailByID(t, rollback.Details, "c-a3"), "ps-r1", 2, 0, 2, CommitmentActive, laterExpiry)
	if _, err := s.Use("ps-u-a1-late", "c-a1", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use c-a1 after confirmation: got %v, want ErrCommitmentClosed", err)
	}
	if _, err := s.Use("ps-u-a2-late", "c-a2", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use c-a2 after confirmation: got %v, want ErrCommitmentClosed", err)
	}

	// 乙没有被这次查询确认到期：随后传入共同到期前的时刻查看乙，仍应看到
	// 实物五件、占用三件、可承诺两件，原承诺仍有效，明细只有乙自己的承诺。
	stB, err := s.PartStatus(psScopePartB, before)
	if err != nil {
		t.Fatalf("part B status at earlier time: %v", err)
	}
	assertScopeAccount(t, stB, 5, 3, 2)
	if len(stB.Details) != 1 || stB.Details[0].CommitmentID != "c-b1" {
		t.Fatalf("part B details = %+v, want only c-b1", stB.Details)
	}
	assertScopeDetail(t, stB.Details[0], "ps-r1", 4, 1, 3, CommitmentActive, noon)

	// 此时用新的使用编号从乙的承诺领取一件应成功：账目变为四件、两件、两件。
	if _, err := s.Use("ps-u-b1-more", "c-b1", 1, before); err != nil {
		t.Fatalf("use 1 more from c-b1: %v", err)
	}
	stB2, err := s.PartStatus(psScopePartB, before)
	if err != nil {
		t.Fatalf("part B status after use: %v", err)
	}
	assertScopeAccount(t, stB2, 4, 2, 2)
	assertScopeDetail(t, mustDetailByID(t, stB2.Details, "c-b1"), "ps-r1", 4, 2, 2, CommitmentActive, noon)

	// 甲到期更晚的一笔保持有效：到期前时刻仍可正常使用。
	if _, err := s.Use("ps-u-a3", "c-a3", 1, before); err != nil {
		t.Fatalf("use 1 from c-a3: %v", err)
	}
	stA2, err := s.PartStatus(psScopePartA, before)
	if err != nil {
		t.Fatalf("part A status after c-a3 use: %v", err)
	}
	assertScopeAccount(t, stA2, 9, 1, 8)
	assertScopeDetail(t, mustDetailByID(t, stA2.Details, "c-a3"), "ps-r1", 2, 1, 1, CommitmentActive, laterExpiry)
}

// TestPartStatusFailuresDoNotConfirmExpiry 保障查询提前失败：备件编号为空
// 返回 ErrInvalidParam，编号尚未登记返回 ErrNotFound。即使传入的时刻已达到
// 承诺的到期时刻，这两种失败也不确认任何已有承诺到期、不改变数量、不创建
// 备件记录；之后按到期前的时刻查看，全部承诺仍保持原有占用并可正常使用。
func TestPartStatusFailuresDoNotConfirmExpiry(t *testing.T) {
	s := psScopeStore(t)
	noon, before, laterExpiry := psScopeTimes()

	// 即便传入共同到期时刻，提前失败的查询也不确认任何承诺。
	if _, err := s.PartStatus("", noon); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty part id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.PartStatus(psScopeMissing, noon); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown part: got %v, want ErrNotFound", err)
	}

	// 失败查询不创建备件记录。
	if _, err := s.Part(psScopeMissing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing part after failed query: got %v, want ErrNotFound", err)
	}

	// 没有任何承诺被确认到期，数量不变。
	assertScopeExpired(t, s, map[string]bool{
		"c-a1": false, "c-a2": false, "c-a3": false, "c-b1": false,
	})
	if p, _ := s.Part(psScopePartA); p.Stock != 10 {
		t.Fatalf("part A stock = %d, want 10", p.Stock)
	}
	if p, _ := s.Part(psScopePartB); p.Stock != 5 {
		t.Fatalf("part B stock = %d, want 5", p.Stock)
	}

	// 之后按到期前的时刻查看：甲仍保持原有占用（三笔共八件未用），全部
	// 承诺显示 active。
	stA, err := s.PartStatus(psScopePartA, before)
	if err != nil {
		t.Fatalf("part A status at earlier time: %v", err)
	}
	assertScopeAccount(t, stA, 10, 8, 2)
	if len(stA.Details) != 3 {
		t.Fatalf("part A details = %d, want 3", len(stA.Details))
	}
	assertScopeDetail(t, mustDetailByID(t, stA.Details, "c-a1"), "ps-r1", 5, 2, 3, CommitmentActive, noon)
	assertScopeDetail(t, mustDetailByID(t, stA.Details, "c-a2"), "ps-r2", 3, 0, 3, CommitmentActive, noon)
	assertScopeDetail(t, mustDetailByID(t, stA.Details, "c-a3"), "ps-r1", 2, 0, 2, CommitmentActive, laterExpiry)

	// 乙同样保持原有占用：实物五件、占用三件、可承诺两件。
	stB, err := s.PartStatus(psScopePartB, before)
	if err != nil {
		t.Fatalf("part B status at earlier time: %v", err)
	}
	assertScopeAccount(t, stB, 5, 3, 2)
	assertScopeDetail(t, mustDetailByID(t, stB.Details, "c-b1"), "ps-r1", 4, 1, 3, CommitmentActive, noon)

	// 未被有效操作确认到期的承诺之后仍可正常使用。
	if _, err := s.Use("ps-u-a1-more", "c-a1", 1, before); err != nil {
		t.Fatalf("c-a1 closed by failed queries: %v", err)
	}
	if _, err := s.Use("ps-u-b1-more", "c-b1", 1, before); err != nil {
		t.Fatalf("c-b1 closed by failed queries: %v", err)
	}
	stA2, err := s.PartStatus(psScopePartA, before)
	if err != nil {
		t.Fatalf("part A status after use: %v", err)
	}
	assertScopeAccount(t, stA2, 9, 7, 2)
}
