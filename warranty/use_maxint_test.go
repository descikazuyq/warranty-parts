package warranty

import (
	"errors"
	"math"
	"testing"
)

// 本文件回归保障分批使用在数量接近当前运行环境 int 可表示的最大正整数
// （记为 M）时的数量规则：同一种备件登记 M 件实物，由两张在保且未命中除外
// 规则的请求分别承诺 M−3 件和 3 件后，分批领取既不能让承诺未用数量或实物
// 库存变成负数、丢失一部分，也不能让超过本笔承诺余量的领取意外成功——即使
// 该数量本身可由 int 表示、实物仍有剩余、另一笔承诺尚有未用余量。这里的 M
// 取 math.MaxInt（当前环境 int 可表示的最大正整数），用可表示的数量描述
// 结果，不把库存上限固定成某个平台的具体数值。测试只沿用现有登记、预留、
// 分批使用与查询入口的公开行为，不改变公开入口、错误类别和数量规则。

// 本文件专用编号，避免与其他测试文件混用。
const (
	muProductID = "p-mu"
	muPartID    = "part-mu"
	muReqBig    = "r-mu-big" // 较大承诺所属请求
	muReqSmall  = "r-mu-sml" // 另一笔承诺所属请求
	muCommitBig = "c-mu-big" // 较大承诺：M−3 件
	muCommitSml = "c-mu-sml" // 另一笔承诺：3 件

	muUseFirst   = "u-mu-first" // 首次领取：M−5 件，成功
	muUseBigOver = "u-mu-over"  // 超量申请：M−4 件，必须 ErrUsageExceeded
	muUseSmlOver = "u-mu-three" // 申请 3 件，同样必须 ErrUsageExceeded
	muUseRest    = "u-mu-rest"  // 恰好领取较大承诺剩余 2 件，成功
	muUseSmall   = "u-mu-sml"   // 领取另一笔承诺 3 件，成功
)

// muM 是当前运行环境 int 可表示的最大正整数。
const muM = math.MaxInt

// muUseScenario 构造大数量分批使用主例：产品保修六十天、除外清单不含 NOISE，
// 备件初始库存 M 件；两个请求都关联该产品且故障代码为 NOISE，全程合格。
// 较大承诺 muCommitBig（属于 muReqBig）预留 M−3 件，muCommitSml（属于
// muReqSmall）预留 3 件，两笔承诺均未取消、未到期，未用数量合计恰为 M。
func muUseScenario(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct(muProductID, t0, 60, []string{"FLOOD"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(muPartID, muM); err != nil {
		t.Fatalf("register part with max-int stock: %v", err)
	}
	if err := s.SubmitRequest(muReqBig, muProductID, "NOISE"); err != nil {
		t.Fatalf("submit big request: %v", err)
	}
	if err := s.SubmitRequest(muReqSmall, muProductID, "NOISE"); err != nil {
		t.Fatalf("submit small request: %v", err)
	}
	if _, err := s.Reserve(muCommitBig, muReqBig, muPartID, muM-3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve big commitment M-3: %v", err)
	}
	if _, err := s.Reserve(muCommitSml, muReqSmall, muPartID, 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve small commitment 3: %v", err)
	}
	return s
}

// assertMuAccount 断言按备件查询的账目恰为给定的实物剩余、有效占用与可承诺
// 数量，并返回明细按承诺编号建立的索引。任何一项都不允许为负数。
func assertMuAccount(t *testing.T, s *Store, phys, occupied, committable int) map[string]CommitmentDetail {
	t.Helper()
	st, err := s.PartStatus(muPartID, nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != phys || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("account = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, phys, occupied, committable)
	}
	if st.PhysicalRemaining < 0 || st.ActiveOccupied < 0 || st.Committable < 0 {
		t.Fatalf("negative account figures: %+v", st)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details = %d, want both commitments", len(st.Details))
	}
	byID := make(map[string]CommitmentDetail, len(st.Details))
	for _, d := range st.Details {
		if d.Status != CommitmentActive {
			t.Fatalf("detail %+v, want both commitments active", d)
		}
		byID[d.CommitmentID] = d
	}
	return byID
}

// assertMuCommitment 直接按编号核对承诺的归属、原数量、累计已用与未用数量，
// 且未取消、未到期；未用数量不允许为负。
func assertMuCommitment(t *testing.T, s *Store, id, requestID string, quantity, used int) Commitment {
	t.Helper()
	c, err := s.Commitment(id)
	if err != nil {
		t.Fatalf("commitment %s: %v", id, err)
	}
	if c.RequestID != requestID || c.PartID != muPartID {
		t.Fatalf("commitment %s ownership altered: %+v", id, c)
	}
	if c.Quantity != quantity || c.Used != used || c.Unused() != quantity-used {
		t.Fatalf("commitment %s = qty %d / used %d / unused %d, want %d/%d/%d",
			id, c.Quantity, c.Used, c.Unused(), quantity, used, quantity-used)
	}
	if c.Unused() < 0 {
		t.Fatalf("commitment %s unused went negative: %+v", id, c)
	}
	if c.Canceled || c.Expired {
		t.Fatalf("commitment %s unexpectedly canceled/expired: %+v", id, c)
	}
	return c
}

// TestUseNearMaxIntBatchWithoutOverflow 是用户给定的完整主例：登记 M 件实物，
// 两笔在保请求分别承诺 M−3 件和 3 件；首次从较大承诺领取 M−5 件成功后，
// 实物剩余与有效占用都为 5、可承诺为 0。此时再从较大承诺申请 M−4 件或
// 3 件都必须 ErrUsageExceeded 且整次失败（空使用记录、无部分扣减、不借用
// 另一笔承诺的 3 件预留）；随后领取恰好剩余的 2 件、再领取另一笔承诺的
// 3 件成功，最终实物剩余、有效占用与可承诺数量全部归零，两笔承诺仍可按
// 请求与按备件一致地查到原数量等于累计已用、未用为零。
func TestUseNearMaxIntBatchWithoutOverflow(t *testing.T) {
	s := muUseScenario(t)

	// 预留阶段：实物 M、有效占用 M（M−3+3）、可承诺 0；两笔承诺各自未用。
	assertMuCommitment(t, s, muCommitBig, muReqBig, muM-3, 0)
	assertMuCommitment(t, s, muCommitSml, muReqSmall, 3, 0)
	assertMuAccount(t, s, muM, muM, 0)

	// 第一步：从较大承诺首次领取 M−5 件，成功。返回记录保留本次数量与承诺归属。
	first, err := s.Use(muUseFirst, muCommitBig, muM-5, nowOK)
	if err != nil {
		t.Fatalf("first use M-5: %v", err)
	}
	if first != (Usage{ID: muUseFirst, CommitmentID: muCommitBig, Quantity: muM - 5}) {
		t.Fatalf("first usage = %+v, want {u-mu-first c-mu-big M-5}", first)
	}

	// 较大承诺：原数量仍为 M−3，累计已用 M−5，未用 2；另一笔承诺仍有 3 件未用。
	assertMuCommitment(t, s, muCommitBig, muReqBig, muM-3, muM-5)
	assertMuCommitment(t, s, muCommitSml, muReqSmall, 3, 0)
	// 实物剩余 M-(M-5)=5，有效占用 2+3=5，可承诺 5-5=0：已领走的 M−5 件
	// 不能继续计入占用。
	byID := assertMuAccount(t, s, 5, 5, 0)
	if d := byID[muCommitBig]; d.OriginalQuantity != muM-3 || d.UsedQuantity != muM-5 ||
		d.RemainingQuantity != 2 || d.RequestID != muReqBig {
		t.Fatalf("big detail = %+v, want original M-3 / used M-5 / remaining 2", d)
	}
	if d := byID[muCommitSml]; d.OriginalQuantity != 3 || d.UsedQuantity != 0 ||
		d.RemainingQuantity != 3 || d.RequestID != muReqSmall {
		t.Fatalf("small detail = %+v, want original 3 / used 0 / remaining 3", d)
	}

	// 第二步：从较大承诺申请 M−4 件。M−4 单独可由 int 表示，但该承诺未用仅
	// 2 件；即使实物还剩 5 件、另一笔承诺尚有 3 件预留，也必须整次失败并返回
	// ErrUsageExceeded——不能是参数非法，不能因数量回绕而成功，不能借用另一张
	// 请求预留的数量。
	rejected, err := s.Use(muUseBigOver, muCommitBig, muM-4, nowOK)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use M-4 over unused 2: got %v, want ErrUsageExceeded", err)
	}
	if errors.Is(err, ErrInvalidParam) {
		t.Fatalf("use M-4: got %v, must not be ErrInvalidParam for a representable positive quantity", err)
	}
	if rejected != (Usage{}) {
		t.Fatalf("rejected M-4 use returned non-empty record: %+v", rejected)
	}

	// 整次失败不留记录、不做部分扣减：用同一编号同一内容再次提交仍是一笔新的
	// 超量申请并再次被拒（编号未被失败占用，也没有先扣掉 2 件余量）。
	if again, err := s.Use(muUseBigOver, muCommitBig, muM-4, nowOK); !errors.Is(err, ErrUsageExceeded) || again != (Usage{}) {
		t.Fatalf("repeat rejected M-4: usage=%+v err=%v, want ErrUsageExceeded with empty result", again, err)
	}

	// 第三步：从较大承诺申请 3 件，同样必须 ErrUsageExceeded：数量本身合法、
	// 实物还有 5 件、另一笔承诺恰有 3 件未用，都不能替代该承诺自己的余量限制。
	rejected3, err := s.Use(muUseSmlOver, muCommitBig, 3, nowOK)
	if !errors.Is(err, ErrUsageExceeded) || rejected3 != (Usage{}) {
		t.Fatalf("use 3 over unused 2: usage=%+v err=%v, want ErrUsageExceeded with empty result", rejected3, err)
	}

	// 每次拒绝之后：较大承诺累计已用仍是 M−5、未用 2，另一笔承诺仍有 3 件；
	// 备件账目维持实物 5 / 占用 5 / 可承诺 0，归属不变。
	assertMuCommitment(t, s, muCommitBig, muReqBig, muM-3, muM-5)
	assertMuCommitment(t, s, muCommitSml, muReqSmall, 3, 0)
	assertMuAccount(t, s, 5, 5, 0)

	// 第四步：从较大承诺领取恰好剩余的 2 件，成功。累计已用达到 M−3、未用归零。
	rest, err := s.Use(muUseRest, muCommitBig, 2, nowOK)
	if err != nil {
		t.Fatalf("use remaining 2: %v", err)
	}
	if rest != (Usage{ID: muUseRest, CommitmentID: muCommitBig, Quantity: 2}) {
		t.Fatalf("remaining usage = %+v, want {u-mu-rest c-mu-big 2}", rest)
	}
	assertMuCommitment(t, s, muCommitBig, muReqBig, muM-3, muM-3)
	assertMuCommitment(t, s, muCommitSml, muReqSmall, 3, 0)
	// 实物 5-2=3，有效占用 0+3=3，可承诺 3-3=0。
	assertMuAccount(t, s, 3, 3, 0)

	// 第五步：领取另一笔承诺的全部 3 件，成功。
	small, err := s.Use(muUseSmall, muCommitSml, 3, nowOK)
	if err != nil {
		t.Fatalf("use small commitment 3: %v", err)
	}
	if small != (Usage{ID: muUseSmall, CommitmentID: muCommitSml, Quantity: 3}) {
		t.Fatalf("small usage = %+v, want {u-mu-sml c-mu-sml 3}", small)
	}

	// 两笔承诺都已用尽：原数量与累计已用一致、未用均为零；实物剩余、有效占用
	// 与可承诺数量全部为零，没有任何数量变成负数或丢失。
	bigFinal := assertMuCommitment(t, s, muCommitBig, muReqBig, muM-3, muM-3)
	smlFinal := assertMuCommitment(t, s, muCommitSml, muReqSmall, 3, 3)
	if bigFinal.Unused() != 0 || smlFinal.Unused() != 0 {
		t.Fatalf("unused after exhaustion: big=%d small=%d, want both 0", bigFinal.Unused(), smlFinal.Unused())
	}
	st, err := s.PartStatus(muPartID, nowOK)
	if err != nil {
		t.Fatalf("final part status: %v", err)
	}
	if st.PhysicalRemaining != 0 || st.ActiveOccupied != 0 || st.Committable != 0 {
		t.Fatalf("final account = %d/%d/%d, want 0/0/0",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}

	// 已用尽的承诺不再接受新使用：即使引用的是仍存在的承诺，也按超量整次拒绝。
	if u, err := s.Use("u-mu-extra", muCommitBig, 1, nowOK); !errors.Is(err, ErrUsageExceeded) || u != (Usage{}) {
		t.Fatalf("use on exhausted big commitment: usage=%+v err=%v, want ErrUsageExceeded empty", u, err)
	}
	if u, err := s.Use("u-mu-extra2", muCommitSml, 1, nowOK); !errors.Is(err, ErrUsageExceeded) || u != (Usage{}) {
		t.Fatalf("use on exhausted small commitment: usage=%+v err=%v, want ErrUsageExceeded empty", u, err)
	}

	// 核对：两笔承诺仍能查到；按请求查看的数量与按备件查看的明细一致。
	wantByID := map[string]struct {
		requestID string
		quantity  int
		used      int
	}{
		muCommitBig: {muReqBig, muM - 3, muM - 3},
		muCommitSml: {muReqSmall, 3, 3},
	}
	partDetails := make(map[string]CommitmentDetail, len(st.Details))
	for _, d := range st.Details {
		partDetails[d.CommitmentID] = d
	}
	for commitID, want := range wantByID {
		d, ok := partDetails[commitID]
		if !ok {
			t.Fatalf("part details missing commitment %q: %+v", commitID, st.Details)
		}
		if d.RequestID != want.requestID || d.OriginalQuantity != want.quantity ||
			d.UsedQuantity != want.used || d.RemainingQuantity != 0 {
			t.Fatalf("part detail %q = %+v, want request %s qty %d used %d remaining 0",
				commitID, d, want.requestID, want.quantity, want.used)
		}

		v, err := s.RequestView(want.requestID, nowOK)
		if err != nil {
			t.Fatalf("request view %s: %v", want.requestID, err)
		}
		if len(v.Commitments) != 1 {
			t.Fatalf("request %s commitments = %d, want 1", want.requestID, len(v.Commitments))
		}
		rd := v.Commitments[0]
		if rd.CommitmentID != commitID || rd.RequestID != want.requestID ||
			rd.OriginalQuantity != d.OriginalQuantity || rd.UsedQuantity != d.UsedQuantity ||
			rd.RemainingQuantity != d.RemainingQuantity || rd.Status != d.Status {
			t.Fatalf("request view %+v inconsistent with part detail %+v", rd, d)
		}
	}

	// 成功的使用记录原样重试仍取回首次结果，不再次扣减（账目保持全零）。
	if got, err := s.Use(muUseFirst, muCommitBig, muM-5, nowOK); err != nil || got != first {
		t.Fatalf("first usage retry: got %+v err %v, want %+v", got, err, first)
	}
	if got, err := s.Use(muUseSmall, muCommitSml, 3, nowOK); err != nil || got != small {
		t.Fatalf("small usage retry: got %+v err %v, want %+v", got, err, small)
	}
	st2, err := s.PartStatus(muPartID, nowOK)
	if err != nil {
		t.Fatalf("part status after retries: %v", err)
	}
	if st2.PhysicalRemaining != 0 || st2.ActiveOccupied != 0 || st2.Committable != 0 {
		t.Fatalf("account changed on idempotent retry: %d/%d/%d, want 0/0/0",
			st2.PhysicalRemaining, st2.ActiveOccupied, st2.Committable)
	}
}
