package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障分批使用被 ErrUsageExceeded 拒绝后的行为：一次新使用因为数量
// 超过本笔承诺的未用数量而被拒绝时，整次提交失败（不能先扣掉允许使用的部分），
// 且业务拒绝不提前占住使用编号——调用方仍能沿用同一编号、把数量改为合法值再次
// 提交并成功。使用编号的内容由首次成功的使用确定：成功后再原样提交取回同一份
// 记录，沿用该编号改回先前被拒绝的数量则返回 ErrConflict。即使仓库还有足够的
// 实物库存，另一笔承诺（属于不同请求、共用同一备件）的余量也不能借给本笔使用。
// 这些测试只沿用登记、预留、使用、取消和查询的既有公开行为。

// exceededRetryStore 构造用户给定的主例：备件 part1 初始十二件；请求 r1、r2
// 均已登记且共用该备件，c1 为 r1 预留五件、已被 u0 用掉两件，c2 为 r2 预留
// 四件、尚未使用。两笔承诺均未取消、未到期（到期时刻同为 expiryOK）。此时
// 实物剩余十件，有效占用七件（c1 未用三件 + c2 未用四件），可再承诺三件；
// c1 自己的未用数量只有三件。返回首次使用结果供比对。
func exceededRetryStore(t *testing.T) (*Store, Usage) {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 12); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r1: %v", err)
	}
	if err := s.SubmitRequest("r2", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c1: %v", err)
	}
	if _, err := s.Reserve("c2", "r2", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	u0, err := s.Use("u0", "c1", 2, nowOK)
	if err != nil {
		t.Fatalf("use u0: %v", err)
	}
	return s, u0
}

// assertExceededRetryStock 断言按备件查看的库存账目：实物剩余、有效占用与
// 可承诺数量符合预期，且 c1、c2 两笔承诺的已用/未用数量与状态符合预期。
// 查询时刻必须早于两笔承诺的到期时刻（调用方需保证），以免查询本身确认到期。
func assertExceededRetryStock(t *testing.T, s *Store, now time.Time,
	phys, occupied, committable, c1Used, c2Used int) {
	t.Helper()
	st, err := s.PartStatus("part1", now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != phys || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, phys, occupied, committable)
	}
	want := map[string]int{"c1": c1Used, "c2": c2Used}
	for id, used := range want {
		d, ok := detailByID(st, id)
		if !ok {
			t.Fatalf("commitment detail missing: %s", id)
		}
		if d.Status != CommitmentActive || d.UsedQuantity != used || d.RemainingQuantity != d.OriginalQuantity-used {
			t.Fatalf("%s detail = %+v, want active/used=%d", id, d, used)
		}
	}
}

// assertExceededRetryRequests 断言按请求查看时的承诺归属与数量：r1 下只有
// c1（原定五件），r2 下只有 c2（原定四件），已用数量符合预期。
func assertExceededRetryRequests(t *testing.T, s *Store, now time.Time, c1Used, c2Used int) {
	t.Helper()
	want := map[string]map[string]int{ // 请求 -> 承诺 -> 已用数量
		"r1": {"c1": c1Used},
		"r2": {"c2": c2Used},
	}
	for reqID, commitments := range want {
		view, err := s.RequestView(reqID, now)
		if err != nil {
			t.Fatalf("request view %s: %v", reqID, err)
		}
		if len(view.Commitments) != len(commitments) {
			t.Fatalf("%s commitments = %d, want %d", reqID, len(view.Commitments), len(commitments))
		}
		for _, d := range view.Commitments {
			used, ok := commitments[d.CommitmentID]
			if !ok {
				t.Fatalf("%s view has unexpected commitment: %+v", reqID, d)
			}
			if d.RequestID != reqID || d.PartID != "part1" {
				t.Fatalf("%s detail attribution = %+v, want request %s part part1", reqID, d, reqID)
			}
			if d.Status != CommitmentActive || d.UsedQuantity != used ||
				d.RemainingQuantity != d.OriginalQuantity-used {
				t.Fatalf("%s detail = %+v, want active/used=%d", d.CommitmentID, d, used)
			}
		}
	}
}

// TestUsageExceededFailsAtomicallyWithoutClaimingID 验证超量使用整次失败：
// 向未用数量只有三件的 c1 提交使用四件，即使仓库实物充足（剩余十件）也返回
// ErrUsageExceeded，不能先扣掉允许使用的三件，也不能借用 c2 的余量；失败后
// 两笔承诺的数量、库存账目与按请求/按备件查看到的归属都保持提交前的值。
func TestUsageExceededFailsAtomicallyWithoutClaimingID(t *testing.T) {
	s, _ := exceededRetryStore(t)
	submitAt := nowOK.Add(day)

	// 新编号 u1 向 c1 提交四件：超过 c1 未用的三件，整次失败。
	u, err := s.Use("u1", "c1", 4, submitAt)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use got %v, want ErrUsageExceeded", err)
	}
	if u != (Usage{}) {
		t.Fatalf("rejected use returned non-empty usage: %+v", u)
	}

	// 整次失败、没有部分扣减：c1 仍已用两件、未用三件，c2 仍未使用。
	c1, _ := s.Commitment("c1")
	c2, _ := s.Commitment("c2")
	if c1.Quantity != 5 || c1.Used != 2 || c1.Unused() != 3 || c1.Canceled || c1.Expired {
		t.Fatalf("c1 changed by rejected use: %+v", c1)
	}
	if c2.Quantity != 4 || c2.Used != 0 || c2.Unused() != 4 || c2.Canceled || c2.Expired {
		t.Fatalf("c2 changed by rejected use: %+v", c2)
	}
	// 实物剩余十件、有效占用七件、可承诺三件，与提交前一致。
	assertExceededRetryStock(t, s, submitAt, 10, 7, 3, 2, 0)
	// 按请求查看：c1 归 r1、c2 归 r2，数量同样保持提交前的值。
	assertExceededRetryRequests(t, s, submitAt, 2, 0)
}

// TestUsageIDReusableAfterExceededRejection 验证业务拒绝不提前占住使用编号：
// 用刚才失败的同一编号 u1 仍指向 c1、改为使用三件（恰为未用余量）应当成功，
// 返回的使用记录明确对应 u1、c1 和三件；c1 恰好用完，不能被先前那次失败误判
// 为编号冲突。成功后原样重试取回同一份记录、数量不再变化；沿用该编号改回先
// 前被拒绝的四件则返回 ErrConflict，并保留三件的成功结果。
func TestUsageIDReusableAfterExceededRejection(t *testing.T) {
	s, _ := exceededRetryStore(t)
	submitAt := nowOK.Add(day)

	// 先制造一次超量拒绝：u1 提交四件失败。
	if _, err := s.Use("u1", "c1", 4, submitAt); !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("over-use got %v, want ErrUsageExceeded", err)
	}

	// 同一编号 u1 改为三件（恰为 c1 未用余量）再次提交：成功，编号内容由此
	// 次成功确定，先前的失败不占住编号、也不被误判为冲突。
	u, err := s.Use("u1", "c1", 3, submitAt)
	if err != nil {
		t.Fatalf("retry with legal quantity after rejection: %v", err)
	}
	if u.ID != "u1" || u.CommitmentID != "c1" || u.Quantity != 3 {
		t.Fatalf("usage = %+v, want {u1 c1 3}", u)
	}

	// c1 累计已用五件、未用零件（恰好用完），c2 保持原值。
	c1, _ := s.Commitment("c1")
	c2, _ := s.Commitment("c2")
	if c1.Used != 5 || c1.Unused() != 0 || c1.Canceled || c1.Expired {
		t.Fatalf("c1 after retry = %+v, want used=5 unused=0 active", c1)
	}
	if c2.Used != 0 || c2.Unused() != 4 || c2.Canceled || c2.Expired {
		t.Fatalf("c2 changed by retry: %+v", c2)
	}
	// 库存变为实物剩余七件、有效占用四件（仅 c2 未用）、可承诺三件。
	assertExceededRetryStock(t, s, submitAt, 7, 4, 3, 5, 0)
	assertExceededRetryRequests(t, s, submitAt, 5, 0)

	// 成功后再原样提交：取回这次三件的使用记录，数量不再变化。
	again, err := s.Use("u1", "c1", 3, submitAt.Add(day))
	if err != nil || again != u {
		t.Fatalf("idempotent replay = %+v, err %v; want %+v", again, err, u)
	}
	assertExceededRetryStock(t, s, submitAt.Add(day), 7, 4, 3, 5, 0)

	// 沿用该编号改回先前被拒绝的四件：ErrConflict，三件的成功结果保留。
	if u2, err := s.Use("u1", "c1", 4, submitAt.Add(2*day)); !errors.Is(err, ErrConflict) || u2 != (Usage{}) {
		t.Fatalf("reuse id with rejected quantity: usage=%+v err=%v, want ErrConflict with empty result", u2, err)
	}
	assertExceededRetryStock(t, s, submitAt.Add(2*day), 7, 4, 3, 5, 0)
	kept, err := s.Use("u1", "c1", 3, submitAt.Add(3*day))
	if err != nil || kept != u {
		t.Fatalf("successful record after conflict = %+v, err %v; want %+v", kept, err, u)
	}
}
