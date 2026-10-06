package warranty

import (
	"errors"
	"math"
	"testing"
	"time"
)

// 本文件回归保障分批使用在数量接近当前运行环境 int 可表示的最大正整数
// （记为 M）时的账目约束：登记 M 件合法库存，并由两张在保且未命中除外
// 规则的请求分别承诺 M−3 件和 3 件（两笔未用合计恰为 M），随后对较大的
// 承诺分批领取。任何成功领取都不能让实物库存、承诺已用/未用数量回绕成
// 负数或丢失一部分；超过本笔承诺未用余量的申请（即使该数量本身可由 int
// 表示、实物仍有剩余、另一笔承诺仍有未用余量）必须按 ErrUsageExceeded
// 整次失败：返回空使用记录，不做部分扣减，也不借用另一张请求预留的数量。
// 这项保障只沿用现有登记、预留、分批使用与查询入口的公开行为，不新增
// 业务操作，也不把库存上限固定成某个平台的数值——M 始终取当前环境 int
// 可表示的最大正整数。

// 本文件专用编号，避免与其他测试文件混用。
const (
	umProductID   = "p-um"
	umPartID      = "part-um"
	umReqBig      = "r-um-big" // 较大承诺所属请求
	umReqSmall    = "r-um-small"
	umCommitBig   = "c-um-big"   // 较大承诺：M−3 件
	umCommitSmall = "c-um-small" // 另一笔承诺：3 件
	umUseFirst    = "u-um-first" // 首次领取：M−5 件，成功
	umUseOver     = "u-um-over"  // 超额申请：M−4 件，必须整次失败
	umUseThree    = "u-um-three" // 超额申请：3 件（余量仅 2 件），必须整次失败
	umUseTwo      = "u-um-two"   // 恰好领取剩余：2 件，成功
	umUseSmallAll = "u-um-small" // 领取另一笔承诺全部：3 件，成功
	umUseAfter    = "u-um-after" // 较大承诺已用尽后再申请：1 件，必须失败
)

// umM 是当前运行环境 int 可表示的最大正整数。
const umM = math.MaxInt

// umClock 汇集本文件的全部时刻：操作在保修期内，承诺到期晚于操作时刻，
// 因此资格拒绝与到期释放不混入这项数量规则。
type umClock struct {
	now    time.Time // 全部预留与使用的当前时刻，保修期内
	expiry time.Time // 全部承诺的到期时刻，晚于 now
}

// umScenario 构造大数量主例：产品保修三十天、除外清单不含 NOISE，备件初始
// 库存 M 件；两个请求都关联该产品且故障代码为 NOISE，全程合格。随后成功
// 预留两笔承诺：较大的 M−3 件（属于 umReqBig）与 3 件（属于 umReqSmall），
// 两笔均未取消、未到期，未用数量合计恰为 M。
func umScenario(t *testing.T) (*Store, umClock) {
	t.Helper()
	clk := umClock{
		now:    t0.Add(10 * day),
		expiry: t0.Add(20 * day),
	}

	s := NewStore()
	if err := s.RegisterProduct(umProductID, t0, 30, []string{"FLOOD"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(umPartID, umM); err != nil {
		t.Fatalf("register part with max-int stock: %v", err)
	}
	if err := s.SubmitRequest(umReqBig, umProductID, "NOISE"); err != nil {
		t.Fatalf("submit big request: %v", err)
	}
	if err := s.SubmitRequest(umReqSmall, umProductID, "NOISE"); err != nil {
		t.Fatalf("submit small request: %v", err)
	}
	if _, err := s.Reserve(umCommitBig, umReqBig, umPartID, umM-3, clk.expiry, clk.now); err != nil {
		t.Fatalf("reserve M-3: %v", err)
	}
	if _, err := s.Reserve(umCommitSmall, umReqSmall, umPartID, 3, clk.expiry, clk.now); err != nil {
		t.Fatalf("reserve 3: %v", err)
	}
	return s, clk
}

// assertUmAccount 断言按备件查询的账目恰为给定的实物剩余、有效占用与可承诺
// 数量——大数量下任何一项都不能回绕成负数。
func assertUmAccount(t *testing.T, s *Store, now time.Time, phys, occupied, committable int) *PartStatus {
	t.Helper()
	st, err := s.PartStatus(umPartID, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != phys || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("account = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, phys, occupied, committable)
	}
	return st
}

// assertUmBigCommitment 断言较大承诺保持原定数量 M−3、归属 umReqBig、
// 到期时刻不变，且已用数量恰为给定值（未用随之确定）。
func assertUmBigCommitment(t *testing.T, s *Store, clk umClock, used int) {
	t.Helper()
	c, err := s.Commitment(umCommitBig)
	if err != nil {
		t.Fatalf("big commitment: %v", err)
	}
	if c.RequestID != umReqBig || c.PartID != umPartID || c.Quantity != umM-3 ||
		c.Used != used || c.Unused() != umM-3-used || c.Canceled || c.Expired ||
		!c.Expiry.Equal(clk.expiry) {
		t.Fatalf("big commitment = %+v, want qty M-3 used %d unused %d", c, used, umM-3-used)
	}
}

// assertUmSmallCommitment 断言另一笔承诺始终保持原定 3 件、归属 umReqSmall，
// 已用数量由调用方给出；被拒绝的超额使用绝不能借用它的余量。
func assertUmSmallCommitment(t *testing.T, s *Store, clk umClock, used int) {
	t.Helper()
	c, err := s.Commitment(umCommitSmall)
	if err != nil {
		t.Fatalf("small commitment: %v", err)
	}
	if c.RequestID != umReqSmall || c.PartID != umPartID || c.Quantity != 3 ||
		c.Used != used || c.Unused() != 3-used || c.Canceled || c.Expired ||
		!c.Expiry.Equal(clk.expiry) {
		t.Fatalf("small commitment = %+v, want qty 3 used %d unused %d", c, used, 3-used)
	}
}

// TestUseNearMaxIntPartialUsageAndExceededRejections 是用户给定的完整主例：
// M 件库存由两笔在保承诺 M−3 件与 3 件占满；首次从较大承诺领取 M−5 件成功
// 后，其实物剩余与有效占用都为 5、可承诺为 0，较大承诺未用仅 2 件、另一笔
// 仍有 3 件。此时无论申请 M−4 件还是 3 件都必须 ErrUsageExceeded 整次失败，
// 数量本身合法不能替代该承诺的余量限制，也不能借用另一张请求的预留；随后
// 恰好领取剩余 2 件、再领取另一笔承诺 3 件成功，全部账目归零且两笔承诺
// 仍可按请求与按备件一致地查到。
func TestUseNearMaxIntPartialUsageAndExceededRejections(t *testing.T) {
	s, clk := umScenario(t)

	// 预留完成时：实物 M，有效占用 M（(M−3)+3），可承诺 0。
	assertUmAccount(t, s, clk.now, umM, umM, 0)

	// 第一步：首次从较大承诺领取 M−5 件，成功。返回的使用记录保留本次
	// 数量与承诺归属。
	first, err := s.Use(umUseFirst, umCommitBig, umM-5, clk.now)
	if err != nil {
		t.Fatalf("use M-5 from M-3 commitment: %v", err)
	}
	if first != (Usage{ID: umUseFirst, CommitmentID: umCommitBig, Quantity: umM - 5}) {
		t.Fatalf("first usage = %+v, want {%s %s M-5}", first, umUseFirst, umCommitBig)
	}

	// 较大承诺的原数量仍为 M−3，累计已用 M−5，未用为 2；另一笔仍有 3 件未用。
	assertUmBigCommitment(t, s, clk, umM-5)
	assertUmSmallCommitment(t, s, clk, 0)

	// 实物剩余与有效占用此时都为 5（未用 2 + 另一笔未用 3），可承诺为 0：
	// 已领走的 M−5 件不能继续计入占用，实物扣减也不能回绕成负数。
	st := assertUmAccount(t, s, clk.now, 5, 5, 0)
	if len(st.Details) != 2 {
		t.Fatalf("details = %+v, want two commitments", st.Details)
	}
	db := mustDetailByID(t, st.Details, umCommitBig)
	if db.Status != CommitmentActive || db.RequestID != umReqBig ||
		db.OriginalQuantity != umM-3 || db.UsedQuantity != umM-5 || db.RemainingQuantity != 2 {
		t.Fatalf("big detail = %+v, want original M-3 / used M-5 / remaining 2 active", db)
	}
	ds := mustDetailByID(t, st.Details, umCommitSmall)
	if ds.Status != CommitmentActive || ds.RequestID != umReqSmall ||
		ds.OriginalQuantity != 3 || ds.UsedQuantity != 0 || ds.RemainingQuantity != 3 {
		t.Fatalf("small detail = %+v, want 3/0/3 active", ds)
	}

	// 第二步：从较大承诺再申请 M−4 件。M−4 单独是可表示的合法正整数，
	// 但该承诺未用仅 2 件：必须返回 ErrUsageExceeded（而不是参数非法或
	// 编号冲突），并整次失败、返回空使用记录。
	rejectOver, err := s.Use(umUseOver, umCommitBig, umM-4, clk.now)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use M-4 over unused 2: got %v, want ErrUsageExceeded", err)
	}
	if errors.Is(err, ErrInvalidParam) {
		t.Fatalf("M-4 is a representable positive quantity, must not be ErrInvalidParam: %v", err)
	}
	if errors.Is(err, ErrConflict) {
		t.Fatalf("business rejection must not be reported as ErrConflict: %v", err)
	}
	if rejectOver != (Usage{}) {
		t.Fatalf("rejected M-4 use returned non-empty record: %+v", rejectOver)
	}

	// 整次失败不留任何变化：累计已用仍是 M−5，账目仍是 5/5/0，另一笔
	// 承诺的 3 件预留没有被借用，也没有部分扣减。
	assertUmBigCommitment(t, s, clk, umM-5)
	assertUmSmallCommitment(t, s, clk, 0)
	assertUmAccount(t, s, clk.now, 5, 5, 0)

	// 第三步：申请 3 件也必须得到同一错误——尽管实物还有 5 件、另一笔
	// 承诺尚有 3 件未用，数量本身合法不能替代该承诺的余量限制。
	rejectThree, err := s.Use(umUseThree, umCommitBig, 3, clk.now)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use 3 over unused 2: got %v, want ErrUsageExceeded", err)
	}
	if rejectThree != (Usage{}) {
		t.Fatalf("rejected 3-piece use returned non-empty record: %+v", rejectThree)
	}

	// 第二次拒绝同样整次失败：之前的累计已用、两笔承诺归属与备件账目
	// 全部保留，被拒绝编号不产生使用记录（沿用该编号改指任何新内容都不
	// 是冲突重试；这里直接验证账目与余量未被触及）。
	assertUmBigCommitment(t, s, clk, umM-5)
	assertUmSmallCommitment(t, s, clk, 0)
	st = assertUmAccount(t, s, clk.now, 5, 5, 0)
	if len(st.Details) != 2 {
		t.Fatalf("details after rejections = %+v, want still two commitments", st.Details)
	}

	// 第四步：从较大承诺领取恰好剩余的 2 件，成功。
	tail, err := s.Use(umUseTwo, umCommitBig, 2, clk.now)
	if err != nil {
		t.Fatalf("use exact remaining 2: %v", err)
	}
	if tail != (Usage{ID: umUseTwo, CommitmentID: umCommitBig, Quantity: 2}) {
		t.Fatalf("tail usage = %+v, want {%s %s 2}", tail, umUseTwo, umCommitBig)
	}

	// 较大承诺累计已用达到 M−3、未用归零；实物剩余与有效占用均为 3
	// （只剩另一笔承诺的未用 3 件），可承诺仍为 0。
	assertUmBigCommitment(t, s, clk, umM-3)
	assertUmSmallCommitment(t, s, clk, 0)
	assertUmAccount(t, s, clk.now, 3, 3, 0)

	// 第五步：领取另一笔承诺的全部 3 件，成功；不能因为它曾是“别人的
	// 预留”而被前面的超额申请吞掉。
	smallUse, err := s.Use(umUseSmallAll, umCommitSmall, 3, clk.now)
	if err != nil {
		t.Fatalf("use all 3 of small commitment: %v", err)
	}
	if smallUse != (Usage{ID: umUseSmallAll, CommitmentID: umCommitSmall, Quantity: 3}) {
		t.Fatalf("small usage = %+v, want {%s %s 3}", smallUse, umUseSmallAll, umCommitSmall)
	}

	// 实物剩余、有效占用和可承诺数量全部为 0，不能为负。
	assertUmBigCommitment(t, s, clk, umM-3)
	assertUmSmallCommitment(t, s, clk, 3)
	st = assertUmAccount(t, s, clk.now, 0, 0, 0)

	// 已全部使用的较大承诺不再接受新使用：1 件虽小，仍按该承诺余量为 0
	// 返回 ErrUsageExceeded，空记录且账目不变。
	rejectAfter, err := s.Use(umUseAfter, umCommitBig, 1, clk.now)
	if !errors.Is(err, ErrUsageExceeded) {
		t.Fatalf("use 1 on fully used commitment: got %v, want ErrUsageExceeded", err)
	}
	if rejectAfter != (Usage{}) {
		t.Fatalf("post-exhaustion use returned non-empty record: %+v", rejectAfter)
	}
	assertUmAccount(t, s, clk.now, 0, 0, 0)

	// 核对：两笔承诺仍能查到，原数量与累计已用一致，未用均为 0。
	big, err := s.Commitment(umCommitBig)
	if err != nil {
		t.Fatalf("lookup big commitment: %v", err)
	}
	if big.Quantity != umM-3 || big.Used != umM-3 || big.Unused() != 0 {
		t.Fatalf("big commitment final = %+v, want original=used=M-3 unused=0", big)
	}
	small, err := s.Commitment(umCommitSmall)
	if err != nil {
		t.Fatalf("lookup small commitment: %v", err)
	}
	if small.Quantity != 3 || small.Used != 3 || small.Unused() != 0 {
		t.Fatalf("small commitment final = %+v, want original=used=3 unused=0", small)
	}

	// 按备件查看的明细同样是原数量等于已用、未用为 0，且归属不变。
	if len(st.Details) != 2 {
		t.Fatalf("final details = %+v, want two commitments", st.Details)
	}
	fb := mustDetailByID(t, st.Details, umCommitBig)
	if fb.RequestID != umReqBig || fb.OriginalQuantity != umM-3 ||
		fb.UsedQuantity != umM-3 || fb.RemainingQuantity != 0 || fb.Status != CommitmentActive {
		t.Fatalf("final big detail = %+v, want M-3/M-3/0 active", fb)
	}
	fs := mustDetailByID(t, st.Details, umCommitSmall)
	if fs.RequestID != umReqSmall || fs.OriginalQuantity != 3 ||
		fs.UsedQuantity != 3 || fs.RemainingQuantity != 0 || fs.Status != CommitmentActive {
		t.Fatalf("final small detail = %+v, want 3/3/0 active", fs)
	}

	// 按请求查看的数量和按备件查看的明细一致，每笔承诺只归属于自己的请求。
	viewBig, err := s.RequestView(umReqBig, clk.now)
	if err != nil {
		t.Fatalf("big request view: %v", err)
	}
	if len(viewBig.Commitments) != 1 {
		t.Fatalf("big request commitments = %+v, want only the M-3 commitment", viewBig.Commitments)
	}
	vb := viewBig.Commitments[0]
	if vb.CommitmentID != umCommitBig || vb.OriginalQuantity != fb.OriginalQuantity ||
		vb.UsedQuantity != fb.UsedQuantity || vb.RemainingQuantity != fb.RemainingQuantity {
		t.Fatalf("big request view %+v inconsistent with part detail %+v", vb, fb)
	}
	viewSmall, err := s.RequestView(umReqSmall, clk.now)
	if err != nil {
		t.Fatalf("small request view: %v", err)
	}
	if len(viewSmall.Commitments) != 1 {
		t.Fatalf("small request commitments = %+v, want only the 3 commitment", viewSmall.Commitments)
	}
	vs := viewSmall.Commitments[0]
	if vs.CommitmentID != umCommitSmall || vs.OriginalQuantity != fs.OriginalQuantity ||
		vs.UsedQuantity != fs.UsedQuantity || vs.RemainingQuantity != fs.RemainingQuantity {
		t.Fatalf("small request view %+v inconsistent with part detail %+v", vs, fs)
	}

	// 三次成功的使用记录仍可按原编号原样取回，数量与承诺归属保持首次结果，
	// 取回不再次扣减（账目维持 0/0/0）。
	if got, err := s.Use(umUseFirst, umCommitBig, umM-5, clk.now); err != nil || got != first {
		t.Fatalf("first usage record lost: got %+v err %v, want %+v", got, err, first)
	}
	if got, err := s.Use(umUseTwo, umCommitBig, 2, clk.now); err != nil || got != tail {
		t.Fatalf("tail usage record lost: got %+v err %v, want %+v", got, err, tail)
	}
	if got, err := s.Use(umUseSmallAll, umCommitSmall, 3, clk.now); err != nil || got != smallUse {
		t.Fatalf("small usage record lost: got %+v err %v, want %+v", got, err, smallUse)
	}
	assertUmAccount(t, s, clk.now, 0, 0, 0)
}
