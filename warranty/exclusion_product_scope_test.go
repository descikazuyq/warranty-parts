package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障“除外清单只作用于所属产品”：多个产品可以共用同一种备件，
// 某个产品把一个故障代码列为除外，不能让另一产品的同代码请求也被拒绝。
// 测试沿用既有的产品/备件/请求登记、资格查询（Evaluate、RequestView）、
// 预留（Reserve）、备件查询（PartStatus）与预留历史（RequestHistory），
// 覆盖资格判断与实际提交预留两个环节，并覆盖首次登记时除外清单重复列出
// 同一代码的情形。这些测试不改变除外匹配方式与任何错误语义。
//
// 主例在同一仓库中登记两件购买时刻相同、保修期均为三十天的产品，操作时刻
// 在购买之后且尚未过保：
//   - p-eps-one 只除外 BROKEN_SEAL；
//   - p-eps-two 只除外 WATER_DAMAGE。
//
// 两件产品共用库存十件的同一种备件；四件请求分别是“产品 × 故障”的四种
// 组合，各用不同承诺编号申请三件。

const (
	epsPartID = "part-eps"
	epsP1     = "p-eps-one"
	epsP2     = "p-eps-two"

	epsReq1Seal  = "r-eps-one-seal"
	epsReq1Water = "r-eps-one-water"
	epsReq2Seal  = "r-eps-two-seal"
	epsReq2Water = "r-eps-two-water"

	epsCommit1Seal  = "c-eps-one-seal"
	epsCommit1Water = "c-eps-one-water"
	epsCommit2Seal  = "c-eps-two-seal"
	epsCommit2Water = "c-eps-two-water"

	epsFaultSeal  = "BROKEN_SEAL"
	epsFaultWater = "WATER_DAMAGE"
)

var (
	epsDeadline = t0.Add(30 * day) // 两件产品共同的保修截止时刻
	epsExpiry   = t0.Add(40 * day) // 承诺到期时刻，晚于操作时刻 nowOK
)

// epsScenario 登记两件保修三十天、除外清单互不相同的产品与十件共用备件，
// 并提交“产品 × 故障”的四张请求（不做任何预留）。
func epsScenario(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct(epsP1, t0, 30, []string{epsFaultSeal}); err != nil {
		t.Fatalf("register %s: %v", epsP1, err)
	}
	if err := s.RegisterProduct(epsP2, t0, 30, []string{epsFaultWater}); err != nil {
		t.Fatalf("register %s: %v", epsP2, err)
	}
	if err := s.RegisterPart(epsPartID, 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, tc := range []struct{ id, product, fault string }{
		{epsReq1Seal, epsP1, epsFaultSeal},
		{epsReq1Water, epsP1, epsFaultWater},
		{epsReq2Seal, epsP2, epsFaultSeal},
		{epsReq2Water, epsP2, epsFaultWater},
	} {
		if err := s.SubmitRequest(tc.id, tc.product, tc.fault); err != nil {
			t.Fatalf("submit request %s: %v", tc.id, err)
		}
	}
	return s
}

// epsExpectations 描述每张请求在所属产品上的资格预期：同一故障在两件产品
// 上的结果必须相反。
var epsExpectations = []struct {
	reqID        string
	productID    string
	fault        string
	wantEligible bool
}{
	{epsReq1Seal, epsP1, epsFaultSeal, false},   // 命中 p-eps-one 自己的清单
	{epsReq1Water, epsP1, epsFaultWater, true},  // 仅在另一产品清单中
	{epsReq2Seal, epsP2, epsFaultSeal, true},    // 仅在另一产品清单中
	{epsReq2Water, epsP2, epsFaultWater, false}, // 命中 p-eps-two 自己的清单
}

// epsAssertEligibility 断言一份资格依据属于指定请求关联的产品：归属编号与
// 故障代码保持原值，购买时刻、保修天数与截止时刻来自该产品；合格与否、
// 除外标记与拒绝原因符合该产品自己的清单。
func epsAssertEligibility(t *testing.T, e *Eligibility, reqID, productID, fault string, wantEligible bool) {
	t.Helper()
	if e == nil {
		t.Fatalf("%s: eligibility is nil", reqID)
	}
	if e.RequestID != reqID || e.ProductID != productID || e.FaultCode != fault {
		t.Fatalf("%s: identity = %q/%q/%q, want %q/%q/%q",
			reqID, e.RequestID, e.ProductID, e.FaultCode, reqID, productID, fault)
	}
	if !e.PurchaseTime.Equal(t0) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(epsDeadline) {
		t.Fatalf("%s: basis times = purchase %v days %d expiry %v, want %v / 30 / %v",
			reqID, e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, t0, epsDeadline)
	}
	if e.Eligible != wantEligible {
		t.Fatalf("%s: eligible = %v, want %v", reqID, e.Eligible, wantEligible)
	}
	if wantEligible {
		if e.Excluded || len(e.Reasons) != 0 {
			t.Fatalf("%s: eligible request got excluded=%v reasons=%v", reqID, e.Excluded, e.Reasons)
		}
		return
	}
	if !e.Excluded {
		t.Fatalf("%s: excluded = false, want true", reqID)
	}
	if len(e.Reasons) != 1 || e.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("%s: reasons = %v, want exactly [fault_code_excluded]", reqID, e.Reasons)
	}
}

// TestExclusionListsScopedToOwningProductForRequests 四张请求都应保存成功，
// 取回时产品编号和故障代码保持原值——保存成功本身不代表资格成立。
func TestExclusionListsScopedToOwningProductForRequests(t *testing.T) {
	s := epsScenario(t)
	for _, tc := range epsExpectations {
		r, err := s.Request(tc.reqID)
		if err != nil {
			t.Fatalf("request %s not saved: %v", tc.reqID, err)
		}
		if r.ID != tc.reqID || r.ProductID != tc.productID || r.FaultCode != tc.fault {
			t.Fatalf("saved request = %q/%q/%q, want %q/%q/%q",
				r.ID, r.ProductID, r.FaultCode, tc.reqID, tc.productID, tc.fault)
		}
	}
}

// TestExclusionListsScopedToOwningProductForEligibility 同一故障在两件产品上
// 的资格结果必须相反：命中本产品清单的请求不合格、除外标记为真、拒绝原因
// 只有 fault_code_excluded；仅出现在另一产品清单中的故障应合格且没有拒绝
// 原因。Evaluate 与 RequestView 两个口径一致，依据都来自请求关联的产品。
func TestExclusionListsScopedToOwningProductForEligibility(t *testing.T) {
	s := epsScenario(t)
	for _, tc := range epsExpectations {
		e, err := s.Evaluate(tc.reqID, nowOK)
		if err != nil {
			t.Fatalf("evaluate %s: %v", tc.reqID, err)
		}
		epsAssertEligibility(t, e, tc.reqID, tc.productID, tc.fault, tc.wantEligible)

		view, err := s.RequestView(tc.reqID, nowOK)
		if err != nil {
			t.Fatalf("request view %s: %v", tc.reqID, err)
		}
		epsAssertEligibility(t, view.Eligibility, tc.reqID, tc.productID, tc.fault, tc.wantEligible)
		// 尚未预留，任何请求都不应有关联承诺。
		if len(view.Commitments) != 0 {
			t.Fatalf("%s: gained commitments before any reserve: %+v", tc.reqID, view.Commitments)
		}
	}
}

// epsReservePlan 是四张请求各自的三件预留，按固定顺序提交，wantBasis 是该
// 次处理记录中“新增占用之前”的库存依据快照。
var epsReservePlan = []struct {
	commitID  string
	reqID     string
	productID string
	fault     string
	wantOK    bool
	wantBasis StockBasis
}{
	// 命中本产品清单被拒：不占用；库存依据仍是初始账目。
	{epsCommit1Seal, epsReq1Seal, epsP1, epsFaultSeal, false, StockBasis{10, 0, 10}},
	// 仅在另一产品清单中：成功占用三件；依据仍是新增占用之前的 10/0/10。
	{epsCommit1Water, epsReq1Water, epsP1, epsFaultWater, true, StockBasis{10, 0, 10}},
	// 同一故障 BROKEN_SEAL 在 p-eps-two 上合格：再占用三件，依据 10/3/7。
	{epsCommit2Seal, epsReq2Seal, epsP2, epsFaultSeal, true, StockBasis{10, 3, 7}},
	// 命中 p-eps-two 自己的 WATER_DAMAGE 清单被拒：不占用，依据 10/6/4。
	{epsCommit2Water, epsReq2Water, epsP2, epsFaultWater, false, StockBasis{10, 6, 4}},
}

// TestExclusionListsScopedToOwningProductForReserves 在实际提交预留时规则
// 同样成立：两张除外请求返回 ErrIneligible、查不到对应承诺、不占用数量；
// 另外两张各自成功预留三件，承诺归属各自的原请求。全部提交后实物仍为
// 十件、有效占用六件、可承诺四件，备件明细只有两笔成功承诺。
func TestExclusionListsScopedToOwningProductForReserves(t *testing.T) {
	s := epsScenario(t)

	for _, tc := range epsReservePlan {
		c, err := s.Reserve(tc.commitID, tc.reqID, epsPartID, 3, epsExpiry, nowOK)
		if tc.wantOK {
			if err != nil {
				t.Fatalf("reserve %s: %v", tc.commitID, err)
			}
			if c.ID != tc.commitID || c.RequestID != tc.reqID || c.PartID != epsPartID ||
				c.Quantity != 3 || c.Used != 0 || !c.Expiry.Equal(epsExpiry) ||
				c.Canceled || c.Expired {
				t.Fatalf("commitment %s altered: %+v", tc.commitID, c)
			}
			continue
		}
		if !errors.Is(err, ErrIneligible) {
			t.Fatalf("reserve %s: got %v, want ErrIneligible", tc.commitID, err)
		}
		// 失败不占用编号：查不到对应承诺。
		if _, err := s.Commitment(tc.commitID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rejected commitment %q exists: err=%v", tc.commitID, err)
		}
	}

	// 预留不扣实物：被拒的申请既不增加占用也不扣减实物。
	st, err := s.PartStatus(epsPartID, nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 6 || st.Committable != 4 {
		t.Fatalf("stock account = phys %d / occupied %d / committable %d, want 10/6/4",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	// 备件明细只能出现两笔成功承诺，按承诺编号排序。
	if len(st.Details) != 2 {
		t.Fatalf("part details = %+v, want exactly the two successful commitments", st.Details)
	}
	successful := map[string]string{
		epsCommit1Water: epsReq1Water,
		epsCommit2Seal:  epsReq2Seal,
	}
	for _, d := range st.Details {
		owner, ok := successful[d.CommitmentID]
		if !ok {
			t.Fatalf("unexpected or rejected commitment in details: %+v", d)
		}
		if d.RequestID != owner || d.PartID != epsPartID ||
			d.OriginalQuantity != 3 || d.UsedQuantity != 0 || d.RemainingQuantity != 3 ||
			d.Status != CommitmentActive || !d.Expiry.Equal(epsExpiry) {
			t.Fatalf("successful detail altered: %+v, want owner %s qty 3 active", d, owner)
		}
	}
	// 明细按承诺编号升序：c-eps-one-water 先于 c-eps-two-seal。
	if st.Details[0].CommitmentID != epsCommit1Water || st.Details[1].CommitmentID != epsCommit2Seal {
		t.Fatalf("details order = %s, %s, want ascending by commitment id",
			st.Details[0].CommitmentID, st.Details[1].CommitmentID)
	}

	// 按请求查看：成功请求各挂自己的承诺，被拒请求没有任何承诺。
	for _, tc := range epsReservePlan {
		view, err := s.RequestView(tc.reqID, nowOK)
		if err != nil {
			t.Fatalf("request view %s: %v", tc.reqID, err)
		}
		if tc.wantOK {
			if len(view.Commitments) != 1 {
				t.Fatalf("%s: commitments = %+v, want exactly %s", tc.reqID, view.Commitments, tc.commitID)
			}
			d := view.Commitments[0]
			if d.CommitmentID != tc.commitID || d.RequestID != tc.reqID || d.OriginalQuantity != 3 {
				t.Fatalf("%s: commitment belongs elsewhere: %+v", tc.reqID, d)
			}
		} else if len(view.Commitments) != 0 {
			t.Fatalf("%s: rejected request gained commitments: %+v", tc.reqID, view.Commitments)
		}
	}
}

// TestReserveHistorySnapshotsStayOnOwningRequest 每张请求的预留历史只保存
// 自己的处理结果：失败记录保留本请求的产品、故障与拒绝依据（原因只有
// fault_code_excluded），成功记录显示本产品允许该故障；谁都不能串用另一
// 产品的资格快照。库存依据按提交次序反映新增占用之前的账目。
func TestReserveHistorySnapshotsStayOnOwningRequest(t *testing.T) {
	s := epsScenario(t)
	for _, tc := range epsReservePlan {
		_, err := s.Reserve(tc.commitID, tc.reqID, epsPartID, 3, epsExpiry, nowOK)
		if tc.wantOK && err != nil {
			t.Fatalf("reserve %s: %v", tc.commitID, err)
		}
		if !tc.wantOK && !errors.Is(err, ErrIneligible) {
			t.Fatalf("reserve %s: got %v, want ErrIneligible", tc.commitID, err)
		}
	}

	for _, tc := range epsReservePlan {
		h, err := s.RequestHistory(tc.reqID)
		if err != nil {
			t.Fatalf("history %s: %v", tc.reqID, err)
		}
		if len(h) != 1 {
			t.Fatalf("%s: history length = %d, want 1 (records must not leak across requests)",
				tc.reqID, len(h))
		}
		rec := h[0]
		if rec.Seq != 1 || rec.CommitID != tc.commitID || rec.PartID != epsPartID ||
			rec.Quantity != 3 || !rec.Expiry.Equal(epsExpiry) || !rec.Now.Equal(nowOK) {
			t.Fatalf("%s: record submission content altered: %+v", tc.reqID, rec)
		}
		if rec.Success != tc.wantOK {
			t.Fatalf("%s: success = %v, want %v", tc.reqID, rec.Success, tc.wantOK)
		}

		// 资格快照必须来自本请求关联的产品，不能串用另一产品的结论。
		epsAssertEligibility(t, rec.Eligibility, tc.reqID, tc.productID, tc.fault, tc.wantOK)

		// 库存依据：处理前的实物剩余、有效占用与可承诺数量，按提交次序固定。
		b := rec.StockBasis
		if b == nil {
			t.Fatalf("%s: missing stock basis snapshot", tc.reqID)
		}
		if b.PhysicalRemaining != tc.wantBasis.PhysicalRemaining ||
			b.ActiveOccupied != tc.wantBasis.ActiveOccupied ||
			b.Committable != tc.wantBasis.Committable {
			t.Fatalf("%s: stock basis = %d/%d/%d, want %d/%d/%d",
				tc.reqID, b.PhysicalRemaining, b.ActiveOccupied, b.Committable,
				tc.wantBasis.PhysicalRemaining, tc.wantBasis.ActiveOccupied, tc.wantBasis.Committable)
		}

		if tc.wantOK {
			if rec.Error != "" {
				t.Fatalf("%s: success record carries error %q", tc.reqID, rec.Error)
			}
			continue
		}
		if rec.Error != HistoryErrorIneligible {
			t.Fatalf("%s: error category = %q, want %q", tc.reqID, rec.Error, HistoryErrorIneligible)
		}
	}

	// 交叉校验：BROKEN_SEAL 在 p-eps-one 下是失败记录、在 p-eps-two 下是
	// 成功记录，两份快照各归其主，结论相反但互不污染。
	seal1, _ := s.RequestHistory(epsReq1Seal)
	seal2, _ := s.RequestHistory(epsReq2Seal)
	if seal1[0].Eligibility.ProductID != epsP1 || seal1[0].Eligibility.Eligible ||
		!seal1[0].Eligibility.Excluded {
		t.Fatalf("p-eps-one BROKEN_SEAL snapshot leaked: %+v", seal1[0].Eligibility)
	}
	if seal2[0].Eligibility.ProductID != epsP2 || !seal2[0].Eligibility.Eligible ||
		seal2[0].Eligibility.Excluded {
		t.Fatalf("p-eps-two BROKEN_SEAL snapshot leaked: %+v", seal2[0].Eligibility)
	}
	// WATER_DAMAGE 方向同理。
	water1, _ := s.RequestHistory(epsReq1Water)
	water2, _ := s.RequestHistory(epsReq2Water)
	if water1[0].Eligibility.ProductID != epsP1 || !water1[0].Eligibility.Eligible {
		t.Fatalf("p-eps-one WATER_DAMAGE snapshot leaked: %+v", water1[0].Eligibility)
	}
	if water2[0].Eligibility.ProductID != epsP2 || water2[0].Eligibility.Eligible ||
		!water2[0].Eligibility.Excluded {
		t.Fatalf("p-eps-two WATER_DAMAGE snapshot leaked: %+v", water2[0].Eligibility)
	}
}

// epsDupStore 构造一个只有一件产品、库存十件备件的仓库，产品除外清单由
// 调用方给定（用于对比“重复列出”和“只列一次”）；另提交两张故障请求。
func epsDupStore(t *testing.T, codes []string) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p-eps-dup", t0, 30, codes); err != nil {
		t.Fatalf("register product with codes %v: %v", codes, err)
	}
	if err := s.RegisterPart("part-eps-dup", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r-eps-dup-seal", "p-eps-dup", epsFaultSeal); err != nil {
		t.Fatalf("submit seal request: %v", err)
	}
	if err := s.SubmitRequest("r-eps-dup-water", "p-eps-dup", epsFaultWater); err != nil {
		t.Fatalf("submit water request: %v", err)
	}
	return s
}

// epsAssertDupOutcome 在一个“清单只含 BROKEN_SEAL”的仓库上跑同一组资格、
// 预留与库存操作，返回最终备件账目供两个仓库逐字对比。
func epsAssertDupOutcome(t *testing.T, s *Store) PartStatus {
	t.Helper()
	// 命中代码：不合格，拒绝原因只出现一次。
	e, err := s.Evaluate("r-eps-dup-seal", nowOK)
	if err != nil {
		t.Fatalf("evaluate seal: %v", err)
	}
	if e.Eligible || !e.Excluded || len(e.Reasons) != 1 || e.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("duplicated listing eligibility = %+v, want one fault_code_excluded reason", e)
	}

	// 命中代码的三件预留被拒，不产生承诺、不占用数量。
	if _, err := s.Reserve("c-eps-dup-seal", "r-eps-dup-seal", "part-eps-dup", 3, epsExpiry, nowOK); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve seal: got %v, want ErrIneligible", err)
	}
	if _, err := s.Commitment("c-eps-dup-seal"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected seal commitment exists: %v", err)
	}

	// 清单外的 WATER_DAMAGE 合格，三件预留成功。
	c, err := s.Reserve("c-eps-dup-water", "r-eps-dup-water", "part-eps-dup", 3, epsExpiry, nowOK)
	if err != nil {
		t.Fatalf("reserve water: %v", err)
	}
	if c.RequestID != "r-eps-dup-water" || c.Quantity != 3 {
		t.Fatalf("water commitment altered: %+v", c)
	}

	st, err := s.PartStatus("part-eps-dup", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 3 || st.Committable != 7 {
		t.Fatalf("stock = %d/%d/%d, want 10/3/7",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if len(st.Details) != 1 {
		t.Fatalf("details = %+v, want only the successful water commitment", st.Details)
	}
	d := st.Details[0]
	if d.CommitmentID != "c-eps-dup-water" || d.RequestID != "r-eps-dup-water" ||
		d.OriginalQuantity != 3 || d.Status != CommitmentActive {
		t.Fatalf("single detail altered: %+v", d)
	}

	// 命中请求的失败历史同样只保留一次拒绝原因。
	h, err := s.RequestHistory("r-eps-dup-seal")
	if err != nil {
		t.Fatalf("seal history: %v", err)
	}
	if len(h) != 1 || h[0].Success || h[0].Error != HistoryErrorIneligible {
		t.Fatalf("seal history = %+v, want one ineligible record", h)
	}
	if h[0].Eligibility == nil || len(h[0].Eligibility.Reasons) != 1 ||
		h[0].Eligibility.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("seal history reasons = %+v, want one fault_code_excluded", h[0].Eligibility)
	}
	return *st
}

// TestDuplicatedExcludedCodeOnFirstRegistration 首次登记产品时除外清单重复
// 列出同一代码：登记仍成功，集合中只保留一个条目，命中后的拒绝原因只出现
// 一次，预留结果与库存数量与只列一次完全相同。
func TestDuplicatedExcludedCodeOnFirstRegistration(t *testing.T) {
	dup := epsDupStore(t, []string{epsFaultSeal, epsFaultSeal})

	p, err := dup.Product("p-eps-dup")
	if err != nil {
		t.Fatalf("product: %v", err)
	}
	if len(p.ExcludedCodes) != 1 {
		t.Fatalf("excluded code set = %+v, want exactly one entry", p.ExcludedCodes)
	}
	if _, ok := p.ExcludedCodes[epsFaultSeal]; !ok {
		t.Fatalf("excluded code set = %+v, want to contain %q", p.ExcludedCodes, epsFaultSeal)
	}

	single := epsDupStore(t, []string{epsFaultSeal})

	dupStatus := epsAssertDupOutcome(t, dup)
	singleStatus := epsAssertDupOutcome(t, single)
	if dupStatus.PhysicalRemaining != singleStatus.PhysicalRemaining ||
		dupStatus.ActiveOccupied != singleStatus.ActiveOccupied ||
		dupStatus.Committable != singleStatus.Committable ||
		len(dupStatus.Details) != len(singleStatus.Details) {
		t.Fatalf("duplicated listing outcome %+v differs from single listing %+v",
			dupStatus, singleStatus)
	}
}
