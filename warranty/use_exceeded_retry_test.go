package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障分批使用在“业务拒绝—修正后沿用原编号重试”场景下的编号规则：
// 使用编号的内容只由首次成功的使用确定；一次新使用因为超过本笔承诺的未用
// 数量而被 ErrUsageExceeded 整次拒绝时，不能先扣掉允许使用的部分，也不能提前
// 占住使用编号——既不把编号绑定到这笔承诺，也不把被拒绝的数量记成冲突内容。
// 调用方随后沿用同一编号、仍指向同一承诺、只把数量改成合法值即可成功；即使
// 仓库实物仍充足，也不能借用其他承诺的未用余量。成功后原样重试取回首次的
// 使用记录，再改回曾被业务拒绝的数量则按 ErrConflict 处理，成功结果保留。
//
// 主例数据（与用户描述一致）：备件 part1 初始库存 12 件；请求 r1 的承诺 c1
// 预留 5 件、已用 2 件（未用 3 件）；请求 r2 的承诺 c2 预留 4 件、尚未使用
// （未用 4 件）。两笔承诺共用同一备件、分属不同请求，均未取消、未到期。
// 此时实物剩余 10 件，有效占用 7 件（3+4），可承诺 3 件。

// exceededRetryStore 构造主例初始状态：part1 初始 12 件，c1（属于 r1）预留
// 5 件并已成功使用 2 件，c2（属于 r2）预留 4 件尚未使用。两笔承诺到期时刻
// 相同且晚于全部操作时刻。
func exceededRetryStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request r1: %v", err)
	}
	if err := s.SubmitRequest("r2", "p1", "NOISE"); err != nil {
		t.Fatalf("submit request r2: %v", err)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r2", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	if _, err := s.Use("uSeed", "c1", 2, nowOK); err != nil {
		t.Fatalf("seed use 2 from c1: %v", err)
	}
	return s
}

// assertExceededBaseline 断言业务拒绝前后的主例状态：实物 10、有效占用 7
// （c1 未用 3 + c2 未用 4）、可承诺 3；c1 仍是原定 5、已用 2、未用 3 且
// 归属于 r1，c2 仍是原定 4、已用 0、未用 4 且归属于 r2，两笔均有效。
func assertExceededBaseline(t *testing.T, s *Store) {
	t.Helper()
	assertPartAccount(t, s, 10, 7, 3)

	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if len(st.Details) != 2 {
		t.Fatalf("part details = %d, want 2", len(st.Details))
	}
	d1 := mustDetailByID(t, st.Details, "c1")
	if d1.Status != CommitmentActive || d1.RequestID != "r1" || d1.PartID != "part1" ||
		d1.OriginalQuantity != 5 || d1.UsedQuantity != 2 || d1.RemainingQuantity != 3 {
		t.Fatalf("c1 part detail = %+v, want active r1/part1 qty=5 used=2 remaining=3", d1)
	}
	d2 := mustDetailByID(t, st.Details, "c2")
	if d2.Status != CommitmentActive || d2.RequestID != "r2" || d2.PartID != "part1" ||
		d2.OriginalQuantity != 4 || d2.UsedQuantity != 0 || d2.RemainingQuantity != 4 {
		t.Fatalf("c2 part detail = %+v, want active r2/part1 qty=4 used=0 remaining=4", d2)
	}

	v1, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view r1: %v", err)
	}
	if len(v1.Commitments) != 1 {
		t.Fatalf("r1 commitments = %d, want 1", len(v1.Commitments))
	}
	rd1 := mustDetailByID(t, v1.Commitments, "c1")
	if rd1.Status != CommitmentActive || rd1.OriginalQuantity != 5 ||
		rd1.UsedQuantity != 2 || rd1.RemainingQuantity != 3 {
		t.Fatalf("r1 view detail = %+v, want active qty=5 used=2 remaining=3", rd1)
	}

	v2, err := s.RequestView("r2", nowOK)
	if err != nil {
		t.Fatalf("request view r2: %v", err)
	}
	if len(v2.Commitments) != 1 {
		t.Fatalf("r2 commitments = %d, want 1", len(v2.Commitments))
	}
	rd2 := mustDetailByID(t, v2.Commitments, "c2")
	if rd2.Status != CommitmentActive || rd2.OriginalQuantity != 4 ||
		rd2.UsedQuantity != 0 || rd2.RemainingQuantity != 4 {
		t.Fatalf("r2 view detail = %+v, want active qty=4 used=0 remaining=4", rd2)
	}
}

// TestUsageExceededRejectedThenSameIDSucceeds 是用户给定的完整主例：先用从未
// 成功过的编号向 c1 申请使用 4 件（c1 未用仅 3 件，尽管实物还剩 10 件、c2
// 还有 4 件未用余量），必须得到 ErrUsageExceeded 且整次提交失败；随后沿用
// 同一编号改为 3 件应当成功并恰好用完 c1 余量；成功后原样重取返回同一记录，
// 再改回先前被拒的 4 件则返回 ErrConflict，三件的成功结果保留。
func TestUsageExceededRejectedThenSameIDSucceeds(t *testing.T) {
	s := exceededRetryStore(t)
	assertExceededBaseline(t, s)

	// 1) 新编号申请 4 件：超过 c1 自己的未用数量 3 件。即使实物剩余 10 件
	//    足以覆盖，也不能借用 c2 的余量；必须按 ErrUsageExceeded 整次失败，
	//    不能先扣掉允许使用的 3 件，也不能按编号冲突处理。
	u, err := s.Use("uRetry", "c1", 4, nowOK)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use 4 over unused 3: got %v, want ErrUsageExceeded", err)
	}
	if errors.Is(err, ErrConflict) {
		t.Fatalf("business rejection must not be reported as ErrConflict: %v", err)
	}
	if u != (Usage{}) {
		t.Fatalf("rejected use returned non-empty record: %+v", u)
	}

	// 整次失败不留任何痕迹：c1、c2 的原定/已用/未用数量、承诺归属，以及实物
	// 剩余、有效占用、可承诺数量都保持提交前的值。
	c1, _ := s.Commitment("c1")
	if c1.Quantity != 5 || c1.Used != 2 || c1.Unused() != 3 || c1.Canceled || c1.Expired {
		t.Fatalf("c1 mutated by rejected use: %+v", c1)
	}
	c2, _ := s.Commitment("c2")
	if c2.Quantity != 4 || c2.Used != 0 || c2.Unused() != 4 || c2.Canceled || c2.Expired {
		t.Fatalf("c2 mutated by rejected use: %+v", c2)
	}
	assertExceededBaseline(t, s)

	// 2) 沿用刚才失败的同一编号、仍指向 c1，改为合法数量 3 件：成功。返回
	//    的使用记录必须明确对应这个编号、这笔承诺和三件数量，不能被先前的
	//    失败误判为编号冲突。
	first, err := s.Use("uRetry", "c1", 3, nowOK)
	if err != nil {
		t.Fatalf("same id with legal quantity 3 after rejection: %v", err)
	}
	if first != (Usage{ID: "uRetry", CommitmentID: "c1", Quantity: 3}) {
		t.Fatalf("successful usage = %+v, want {uRetry c1 3}", first)
	}

	// c1 累计已用 5、未用 0（恰好用完余量）；c2 保持原定 4、已用 0、未用 4。
	c1b, _ := s.Commitment("c1")
	if c1b.Quantity != 5 || c1b.Used != 5 || c1b.Unused() != 0 {
		t.Fatalf("c1 after 3-piece use: %+v, want qty=5 used=5 unused=0", c1b)
	}
	c2b, _ := s.Commitment("c2")
	if c2b.Quantity != 4 || c2b.Used != 0 || c2b.Unused() != 4 {
		t.Fatalf("c2 disturbed by c1 use: %+v, want qty=4 used=0 unused=4", c2b)
	}
	// 实物 10-3=7，有效占用 4（c1 未用 0 + c2 未用 4），可承诺 3。
	assertPartAccount(t, s, 7, 4, 3)

	// 按备件查看：c1 显示 5/5/0 且仍归属 r1，c2 显示 4/0/4 且仍归属 r2。
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status after success: %v", err)
	}
	sd1 := mustDetailByID(t, st.Details, "c1")
	if sd1.Status != CommitmentActive || sd1.RequestID != "r1" ||
		sd1.OriginalQuantity != 5 || sd1.UsedQuantity != 5 || sd1.RemainingQuantity != 0 {
		t.Fatalf("c1 detail after success: %+v, want active r1 qty=5 used=5 remaining=0", sd1)
	}
	sd2 := mustDetailByID(t, st.Details, "c2")
	if sd2.Status != CommitmentActive || sd2.RequestID != "r2" ||
		sd2.OriginalQuantity != 4 || sd2.UsedQuantity != 0 || sd2.RemainingQuantity != 4 {
		t.Fatalf("c2 detail after success: %+v, want active r2 qty=4 used=0 remaining=4", sd2)
	}
	// 按请求查看：r1 看到自己用完的 c1，r2 只看到原值不变的 c2。
	v1, err := s.RequestView("r1", nowOK)
	if err != nil {
		t.Fatalf("request view r1: %v", err)
	}
	if rv := mustDetailByID(t, v1.Commitments, "c1"); rv.UsedQuantity != 5 || rv.RemainingQuantity != 0 {
		t.Fatalf("r1 view after success: %+v, want used=5 remaining=0", rv)
	}
	v2, err := s.RequestView("r2", nowOK)
	if err != nil {
		t.Fatalf("request view r2: %v", err)
	}
	if rv := mustDetailByID(t, v2.Commitments, "c2"); rv.UsedQuantity != 0 || rv.RemainingQuantity != 4 {
		t.Fatalf("r2 view after success: %+v, want used=0 remaining=4", rv)
	}

	// 3) 成功后原样提交：取回这次三件的使用记录，数量与库存不再变化。
	again, err := s.Use("uRetry", "c1", 3, nowOK)
	if err != nil || again != first {
		t.Fatalf("identical resubmit = %+v, err %v; want first result %+v", again, err, first)
	}
	if c := mustCommitment(t, s, "c1"); c.Used != 5 {
		t.Fatalf("identical resubmit deducted again: c1 = %+v", c)
	}
	assertPartAccount(t, s, 7, 4, 3)

	// 4) 沿用该编号改回先前被业务拒绝的 4 件：编号内容已由首次成功确定，
	//    此时返回 ErrConflict，三件的成功结果与全部数量保留。
	if conflicted, err := s.Use("uRetry", "c1", 4, nowOK); !errors.Is(err, ErrConflict) || conflicted != (Usage{}) {
		t.Fatalf("change back to rejected 4: usage=%+v err=%v, want ErrConflict with empty result", conflicted, err)
	}
	if c := mustCommitment(t, s, "c1"); c.Used != 5 || c.Unused() != 0 {
		t.Fatalf("c1 changed by conflict: %+v, want used=5 unused=0", c)
	}
	if c := mustCommitment(t, s, "c2"); c.Used != 0 || c.Unused() != 4 {
		t.Fatalf("c2 changed by conflict: %+v, want used=0 unused=4", c)
	}
	assertPartAccount(t, s, 7, 4, 3)
	// 三件的成功记录仍可原样取回。
	if got, err := s.Use("uRetry", "c1", 3, nowOK); err != nil || got != first {
		t.Fatalf("three-piece result lost after conflict: got %+v err %v, want %+v", got, err, first)
	}
}

// TestUsageExceededRejectionLeavesIDFreeForOtherCommitment 补充锁定“业务拒绝
// 不提前占住编号”：同一新编号在 c1 上申请 4 件被 ErrUsageExceeded 拒绝后，
// 编号没有绑定到 c1，也没有记录被拒绝的内容；把它指向未用数量充足的 c2
// 申请 4 件仍是一笔合法的新使用并成功。此后编号内容由这次成功确定，再指向
// c1 提交即按 ErrConflict 处理，c1 的数量自始至终不被波及。
func TestUsageExceededRejectionLeavesIDFreeForOtherCommitment(t *testing.T) {
	s := exceededRetryStore(t)

	if _, err := s.Use("uRejected", "c1", 4, nowOK); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use 4 on c1 (unused 3): got %v, want ErrUsageExceeded", err)
	}

	// 同一编号改指 c2、数量 4（c2 未用恰为 4）：业务拒绝没有占用编号，
	// 这仍是全新的合法使用，应当成功并把编号内容绑定为 {uRejected c2 4}。
	u, err := s.Use("uRejected", "c2", 4, nowOK)
	if err != nil {
		t.Fatalf("rejected id reused against c2 with legal quantity: %v", err)
	}
	if u != (Usage{ID: "uRejected", CommitmentID: "c2", Quantity: 4}) {
		t.Fatalf("usage = %+v, want {uRejected c2 4}", u)
	}

	// c2 被用完 4 件；c1 维持 5/2/3，拒绝与 c2 的使用都没有动它。
	if c := mustCommitment(t, s, "c1"); c.Quantity != 5 || c.Used != 2 || c.Unused() != 3 {
		t.Fatalf("c1 disturbed: %+v, want qty=5 used=2 unused=3", c)
	}
	if c := mustCommitment(t, s, "c2"); c.Quantity != 4 || c.Used != 4 || c.Unused() != 0 {
		t.Fatalf("c2 after use: %+v, want qty=4 used=4 unused=0", c)
	}
	// 实物 10-4=6，有效占用 3（只剩 c1 未用 3），可承诺 3。
	assertPartAccount(t, s, 6, 3, 3)

	// 编号内容已绑定到 c2 的 4 件：再指向 c1 即使数量合法（3 件）也冲突，
	// 且原样取回的仍是 c2 的四件记录。
	if _, err := s.Use("uRejected", "c1", 3, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebound id pointed at c1: got %v, want ErrConflict", err)
	}
	if got, err := s.Use("uRejected", "c2", 4, nowOK); err != nil || got != u {
		t.Fatalf("record after conflict: got %+v err %v, want %+v", got, err, u)
	}
	assertPartAccount(t, s, 6, 3, 3)
}

// mustCommitment 取回承诺副本，不存在时直接失败。
func mustCommitment(t *testing.T, s *Store, id string) Commitment {
	t.Helper()
	c, err := s.Commitment(id)
	if err != nil {
		t.Fatalf("commitment %s: %v", id, err)
	}
	return c
}
