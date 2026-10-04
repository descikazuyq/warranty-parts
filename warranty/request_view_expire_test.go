package warranty

import (
	"errors"
	"testing"
	"time"
)

// viewDetailByID 在请求视图中按承诺编号查找明细。
func viewDetailByID(v *RequestView, id string) (CommitmentDetail, bool) {
	for _, d := range v.Commitments {
		if d.CommitmentID == id {
			return d, true
		}
	}
	return CommitmentDetail{}, false
}

// TestRequestViewConfirmsOnlyOwnExpiries 对应用户给出的主例：两个保修期内的
// 请求共享一种备件，实物库存十件；甲预留六件并用掉两件，乙预留四件，两笔承诺
// 同一时刻到期。在到期时刻查看甲：只确认甲的承诺到期，释放其未用四件，已用两
// 件不回实物；乙的承诺不被这次查询顺带确认。随后以到期前时刻查看，乙仍有效，
// 备件实物八件、有效占用四件、可承诺四件，甲保持 expired。
func TestRequestViewConfirmsOnlyOwnExpiries(t *testing.T) {
	s := expStore(t, 10)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)

	// 甲预留六件、使用两件；乙预留四件；两笔同时到期。
	if _, err := s.Reserve("jia", "r1", "part1", 6, noon, before); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}
	if _, err := s.Use("u1", "jia", 2, before); err != nil {
		t.Fatalf("use jia: %v", err)
	}
	if _, err := s.Reserve("yi", "r2", "part1", 4, noon, before); err != nil {
		t.Fatalf("reserve yi: %v", err)
	}

	// 到期时刻查看甲：明细只含甲的承诺，状态 expired，数量原样可查。
	v, err := s.RequestView("r1", noon)
	if err != nil {
		t.Fatalf("view r1 at expiry: %v", err)
	}
	if !v.Eligibility.Eligible {
		t.Fatalf("r1 should be eligible at noon: %+v", v.Eligibility)
	}
	if len(v.Commitments) != 1 {
		t.Fatalf("r1 view commitments = %d, want 1 (only jia)", len(v.Commitments))
	}
	dj := v.Commitments[0]
	if dj.CommitmentID != "jia" || dj.Status != CommitmentExpired ||
		dj.OriginalQuantity != 6 || dj.UsedQuantity != 2 || dj.RemainingQuantity != 4 ||
		!dj.Expiry.Equal(noon) {
		t.Fatalf("jia detail at expiry: %+v", dj)
	}

	// 这次查看没有确认乙的承诺。
	cy, err := s.Commitment("yi")
	if err != nil {
		t.Fatalf("get yi: %v", err)
	}
	if cy.Expired {
		t.Fatal("viewing r1 confirmed yi of r2")
	}

	// 以到期前时刻查看乙：仍显示 active。
	v2, err := s.RequestView("r2", before)
	if err != nil {
		t.Fatalf("view r2 before expiry: %v", err)
	}
	dy, ok := viewDetailByID(v2, "yi")
	if !ok || dy.Status != CommitmentActive || dy.RemainingQuantity != 4 {
		t.Fatalf("yi before expiry: %+v ok=%v, want active/remaining=4", dy, ok)
	}

	// 以同一较早时刻查看备件：甲已永久到期（占用释放、已用两件不回实物），
	// 乙未确认、按本次时刻仍有效——实物八件、有效占用四件、可承诺四件。
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status before expiry: %v", err)
	}
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 4 || st.Committable != 4 {
		t.Fatalf("stock before expiry: phys=%d occupied=%d committable=%d, want 8/4/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if d, _ := detailByID(st, "jia"); d.Status != CommitmentExpired {
		t.Fatalf("jia status at earlier time = %q, want expired", d.Status)
	}
	if d, _ := detailByID(st, "yi"); d.Status != CommitmentActive {
		t.Fatalf("yi status at earlier time = %q, want active", d.Status)
	}

	// 甲在较早时刻继续显示 expired，确认不可逆。
	v3, err := s.RequestView("r1", before)
	if err != nil {
		t.Fatalf("view r1 at earlier time: %v", err)
	}
	if d, _ := viewDetailByID(v3, "jia"); d.Status != CommitmentExpired {
		t.Fatalf("jia rolled back to %q, want expired", d.Status)
	}
}

// TestRequestViewConfirmsAllOwnExpiriesAcrossParts 验证同一请求关联多笔承诺时，
// 确认范围按请求归属决定：该请求已到期的承诺（即使涉及不同备件）全部确认并
// 各自释放未用占用，尚未到期的承诺保持有效；其他请求即使使用相同备件、到期
// 时刻相同，也保持未确认状态。明细按承诺编号排序，数量字段保留原值。
func TestRequestViewConfirmsAllOwnExpiriesAcrossParts(t *testing.T) {
	s := expStore(t, 10)
	if err := s.RegisterPart("part2", 5); err != nil {
		t.Fatalf("register part2: %v", err)
	}
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	later := noon.Add(day)

	// r1 的三笔承诺：c1（part1，四件，中午到期）、c2（part2，三件，中午到期，
	// 已用一件）、c3（part1，两件，更晚到期）。r2 的 c4 与 c1 同备件同时刻到期。
	if _, err := s.Reserve("c1", "r1", "part1", 4, noon, before); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r1", "part2", 3, noon, before); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	if _, err := s.Use("u1", "c2", 1, before); err != nil {
		t.Fatalf("use c2: %v", err)
	}
	if _, err := s.Reserve("c3", "r1", "part1", 2, later, before); err != nil {
		t.Fatalf("reserve c3: %v", err)
	}
	if _, err := s.Reserve("c4", "r2", "part1", 4, noon, before); err != nil {
		t.Fatalf("reserve c4: %v", err)
	}

	// 中午查看 r1：c1、c2 确认到期，c3 未到期保持 active；明细按编号排序，
	// 各承诺的原数量、已用、未用数量保留。
	v, err := s.RequestView("r1", noon)
	if err != nil {
		t.Fatalf("view r1 at noon: %v", err)
	}
	if len(v.Commitments) != 3 {
		t.Fatalf("r1 commitments = %d, want 3", len(v.Commitments))
	}
	for i, id := range []string{"c1", "c2", "c3"} {
		if v.Commitments[i].CommitmentID != id {
			t.Fatalf("commitments[%d] = %q, want %q (sorted by id)", i, v.Commitments[i].CommitmentID, id)
		}
	}
	d1 := v.Commitments[0]
	if d1.Status != CommitmentExpired || d1.OriginalQuantity != 4 ||
		d1.UsedQuantity != 0 || d1.RemainingQuantity != 4 {
		t.Fatalf("c1 detail: %+v, want expired 4/0/4", d1)
	}
	d2 := v.Commitments[1]
	if d2.Status != CommitmentExpired || d2.OriginalQuantity != 3 ||
		d2.UsedQuantity != 1 || d2.RemainingQuantity != 2 {
		t.Fatalf("c2 detail: %+v, want expired 3/1/2", d2)
	}
	d3 := v.Commitments[2]
	if d3.Status != CommitmentActive || d3.OriginalQuantity != 2 ||
		d3.UsedQuantity != 0 || d3.RemainingQuantity != 2 {
		t.Fatalf("c3 detail: %+v, want active 2/0/2", d3)
	}

	// 到期前时刻核对两种备件各自的账目：
	// part1 实物十件（未被使用），c1 四件已释放，c3 两件与 c4 四件仍占用；
	// part2 实物四件（c2 已用一件），c2 未用两件已释放，无有效占用。
	st1, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part1 status: %v", err)
	}
	if st1.PhysicalRemaining != 10 || st1.ActiveOccupied != 6 || st1.Committable != 4 {
		t.Fatalf("part1 stock: phys=%d occupied=%d committable=%d, want 10/6/4",
			st1.PhysicalRemaining, st1.ActiveOccupied, st1.Committable)
	}
	st2, err := s.PartStatus("part2", before)
	if err != nil {
		t.Fatalf("part2 status: %v", err)
	}
	if st2.PhysicalRemaining != 4 || st2.ActiveOccupied != 0 || st2.Committable != 4 {
		t.Fatalf("part2 stock: phys=%d occupied=%d committable=%d, want 4/0/4",
			st2.PhysicalRemaining, st2.ActiveOccupied, st2.Committable)
	}

	// r2 的 c4 与 c1 同备件、同时刻到期，但不因查看 r1 被确认。
	c4, err := s.Commitment("c4")
	if err != nil {
		t.Fatalf("get c4: %v", err)
	}
	if c4.Expired {
		t.Fatal("viewing r1 confirmed c4 of r2")
	}
	v2, err := s.RequestView("r2", before)
	if err != nil {
		t.Fatalf("view r2 before expiry: %v", err)
	}
	if d, _ := viewDetailByID(v2, "c4"); d.Status != CommitmentActive {
		t.Fatalf("c4 before expiry = %q, want active", d.Status)
	}
}

// TestRequestViewFailuresDoNotConfirmExpiry 验证空请求编号返回 ErrInvalidParam、
// 未知请求返回 ErrNotFound，且这两种提前失败不用本次时刻确认任何承诺到期，
// 也不改变库存。
func TestRequestViewFailuresDoNotConfirmExpiry(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}

	if _, err := s.RequestView("", after); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v, want ErrInvalidParam", err)
	}
	if _, err := s.RequestView("rMissing", after); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v, want ErrNotFound", err)
	}

	// 甲未被确认到期：较早时刻仍有效、占用五件，库存不变。
	c, err := s.Commitment("jia")
	if err != nil {
		t.Fatalf("get jia: %v", err)
	}
	if c.Expired {
		t.Fatal("failed request views confirmed expiry")
	}
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 5 || st.ActiveOccupied != 5 || st.Committable != 0 {
		t.Fatalf("failed views changed stock: phys=%d occupied=%d committable=%d, want 5/5/0",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if d, _ := detailByID(st, "jia"); d.Status != CommitmentActive {
		t.Fatalf("jia status = %q, want active", d.Status)
	}
}
