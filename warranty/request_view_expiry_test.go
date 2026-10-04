package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障 RequestView 确认到期的范围：查询只按请求归属确认该请求自己
// 的承诺，同一备件被多个请求预留时不能顺带确认其他请求的承诺；同一请求的多
// 笔到期承诺要一次全部确认。空编号与未知请求的失败查询不确认任何承诺。

// rvSharedStore 构造一个产品（保修 30 天）、库存为 stock 的备件和两个在保修期
// 内始终合格的请求，供按请求查看的到期范围测试使用。
func rvSharedStore(t *testing.T, stock int) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", stock); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r1: %v", err)
	}
	if err := s.SubmitRequest("r2", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}
	return s
}

// rvDetail 在请求视图中按承诺编号取明细。
func rvDetail(v *RequestView, id string) (CommitmentDetail, bool) {
	for _, d := range v.Commitments {
		if d.CommitmentID == id {
			return d, true
		}
	}
	return CommitmentDetail{}, false
}

// TestRequestViewExpiryScopedToViewedRequest 对应用户给出的主例：实物库存
// 十件，甲预留六件并使用两件，乙预留四件，两笔承诺同一时刻到期。到期时刻
// 查看甲只确认甲自己：甲 expired、释放四件未用占用、已用两件不回库存；乙
// 不被确认。随后以到期前时刻查看，乙仍 active，备件账目为 8/4/4，甲持续
// expired。
func TestRequestViewExpiryScopedToViewedRequest(t *testing.T) {
	s := rvSharedStore(t, 10)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)

	// 甲预留六件并使用两件；乙预留四件，两笔承诺在同一时刻到期。
	if _, err := s.Reserve("jia", "r1", "part1", 6, noon, before); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}
	if _, err := s.Use("u-jia", "jia", 2, before); err != nil {
		t.Fatalf("use jia: %v", err)
	}
	if _, err := s.Reserve("yi", "r2", "part1", 4, noon, before); err != nil {
		t.Fatalf("reserve yi: %v", err)
	}

	// 到期时刻查看甲：明细只包含甲的承诺，状态 expired，数量仍可核对。
	v, err := s.RequestView("r1", noon)
	if err != nil {
		t.Fatalf("view r1 at expiry: %v", err)
	}
	if !v.Eligibility.Eligible {
		t.Fatalf("eligibility at day 10: %+v", v.Eligibility)
	}
	if len(v.Commitments) != 1 {
		t.Fatalf("r1 view contains %d commitments %+v, want only jia", len(v.Commitments), v.Commitments)
	}
	dj := v.Commitments[0]
	if dj.CommitmentID != "jia" || dj.Status != CommitmentExpired ||
		dj.OriginalQuantity != 6 || dj.UsedQuantity != 2 || dj.RemainingQuantity != 4 ||
		!dj.Expiry.Equal(noon) || dj.RequestID != "r1" || dj.PartID != "part1" {
		t.Fatalf("jia detail: %+v, want expired 6/2/4 expiring at %v", dj, noon)
	}

	// 甲被确认到期、乙没有被这次查询顺带确认。
	cj, _ := s.Commitment("jia")
	if !cj.Expired || cj.Canceled || cj.Used != 2 {
		t.Fatalf("jia after view: %+v, want expired/used=2", cj)
	}
	cy, _ := s.Commitment("yi")
	if cy.Expired || cy.Canceled || cy.Used != 0 {
		t.Fatalf("yi confirmed by r1 view: %+v, want still unconfirmed", cy)
	}

	// 以到期前时刻查看乙：仍 active，明细只有乙自己。
	vy, err := s.RequestView("r2", before)
	if err != nil {
		t.Fatalf("view r2 before expiry: %v", err)
	}
	if len(vy.Commitments) != 1 {
		t.Fatalf("r2 view contains %d commitments %+v, want only yi", len(vy.Commitments), vy.Commitments)
	}
	dy := vy.Commitments[0]
	if dy.CommitmentID != "yi" || dy.Status != CommitmentActive ||
		dy.OriginalQuantity != 4 || dy.UsedQuantity != 0 || dy.RemainingQuantity != 4 {
		t.Fatalf("yi detail at earlier time: %+v, want active 4/0/4", dy)
	}

	// 以到期前时刻查看备件：实物八件（甲已用两件不回库），有效占用只剩乙的
	// 四件，可承诺四件；甲持续显示 expired。
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("stock after scoped confirmation: phys=%d occupied=%d committable=%d, want 8/4/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	pj, ok := detailByID(st, "jia")
	if !ok || pj.Status != CommitmentExpired || pj.OriginalQuantity != 6 ||
		pj.UsedQuantity != 2 || pj.RemainingQuantity != 4 {
		t.Fatalf("jia in part details: %+v ok=%v, want expired 6/2/4", pj, ok)
	}
	py, ok := detailByID(st, "yi")
	if !ok || py.Status != CommitmentActive || py.RemainingQuantity != 4 {
		t.Fatalf("yi in part details: %+v ok=%v, want active remaining=4", py, ok)
	}
	// 甲在自己的请求视图里回退时刻也保持 expired。
	v1, _ := s.RequestView("r1", before)
	if d, ok := rvDetail(v1, "jia"); !ok || d.Status != CommitmentExpired {
		t.Fatalf("jia rolled back in own view: %+v ok=%v", d, ok)
	}

	// 乙未被确认：到期前时刻仍可使用；甲在到期前时刻也一律关闭。
	if _, err := s.Use("u-yi", "yi", 1, before); err != nil {
		t.Fatalf("yi closed by r1 view: %v", err)
	}
	if _, err := s.Use("u-jia-2", "jia", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use jia after confirmation: got %v, want ErrCommitmentClosed", err)
	}
}

// TestRequestViewConfirmsAllOwnCommitmentsAcrossParts 保障同一请求关联多笔
// 承诺时确认范围按请求归属决定，不能只处理其中一笔：r1 在 partA/partB 上
// 各有承诺，到期的全部释放未用占用，未到期的保持有效；r2 使用相同备件、
// 到期时刻相同也不被确认。明细按承诺编号排序并保留原数量、已用和未用。
func TestRequestViewConfirmsAllOwnCommitmentsAcrossParts(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("partA", 10); err != nil {
		t.Fatalf("register partA: %v", err)
	}
	if err := s.RegisterPart("partB", 10); err != nil {
		t.Fatalf("register partB: %v", err)
	}
	for _, id := range []string{"r1", "r2"} {
		if err := s.SubmitRequest(id, "p1", "FAULTY"); err != nil {
			t.Fatalf("submit %s: %v", id, err)
		}
	}

	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	later := noon.Add(day)

	// r1：partA 上一笔到期（四件用一件）、一笔尚未到期（三件）；
	// partB 上一笔到期且已全部使用（两件）。
	if _, err := s.Reserve("c-a1", "r1", "partA", 4, noon, before); err != nil {
		t.Fatalf("reserve c-a1: %v", err)
	}
	if _, err := s.Use("u-a1", "c-a1", 1, before); err != nil {
		t.Fatalf("use c-a1: %v", err)
	}
	if _, err := s.Reserve("c-a2", "r1", "partA", 3, later, before); err != nil {
		t.Fatalf("reserve c-a2: %v", err)
	}
	if _, err := s.Reserve("c-b1", "r1", "partB", 2, noon, before); err != nil {
		t.Fatalf("reserve c-b1: %v", err)
	}
	if _, err := s.Use("u-b1", "c-b1", 2, before); err != nil {
		t.Fatalf("use c-b1: %v", err)
	}
	// r2：在相同备件上有同一时刻到期的承诺，查询 r1 不得确认它们。
	if _, err := s.Reserve("c-a3", "r2", "partA", 2, noon, before); err != nil {
		t.Fatalf("reserve c-a3: %v", err)
	}
	if _, err := s.Reserve("c-b2", "r2", "partB", 5, noon, before); err != nil {
		t.Fatalf("reserve c-b2: %v", err)
	}

	// 到期时刻查看 r1：三笔承诺全部出现且按编号排序，到期的两笔一次全部
	// 确认，未到期的一笔仍 active；原数量/已用/未用都保留。
	v, err := s.RequestView("r1", noon)
	if err != nil {
		t.Fatalf("view r1: %v", err)
	}
	wantOrder := []string{"c-a1", "c-a2", "c-b1"}
	if len(v.Commitments) != len(wantOrder) {
		t.Fatalf("r1 commitments = %d, want %d", len(v.Commitments), len(wantOrder))
	}
	for i, id := range wantOrder {
		if v.Commitments[i].CommitmentID != id {
			t.Fatalf("order at %d = %q, want %q (full order %v)",
				i, v.Commitments[i].CommitmentID, id, wantOrder)
		}
	}
	want := map[string]struct {
		status                    CommitmentStatus
		orig, used, remaining int
		part                      string
	}{
		"c-a1": {CommitmentExpired, 4, 1, 3, "partA"},
		"c-a2": {CommitmentActive, 3, 0, 3, "partA"},
		"c-b1": {CommitmentExpired, 2, 2, 0, "partB"},
	}
	for _, d := range v.Commitments {
		w := want[d.CommitmentID]
		if d.Status != w.status || d.OriginalQuantity != w.orig ||
			d.UsedQuantity != w.used || d.RemainingQuantity != w.remaining ||
			d.PartID != w.part || d.RequestID != "r1" {
			t.Fatalf("detail %s = %+v, want status=%s %d/%d/%d part=%s",
				d.CommitmentID, d, w.status, w.orig, w.used, w.remaining, w.part)
		}
	}

	// r1 的到期承诺都置位，未到期承诺与 r2 的承诺都不置位。
	for _, tc := range []struct {
		id      string
		expired bool
	}{
		{"c-a1", true}, {"c-a2", false}, {"c-b1", true},
		{"c-a3", false}, {"c-b2", false},
	} {
		c, _ := s.Commitment(tc.id)
		if c.Expired != tc.expired {
			t.Fatalf("%s Expired=%v, want %v", tc.id, c.Expired, tc.expired)
		}
	}

	// partA：实物九件；c-a1 释放三件，占用只剩 c-a2 三件与 r2 的 c-a3 两件，
	// 可承诺四件。
	stA, err := s.PartStatus("partA", before)
	if err != nil {
		t.Fatalf("partA status: %v", err)
	}
	if stA.PhysicalRemaining != 9 || stA.ActiveOccupied != 5 || stA.Committable != 4 {
		t.Fatalf("partA after r1 view: phys=%d occupied=%d committable=%d, want 9/5/4",
			stA.PhysicalRemaining, stA.ActiveOccupied, stA.Committable)
	}
	if d, _ := detailByID(stA, "c-a1"); d.Status != CommitmentExpired || d.RemainingQuantity != 3 {
		t.Fatalf("c-a1 in partA details: %+v, want expired remaining=3", d)
	}
	if d, _ := detailByID(stA, "c-a2"); d.Status != CommitmentActive || d.RemainingQuantity != 3 {
		t.Fatalf("c-a2 in partA details: %+v, want active remaining=3", d)
	}
	if d, _ := detailByID(stA, "c-a3"); d.Status != CommitmentActive || d.RemainingQuantity != 2 {
		t.Fatalf("c-a3 in partA details: %+v, want active remaining=2", d)
	}

	// partB：实物八件；c-b1 未用为零本就不占数量，占用只剩 r2 的五件，
	// 可承诺三件；全部用完的到期记录仍显示 expired 并保留 2/2/0。
	stB, err := s.PartStatus("partB", before)
	if err != nil {
		t.Fatalf("partB status: %v", err)
	}
	if stB.PhysicalRemaining != 8 || stB.ActiveOccupied != 5 || stB.Committable != 3 {
		t.Fatalf("partB after r1 view: phys=%d occupied=%d committable=%d, want 8/5/3",
			stB.PhysicalRemaining, stB.ActiveOccupied, stB.Committable)
	}
	if d, _ := detailByID(stB, "c-b1"); d.Status != CommitmentExpired ||
		d.OriginalQuantity != 2 || d.UsedQuantity != 2 || d.RemainingQuantity != 0 {
		t.Fatalf("c-b1 in partB details: %+v, want expired 2/2/0", d)
	}
	if d, _ := detailByID(stB, "c-b2"); d.Status != CommitmentActive || d.RemainingQuantity != 5 {
		t.Fatalf("c-b2 in partB details: %+v, want active remaining=5", d)
	}

	// r2 在到期前时刻查看：两笔承诺保持原先未确认的 active 状态，按编号排序。
	v2, err := s.RequestView("r2", before)
	if err != nil {
		t.Fatalf("view r2: %v", err)
	}
	if len(v2.Commitments) != 2 ||
		v2.Commitments[0].CommitmentID != "c-a3" || v2.Commitments[1].CommitmentID != "c-b2" {
		t.Fatalf("r2 commitments = %+v, want sorted [c-a3 c-b2]", v2.Commitments)
	}
	for _, d := range v2.Commitments {
		if d.Status != CommitmentActive {
			t.Fatalf("%s status = %q, want active", d.CommitmentID, d.Status)
		}
	}

	// 再次以较早时刻查看 r1：到期的不可逆、未到期的仍有效，状态不随时刻回退。
	v1, err := s.RequestView("r1", before)
	if err != nil {
		t.Fatalf("view r1 at earlier time: %v", err)
	}
	if d, _ := rvDetail(v1, "c-a1"); d.Status != CommitmentExpired {
		t.Fatalf("c-a1 rolled back: %+v", d)
	}
	if d, _ := rvDetail(v1, "c-b1"); d.Status != CommitmentExpired {
		t.Fatalf("c-b1 rolled back: %+v", d)
	}
	if d, _ := rvDetail(v1, "c-a2"); d.Status != CommitmentActive {
		t.Fatalf("c-a2 not active at earlier time: %+v", d)
	}

	// r2 的承诺到期前仍可正常使用，证明未被 r1 的查询关闭。
	if _, err := s.Use("u-b2", "c-b2", 1, before); err != nil {
		t.Fatalf("c-b2 closed by r1 view: %v", err)
	}
}

// TestRequestViewFailuresDoNotConfirmExpiry 保障空请求编号返回
// ErrInvalidParam、未知请求返回 ErrNotFound，两种失败都不确认任何已有承诺
// 到期，也不改变库存；查询资格仍按本次传入时刻计算。
func TestRequestViewFailuresDoNotConfirmExpiry(t *testing.T) {
	s := rvSharedStore(t, 10)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 6, noon, before); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}
	if _, err := s.Use("u-jia", "jia", 2, before); err != nil {
		t.Fatalf("use jia: %v", err)
	}

	// 即便传入晚于到期的时刻，提前失败的查询也不确认任何承诺。
	if _, err := s.RequestView("", noon); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.RequestView("rMissing", noon); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v, want ErrNotFound", err)
	}

	// 甲未被确认：到期前仍 active、占用四件未用，新使用成功。
	v, err := s.RequestView("r1", before)
	if err != nil {
		t.Fatalf("view r1: %v", err)
	}
	d, ok := rvDetail(v, "jia")
	if !ok || d.Status != CommitmentActive || d.OriginalQuantity != 6 ||
		d.UsedQuantity != 2 || d.RemainingQuantity != 4 {
		t.Fatalf("jia after failed views: %+v ok=%v, want active 6/2/4", d, ok)
	}
	st, _ := s.PartStatus("part1", before)
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("stock changed by failed views: phys=%d occupied=%d committable=%d, want 8/4/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if c, _ := s.Commitment("jia"); c.Expired {
		t.Fatalf("jia confirmed by failed view: %+v", c)
	}
	if _, err := s.Use("u-jia-more", "jia", 1, before); err != nil {
		t.Fatalf("jia closed by a non-confirming failed view: %v", err)
	}
}
