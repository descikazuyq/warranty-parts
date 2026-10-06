package warranty

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归保障首次预留同时遇到保修资格不合格与可承诺数量不足时的拒绝结果
// 归属：调用方需要据此区分“请求不能保修”和“备件暂时不够”，不能因为可承诺
// 数量较少，就把过保或命中除外清单的请求记录成缺货。主例为：备件初始十件，
// 先为合格请求预留八件并成功使用两件，剩余实物八件、有效占用六件、可承诺
// 两件，原承诺尚未到期也未取消；此时另一张已知请求以从未预留成功的编号申请
// 三件（数量为正、到期时刻晚于本次当前时刻、产品与备件均已登记），若请求
// 已过保或故障代码命中除外清单，首次预留必须返回 ErrIneligible 而非
// ErrInsufficientStock，过保与除外同时成立时两项拒绝原因都要保留。
//
// 拒绝后不得产生新承诺：原承诺的已用与未用数量、备件三项账目保持原值；
// 该请求的处理历史新增一条类别为 ineligible 的失败记录，保存本次提交的数量
// 与时刻，并留下实际取得的资格依据（与按同一时刻直接查询 Evaluate 的结论
// 一致）和库存依据（处理前的八/六/二，不因拒绝变成空依据或零库存，也不混入
// 申请的三件占用）。可承诺恰好为零、当前时刻恰好达到保修截止时刻同样不能让
// 库存不足覆盖资格拒绝；作为对照，在保且未命中除外的请求在同样库存不足时仍
// 返回 ErrInsufficientStock，历史保存合格资格与真实库存依据。这些检查只
// 围绕尚未成功预留的编号展开，已成功编号原样重试仍取回首次承诺快照，本次不
// 增加任何新的预留规则。

// 本文件专用编号，避免与其他测试文件的包级常量混用。
const (
	ipProductID  = "p-iprio"
	ipPartID     = "part-iprio"
	ipBaseReq    = "r-ip-base"
	ipBaseCommit = "c-ip-base"
	ipBaseUsage  = "u-ip-base"
)

// ipClock 汇集本文件全部时刻：购买时刻 t0、保修三十天，首笔预留发生在保修
// 期内第十天，新申请的到期时刻固定在第四十天（晚于下列所有当前时刻）。
type ipClock struct {
	deadline   time.Time // 保修截止时刻：t0 起第三十天，达到即算过保
	reserveAt  time.Time // 首笔合格预留与两件使用的当前时刻（第十天）
	withinAt   time.Time // 保修期内的当前时刻（第十五天）
	deadlineAt time.Time // 恰好达到保修截止时刻（第三十天）
	expiredAt  time.Time // 已过保的当前时刻（第三十一天）
	baseExpiry time.Time // 首笔承诺到期时刻（第六十天），全部检查中仍有效
	newExpiry  time.Time // 新申请给定的承诺到期时刻（第四十天），晚于各当前时刻
}

func ipNewClock() ipClock {
	return ipClock{
		deadline:   t0.Add(30 * day),
		reserveAt:  t0.Add(10 * day),
		withinAt:   t0.Add(15 * day),
		deadlineAt: t0.Add(30 * day),
		expiredAt:  t0.Add(31 * day),
		baseExpiry: t0.Add(60 * day),
		newExpiry:  t0.Add(40 * day),
	}
}

// ipSetupStore 构造主例库存形态：登记保修三十天、BROKEN_SEAL 在除外清单中
// 的产品与十件备件，为合格请求 r-ip-base（故障 NOISE）预留 firstQty 件，再
// 成功使用两件。firstQty=8 时账目为实物八、占用六、可承诺二；firstQty=10
// 时账目为实物八、占用八、可承诺零。两种形态下首笔承诺在 clk 的各个当前
// 时刻都尚未到期、未取消。
func ipSetupStore(t *testing.T, firstQty int) (*Store, ipClock) {
	t.Helper()
	clk := ipNewClock()
	s := NewStore()
	if err := s.RegisterProduct(ipProductID, t0, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(ipPartID, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(ipBaseReq, ipProductID, "NOISE"); err != nil {
		t.Fatalf("submit base request: %v", err)
	}
	if _, err := s.Reserve(ipBaseCommit, ipBaseReq, ipPartID, firstQty, clk.baseExpiry, clk.reserveAt); err != nil {
		t.Fatalf("reserve base commitment of %d: %v", firstQty, err)
	}
	if _, err := s.Use(ipBaseUsage, ipBaseCommit, 2, clk.reserveAt); err != nil {
		t.Fatalf("use two against base commitment: %v", err)
	}
	return s, clk
}

// ipAssertAccounts 断言备件三项账目与期望完全一致。
func ipAssertAccounts(t *testing.T, s *Store, now time.Time, want StockBasis) {
	t.Helper()
	st, err := s.PartStatus(ipPartID, now)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != want.PhysicalRemaining ||
		st.ActiveOccupied != want.ActiveOccupied ||
		st.Committable != want.Committable {
		t.Fatalf("accounts = phys %d / occupied %d / committable %d, want %d/%d/%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable,
			want.PhysicalRemaining, want.ActiveOccupied, want.Committable)
	}
}

// ipAssertBaseCommitmentUntouched 断言首笔承诺仍为预留 firstQty 件、已用
// 两件、未用 firstQty-2 件，未取消、未到期，拒绝没有改写它。
func ipAssertBaseCommitmentUntouched(t *testing.T, s *Store, clk ipClock, firstQty int) {
	t.Helper()
	c, err := s.Commitment(ipBaseCommit)
	if err != nil {
		t.Fatalf("base commitment missing: %v", err)
	}
	if c.Quantity != firstQty || c.Used != 2 || c.Unused() != firstQty-2 {
		t.Fatalf("base commitment quantities = qty %d / used %d / unused %d, want %d/2/%d",
			c.Quantity, c.Used, c.Unused(), firstQty, firstQty-2)
	}
	if c.Canceled || c.Expired {
		t.Fatalf("base commitment closed by rejection: canceled=%v expired=%v", c.Canceled, c.Expired)
	}
	if c.Status(clk.expiredAt) != CommitmentActive {
		t.Fatalf("base commitment status = %q, want active", c.Status(clk.expiredAt))
	}
}

// ipAssertEligibilityMatchesQuery 断言历史中的资格依据与按同一时刻直接调用
// Evaluate 取得的结论逐字段一致（含拒绝原因的内容与次序）。
func ipAssertEligibilityMatchesQuery(t *testing.T, s *Store, now time.Time, got *Eligibility, reqID, faultCode string) {
	t.Helper()
	if got == nil {
		t.Fatal("eligibility snapshot is nil, want the actual eligibility basis")
	}
	want, err := s.Evaluate(reqID, now)
	if err != nil {
		t.Fatalf("evaluate at same time: %v", err)
	}
	if got.RequestID != want.RequestID || got.ProductID != want.ProductID ||
		got.FaultCode != want.FaultCode || got.FaultCode != faultCode {
		t.Fatalf("eligibility identity = %q/%q/%q, want %q/%q/%q",
			got.RequestID, got.ProductID, got.FaultCode,
			want.RequestID, want.ProductID, faultCode)
	}
	if got.Eligible != want.Eligible || got.Excluded != want.Excluded {
		t.Fatalf("eligibility flags = eligible %v / excluded %v, want %v / %v",
			got.Eligible, got.Excluded, want.Eligible, want.Excluded)
	}
	if len(got.Reasons) != len(want.Reasons) {
		t.Fatalf("reasons = %v, want %v", got.Reasons, want.Reasons)
	}
	for i := range want.Reasons {
		if got.Reasons[i] != want.Reasons[i] {
			t.Fatalf("reason[%d] = %q, want %q (full got %v want %v)",
				i, got.Reasons[i], want.Reasons[i], got.Reasons, want.Reasons)
		}
	}
	if !got.PurchaseTime.Equal(want.PurchaseTime) || got.WarrantyDays != want.WarrantyDays ||
		!got.WarrantyExpiry.Equal(want.WarrantyExpiry) {
		t.Fatalf("eligibility terms = purchase %v / days %d / expiry %v, want %v / %d / %v",
			got.PurchaseTime, got.WarrantyDays, got.WarrantyExpiry,
			want.PurchaseTime, want.WarrantyDays, want.WarrantyExpiry)
	}
}

// ipAssertFailureRecord 断言该请求只有一条失败历史，且完整保留本次提交内容、
// 失败类别、资格依据与处理前库存依据。
func ipAssertFailureRecord(t *testing.T, s *Store, clk ipClock, rec HistoryRecord,
	reqID, commitID, faultCode string, qty int, now time.Time,
	wantError HistoryError, wantBasis StockBasis) {
	t.Helper()
	if rec.Seq != 1 || rec.Success {
		t.Fatalf("record result = seq %d success %v, want seq 1 failure", rec.Seq, rec.Success)
	}
	if rec.Error != wantError {
		t.Fatalf("record error category = %q, want %q", rec.Error, wantError)
	}
	if rec.CommitID != commitID || rec.PartID != ipPartID || rec.Quantity != qty {
		t.Fatalf("record submission = %q/%q/%d, want %q/%q/%d",
			rec.CommitID, rec.PartID, rec.Quantity, commitID, ipPartID, qty)
	}
	if !rec.Expiry.Equal(clk.newExpiry) || !rec.Now.Equal(now) {
		t.Fatalf("record times = expiry %v / now %v, want %v / %v",
			rec.Expiry, rec.Now, clk.newExpiry, now)
	}
	ipAssertEligibilityMatchesQuery(t, s, now, rec.Eligibility, reqID, faultCode)
	if rec.StockBasis == nil {
		t.Fatal("stock basis is nil, want the pre-processing stock basis")
	}
	if *rec.StockBasis != wantBasis {
		t.Fatalf("stock basis = %+v, want %+v", rec.StockBasis, wantBasis)
	}
}

// ipAssertBaseHistoryUntouched 断言首笔合格请求下仍只有那一条首次预留成功
// 记录：其他请求的失败不能挂到它名下，成功编号的原样重试也不追加历史。
func ipAssertBaseHistoryUntouched(t *testing.T, s *Store, firstQty int) {
	t.Helper()
	hist, err := s.RequestHistory(ipBaseReq)
	if err != nil {
		t.Fatalf("base request history: %v", err)
	}
	if len(hist) != 1 || !hist[0].Success || hist[0].Seq != 1 ||
		hist[0].CommitID != ipBaseCommit || hist[0].Quantity != firstQty {
		t.Fatalf("base request history altered: %+v", hist)
	}
}

// TestIneligibleOutranksInsufficientStock 主例矩阵：账目八/六/二、申请三件
// （可承诺只有两件）的同一使用条件下，分别覆盖仅过保、仅命中除外、过保且
// 命中除外、当前时刻恰好达到保修截止时刻四种资格情形，全部必须返回
// ErrIneligible，不能返回 ErrInsufficientStock，并满足拒绝后的全部不留痕
// 占用与历史依据要求。
func TestIneligibleOutranksInsufficientStock(t *testing.T) {
	cases := []struct {
		name        string
		reqID       string
		commitID    string
		faultCode   string
		now         time.Time
		wantReasons []RejectionReason
	}{
		{
			name:        "expired_only",
			reqID:       "r-ip-exp",
			commitID:    "c-ip-exp",
			faultCode:   "NOISE",
			now:         ipNewClock().expiredAt,
			wantReasons: []RejectionReason{ReasonWarrantyExpired},
		},
		{
			name:        "excluded_only_within_warranty",
			reqID:       "r-ip-exc",
			commitID:    "c-ip-exc",
			faultCode:   "BROKEN_SEAL",
			now:         ipNewClock().withinAt,
			wantReasons: []RejectionReason{ReasonFaultExcluded},
		},
		{
			name:        "expired_and_excluded",
			reqID:       "r-ip-both",
			commitID:    "c-ip-both",
			faultCode:   "BROKEN_SEAL",
			now:         ipNewClock().expiredAt,
			wantReasons: []RejectionReason{ReasonWarrantyExpired, ReasonFaultExcluded},
		},
		{
			name:        "now_equals_warranty_deadline",
			reqID:       "r-ip-dead",
			commitID:    "c-ip-dead",
			faultCode:   "NOISE",
			now:         ipNewClock().deadlineAt,
			wantReasons: []RejectionReason{ReasonWarrantyExpired},
		},
	}

	const firstQty = 8
	wantBasis := StockBasis{PhysicalRemaining: 8, ActiveOccupied: 6, Committable: 2}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := ipSetupStore(t, firstQty)
			// 前置条件核对：原承诺未到期未取消，账目八/六/二。
			ipAssertAccounts(t, s, tc.now, wantBasis)
			if err := s.SubmitRequest(tc.reqID, ipProductID, tc.faultCode); err != nil {
				t.Fatalf("submit request: %v", err)
			}
			// 该承诺编号从未预留成功过。
			if _, err := s.Commitment(tc.commitID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("precondition: commitment %q already exists: %v", tc.commitID, err)
			}

			_, err := s.Reserve(tc.commitID, tc.reqID, ipPartID, 3, clk.newExpiry, tc.now)
			if !errors.Is(err, ErrIneligible) {
				t.Fatalf("reserve err = %v, want ErrIneligible", err)
			}
			if errors.Is(err, ErrInsufficientStock) {
				t.Fatalf("ineligible request recorded as insufficient stock: %v", err)
			}

			// 拒绝不产生新承诺，申请的三件不进入占用。
			if _, err := s.Commitment(tc.commitID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rejected commitment %q created: %v", tc.commitID, err)
			}
			ipAssertAccounts(t, s, tc.now, wantBasis)
			ipAssertBaseCommitmentUntouched(t, s, clk, firstQty)

			// 历史新增一条 ineligible 失败记录，依据为真实资格与处理前八/六/二。
			hist, err := s.RequestHistory(tc.reqID)
			if err != nil {
				t.Fatalf("request history: %v", err)
			}
			if len(hist) != 1 {
				t.Fatalf("history length = %d, want exactly 1 failure record", len(hist))
			}
			ipAssertFailureRecord(t, s, clk, hist[0], tc.reqID, tc.commitID,
				tc.faultCode, 3, tc.now, HistoryErrorIneligible, wantBasis)
			if hist[0].Eligibility.Eligible || len(hist[0].Eligibility.Reasons) != len(tc.wantReasons) {
				t.Fatalf("saved eligibility = %+v, want ineligible with %v",
					hist[0].Eligibility, tc.wantReasons)
			}
			for i, want := range tc.wantReasons {
				if hist[0].Eligibility.Reasons[i] != want {
					t.Fatalf("saved reason[%d] = %q, want %q", i, hist[0].Eligibility.Reasons[i], want)
				}
			}
			ipAssertBaseHistoryUntouched(t, s, firstQty)
		})
	}
}

// TestIneligibleOutranksZeroCommittable 覆盖可承诺数量恰好为零的相同使用
// 条件：首笔预留十件、使用两件后账目为八/八/零，过保且命中除外的请求申请
// 三件仍必须按资格拒绝（ErrIneligible、两项原因保留），库存依据是真实的
// 八/八/零，而不是空依据，账目与原承诺保持不变。
func TestIneligibleOutranksZeroCommittable(t *testing.T) {
	const firstQty = 10
	wantBasis := StockBasis{PhysicalRemaining: 8, ActiveOccupied: 8, Committable: 0}
	const (
		reqID    = "r-ip-zero"
		commitID = "c-ip-zero"
	)

	s, clk := ipSetupStore(t, firstQty)
	ipAssertAccounts(t, s, clk.expiredAt, wantBasis)
	if err := s.SubmitRequest(reqID, ipProductID, "BROKEN_SEAL"); err != nil {
		t.Fatalf("submit request: %v", err)
	}

	_, err := s.Reserve(commitID, reqID, ipPartID, 3, clk.newExpiry, clk.expiredAt)
	if !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve at zero committable err = %v, want ErrIneligible", err)
	}
	if errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("ineligible request at zero committable recorded as stock-out: %v", err)
	}

	if _, err := s.Commitment(commitID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected commitment %q created: %v", commitID, err)
	}
	ipAssertAccounts(t, s, clk.expiredAt, wantBasis)
	ipAssertBaseCommitmentUntouched(t, s, clk, firstQty)

	hist, err := s.RequestHistory(reqID)
	if err != nil {
		t.Fatalf("request history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history length = %d, want 1", len(hist))
	}
	ipAssertFailureRecord(t, s, clk, hist[0], reqID, commitID,
		"BROKEN_SEAL", 3, clk.expiredAt, HistoryErrorIneligible, wantBasis)
	got := hist[0].Eligibility
	if got.Eligible || !got.Excluded || len(got.Reasons) != 2 ||
		got.Reasons[0] != ReasonWarrantyExpired || got.Reasons[1] != ReasonFaultExcluded {
		t.Fatalf("saved eligibility = %+v, want both rejection reasons retained", got)
	}
	ipAssertBaseHistoryUntouched(t, s, firstQty)
}

// TestEligibleShortStockStillReturnsInsufficientStock 对照：产品在保且故障
// 未被除外的请求在同样库存不足（申请三件而可承诺为二，或可承诺恰好为零）
// 时，仍按现有规则返回 ErrInsufficientStock；历史类别为 insufficient_stock，
// 保存合格资格依据与真实库存依据，不产生承诺、不改变账目。
func TestEligibleShortStockStillReturnsInsufficientStock(t *testing.T) {
	cases := []struct {
		name      string
		firstQty  int
		wantBasis StockBasis
	}{
		{
			name:      "committable_two_need_three",
			firstQty:  8,
			wantBasis: StockBasis{PhysicalRemaining: 8, ActiveOccupied: 6, Committable: 2},
		},
		{
			name:      "committable_zero_need_three",
			firstQty:  10,
			wantBasis: StockBasis{PhysicalRemaining: 8, ActiveOccupied: 8, Committable: 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := ipSetupStore(t, tc.firstQty)
			reqID := "r-ip-ok-" + tc.name
			commitID := "c-ip-ok-" + tc.name
			ipAssertAccounts(t, s, clk.withinAt, tc.wantBasis)
			if err := s.SubmitRequest(reqID, ipProductID, "NOISE"); err != nil {
				t.Fatalf("submit request: %v", err)
			}

			_, err := s.Reserve(commitID, reqID, ipPartID, 3, clk.newExpiry, clk.withinAt)
			if !errors.Is(err, ErrInsufficientStock) {
				t.Fatalf("eligible short-stock reserve err = %v, want ErrInsufficientStock", err)
			}
			if errors.Is(err, ErrIneligible) {
				t.Fatalf("eligible request recorded as ineligible: %v", err)
			}

			if _, err := s.Commitment(commitID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rejected commitment %q created: %v", commitID, err)
			}
			ipAssertAccounts(t, s, clk.withinAt, tc.wantBasis)
			ipAssertBaseCommitmentUntouched(t, s, clk, tc.firstQty)

			hist, err := s.RequestHistory(reqID)
			if err != nil {
				t.Fatalf("request history: %v", err)
			}
			if len(hist) != 1 {
				t.Fatalf("history length = %d, want 1", len(hist))
			}
			ipAssertFailureRecord(t, s, clk, hist[0], reqID, commitID,
				"NOISE", 3, clk.withinAt, HistoryErrorInsufficientStock, tc.wantBasis)
			if !hist[0].Eligibility.Eligible || hist[0].Eligibility.Excluded ||
				len(hist[0].Eligibility.Reasons) != 0 {
				t.Fatalf("saved eligibility = %+v, want eligible with no rejection reasons",
					hist[0].Eligibility)
			}
			ipAssertBaseHistoryUntouched(t, s, tc.firstQty)
		})
	}
}

// TestSuccessfulCommitRetryUnaffectedByIneligibilityAndShortStock 围绕尚未
// 成功预留编号的这些检查不能改变已成功编号原样重试的既有结果：首笔承诺在
// 保修期内预留八件、使用两件后，待到请求本身已经过保（第三十一天）且可
// 承诺只有两件、不足八件时，用同一编号原样重试仍取回首次成功时的承诺快照
// （已用数量为零、未取消），不重新判断资格与库存、不追加历史、不改变仓库
// 中承诺的实际已用数量与备件账目。
func TestSuccessfulCommitRetryUnaffectedByIneligibilityAndShortStock(t *testing.T) {
	const firstQty = 8
	wantBasis := StockBasis{PhysicalRemaining: 8, ActiveOccupied: 6, Committable: 2}
	s, clk := ipSetupStore(t, firstQty)

	// 重试前的实际状态：请求此刻已过保、库存不足以容纳八件。
	direct, err := s.Evaluate(ipBaseReq, clk.expiredAt)
	if err != nil {
		t.Fatalf("evaluate base request after warranty expiry: %v", err)
	}
	if direct.Eligible {
		t.Fatalf("precondition: base request should be out of warranty at %v", clk.expiredAt)
	}
	ipAssertAccounts(t, s, clk.expiredAt, wantBasis)

	got, err := s.Reserve(ipBaseCommit, ipBaseReq, ipPartID, firstQty, clk.baseExpiry, clk.expiredAt)
	if err != nil {
		t.Fatalf("same-content retry of successful commitment: %v", err)
	}
	if got.ID != ipBaseCommit || got.Quantity != firstQty || got.Used != 0 || got.Canceled {
		t.Fatalf("retry snapshot = %+v, want first-success snapshot (qty %d, used 0, not canceled)",
			got, firstQty)
	}
	if !got.Expiry.Equal(clk.baseExpiry) {
		t.Fatalf("retry snapshot expiry = %v, want %v", got.Expiry, clk.baseExpiry)
	}

	// 快照不反映后来的使用：仓库中的真实承诺仍是已用两件、未用六件。
	real, err := s.Commitment(ipBaseCommit)
	if err != nil {
		t.Fatalf("load real commitment: %v", err)
	}
	if real.Quantity != firstQty || real.Used != 2 || real.Unused() != firstQty-2 {
		t.Fatalf("real commitment after retry = qty %d / used %d / unused %d, want %d/2/%d",
			real.Quantity, real.Used, real.Unused(), firstQty, firstQty-2)
	}
	ipAssertAccounts(t, s, clk.expiredAt, wantBasis)
	ipAssertBaseHistoryUntouched(t, s, firstQty)
}
