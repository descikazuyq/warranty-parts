package warranty

import (
	"errors"
	"math"
	"testing"
)

// 大数量库存回归保障：M 为当前运行环境中 int 可表示的最大正整数。登记接受
// 非负整数库存（M 合法），预留接受正整数数量（M-2、M-1、2、1 均合法），
// 这些大数必须继续可用；同一备件被多个合格请求预留时，累计未用数量不得因
// int 回绕而超过实物库存，超量提交只能返回 ErrInsufficientStock，不能成功，
// 也不能被当成参数非法。整个过程沿用现有 Reserve/PartStatus/RequestView/
// RequestHistory 公开入口，不新增业务操作。
func TestReserveLargeQuantityNoIntegerWrap(t *testing.T) {
	const M = math.MaxInt

	s := NewStore()
	// 两个请求关联同一已登记产品；故障代码不在除外清单中。
	if err := s.RegisterProduct("p1", t0, 365, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	// 初始库存就是 int 最大正整数：合法的非负整数登记必须继续可用。
	if err := s.RegisterPart("part1", M); err != nil {
		t.Fatalf("register part with max stock: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "NOISE"); err != nil {
		t.Fatalf("submit r1: %v", err)
	}
	if err := s.SubmitRequest("r2", "p1", "NOISE"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}

	// 第一个合格请求先成功预留 M-2 件（合法正整数数量）。
	c1, err := s.Reserve("c1", "r1", "part1", M-2, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve M-2: %v", err)
	}
	if c1.Quantity != M-2 || c1.Used != 0 || c1.Unused() != M-2 ||
		c1.RequestID != "r1" || c1.PartID != "part1" || !c1.Expiry.Equal(expiryOK) ||
		c1.Canceled || c1.Expired {
		t.Fatalf("unexpected first commitment: %+v", c1)
	}

	// 库存查询：实物剩余仍是 M（预留不扣减实物库存），有效占用 M-2，
	// 可承诺数量恰好 2，不允许回绕成大负数或大数。
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != M {
		t.Fatalf("physical remaining = %d, want %d", st.PhysicalRemaining, M)
	}
	if st.ActiveOccupied != M-2 {
		t.Fatalf("active occupied = %d, want %d", st.ActiveOccupied, M-2)
	}
	if st.Committable != 2 {
		t.Fatalf("committable = %d, want 2", st.Committable)
	}
	if len(st.Details) != 1 {
		t.Fatalf("details len = %d, want 1", len(st.Details))
	}
	d := st.Details[0]
	// 承诺明细保留原定数量与未用数量，已用数量为零。
	if d.CommitmentID != "c1" || d.RequestID != "r1" || d.PartID != "part1" ||
		d.OriginalQuantity != M-2 || d.UsedQuantity != 0 || d.RemainingQuantity != M-2 ||
		d.Status != CommitmentActive {
		t.Fatalf("unexpected detail: %+v", d)
	}

	// 第二个合格请求用新编号申请 M-1 件：数量本身可由 int 表示，但与现存
	// 占用累计会超过实物 M（若按加法核算还会 int 回绕）。必须返回库存不足，
	// 不能成功，也不能返回参数非法。
	if _, err := s.Reserve("c2", "r2", "part1", M-1, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve M-1 with only 2 committable: got %v", err)
	}

	// 失败不生成承诺。
	if _, err := s.Commitment("c2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve should not create commitment, got %v", err)
	}
	// 不改变原承诺的数量、归属与到期时刻。
	got1, err := s.Commitment("c1")
	if err != nil {
		t.Fatalf("commitment c1: %v", err)
	}
	if got1.Quantity != M-2 || got1.Used != 0 || got1.RequestID != "r1" ||
		!got1.Expiry.Equal(expiryOK) || got1.Canceled || got1.Expired {
		t.Fatalf("original commitment changed after failed reserve: %+v", got1)
	}
	// 查询仍只有先前的成功占用。
	st, _ = s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != M || st.ActiveOccupied != M-2 || st.Committable != 2 {
		t.Fatalf("stock changed after failed reserve: %+v", st)
	}
	if len(st.Details) != 1 || st.Details[0].CommitmentID != "c1" {
		t.Fatalf("details after failed reserve: %+v", st.Details)
	}

	// 失败请求仍能查到本次库存不足的历史记录；库存依据是拒绝前的真实账目
	// （实物 M、占用 M-2、可承诺 2），不能记成负数、零库存或失败之后的状态。
	h2, err := s.RequestHistory("r2")
	if err != nil {
		t.Fatalf("history r2: %v", err)
	}
	if len(h2) != 1 {
		t.Fatalf("r2 history len = %d, want 1", len(h2))
	}
	failRec := h2[0]
	if failRec.Seq != 1 || failRec.Success || failRec.Error != HistoryErrorInsufficientStock {
		t.Fatalf("bad failure record: %+v", failRec)
	}
	if failRec.CommitID != "c2" || failRec.PartID != "part1" || failRec.Quantity != M-1 {
		t.Fatalf("bad failure submission fields: %+v", failRec)
	}
	if failRec.Eligibility == nil || !failRec.Eligibility.Eligible {
		t.Fatalf("failure record eligibility should be eligible: %+v", failRec.Eligibility)
	}
	if failRec.StockBasis == nil {
		t.Fatal("failure record should carry the pre-rejection stock basis")
	}
	if failRec.StockBasis.PhysicalRemaining != M ||
		failRec.StockBasis.ActiveOccupied != M-2 ||
		failRec.StockBasis.Committable != 2 {
		t.Fatalf("failure stock basis must be real account before rejection: %+v", failRec.StockBasis)
	}

	// 同一请求改用新编号申请恰好 2 件：恰好等于可承诺数量，应成功。
	c3, err := s.Reserve("c3", "r2", "part1", 2, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve exact 2: %v", err)
	}
	if c3.Quantity != 2 || c3.Used != 0 || c3.Unused() != 2 || c3.RequestID != "r2" {
		t.Fatalf("unexpected second commitment: %+v", c3)
	}

	// 两笔有效承诺的未用数量合计为 M，实物库存仍为 M，可承诺数量为零；
	// (M-2)+2 在 int 内不回绕，占用核算不得依赖更小的数量上限。
	st, _ = s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != M || st.ActiveOccupied != M || st.Committable != 0 {
		t.Fatalf("stock after exact-second reserve: %+v", st)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details len = %d, want 2", len(st.Details))
	}
	remainingByID := map[string]int{}
	for _, d := range st.Details {
		if d.Status != CommitmentActive || d.UsedQuantity != 0 {
			t.Fatalf("unexpected detail status/used: %+v", d)
		}
		remainingByID[d.CommitmentID] = d.RemainingQuantity
	}
	if remainingByID["c1"] != M-2 || remainingByID["c3"] != 2 {
		t.Fatalf("remaining by commitment = %v", remainingByID)
	}
	if got := remainingByID["c1"] + remainingByID["c3"]; got != M {
		t.Fatalf("total unused = %d, want %d", got, M)
	}

	// 按请求和按备件查看的成功承诺归属与数量应一致。
	v1, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view r1: %v", err)
	}
	if len(v1.Commitments) != 1 || v1.Commitments[0].CommitmentID != "c1" ||
		v1.Commitments[0].RequestID != "r1" || v1.Commitments[0].OriginalQuantity != M-2 ||
		v1.Commitments[0].RemainingQuantity != M-2 {
		t.Fatalf("r1 view inconsistent with part view: %+v", v1.Commitments)
	}
	v2, err := s.RequestView("r2", nowOK)
	if err != nil {
		t.Fatalf("request view r2: %v", err)
	}
	// 失败的 c2 不产生承诺，r2 视图只有成功的 c3。
	if len(v2.Commitments) != 1 || v2.Commitments[0].CommitmentID != "c3" ||
		v2.Commitments[0].RequestID != "r2" || v2.Commitments[0].OriginalQuantity != 2 ||
		v2.Commitments[0].RemainingQuantity != 2 {
		t.Fatalf("r2 view inconsistent with part view: %+v", v2.Commitments)
	}

	// 成功记录的库存依据保留新增占用之前的数值（实物 M、占用 M-2、可承诺 2）。
	h2, _ = s.RequestHistory("r2")
	if len(h2) != 2 {
		t.Fatalf("r2 history len = %d, want 2", len(h2))
	}
	successRec := h2[1]
	if successRec.Seq != 2 || !successRec.Success || successRec.Error != "" ||
		successRec.CommitID != "c3" || successRec.Quantity != 2 {
		t.Fatalf("bad success record: %+v", successRec)
	}
	if successRec.StockBasis == nil ||
		successRec.StockBasis.PhysicalRemaining != M ||
		successRec.StockBasis.ActiveOccupied != M-2 ||
		successRec.StockBasis.Committable != 2 {
		t.Fatalf("success stock basis must predate the new occupancy: %+v", successRec.StockBasis)
	}

	// 可承诺数量已经为零：再用新编号申请 1 件仍返回库存不足，不能留下第三笔承诺。
	if _, err := s.Reserve("c4", "r2", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve 1 with zero committable: got %v", err)
	}
	if _, err := s.Commitment("c4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve should not create third commitment, got %v", err)
	}
	st, _ = s.PartStatus("part1", nowOK)
	if st.PhysicalRemaining != M || st.ActiveOccupied != M || st.Committable != 0 {
		t.Fatalf("stock changed after final failed reserve: %+v", st)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details len = %d, want still 2", len(st.Details))
	}

	// 最后一次失败记录的库存依据是拒绝前账目：实物 M、占用 M、可承诺 0。
	h2, _ = s.RequestHistory("r2")
	if len(h2) != 3 {
		t.Fatalf("r2 history len = %d, want 3", len(h2))
	}
	last := h2[2]
	if last.Seq != 3 || last.Success || last.Error != HistoryErrorInsufficientStock ||
		last.CommitID != "c4" || last.Quantity != 1 {
		t.Fatalf("bad final failure record: %+v", last)
	}
	if last.StockBasis == nil ||
		last.StockBasis.PhysicalRemaining != M ||
		last.StockBasis.ActiveOccupied != M ||
		last.StockBasis.Committable != 0 {
		t.Fatalf("final failure stock basis must be real account before rejection: %+v", last.StockBasis)
	}
}
