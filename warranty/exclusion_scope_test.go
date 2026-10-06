package warranty

import (
	"errors"
	"testing"
)

// 本文件回归保障除外故障代码清单只作用于所属产品：多个产品可以共用同一种
// 备件，但某个产品把一个故障代码列为除外，不能让另一产品的同代码请求也被
// 拒绝。测试只沿用现有的登记、资格查询、预留与历史查询等公开行为，不改变
// 除外匹配方式与错误语义。

// exclusionScopeStore 构造两件共用备件的产品：购买时刻相同、保修期均为三十天，
// pA 只除外 BROKEN_SEAL，pB 只除外 WATER_DAMAGE；备件 part1 初始库存十件。
func exclusionScopeStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("pA", t0, 30, []string{"BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product pA: %v", err)
	}
	if err := s.RegisterProduct("pB", t0, 30, []string{"WATER_DAMAGE"}); err != nil {
		t.Fatalf("register product pB: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	return s
}

// mustSubmitScope 提交一张保修请求。
func mustSubmitScope(t *testing.T, s *Store, id, productID, faultCode string) {
	t.Helper()
	if err := s.SubmitRequest(id, productID, faultCode); err != nil {
		t.Fatalf("submit request %s: %v", id, err)
	}
}

// scopeRequests 是四张请求：每件产品各提交两种故障代码。
var scopeRequests = []struct {
	id        string
	productID string
	faultCode string
	excluded  bool // 是否命中本产品的除外清单
}{
	{"rA-seal", "pA", "BROKEN_SEAL", true},
	{"rA-water", "pA", "WATER_DAMAGE", false},
	{"rB-seal", "pB", "BROKEN_SEAL", false},
	{"rB-water", "pB", "WATER_DAMAGE", true},
}

// submitScopeRequests 提交全部四张请求。
func submitScopeRequests(t *testing.T, s *Store) {
	t.Helper()
	for _, r := range scopeRequests {
		mustSubmitScope(t, s, r.id, r.productID, r.faultCode)
	}
}

// TestExclusionScopeRequestsSavedAsSubmitted 四张请求都应保存成功，取回时
// 产品编号和故障代码保持原值。
func TestExclusionScopeRequestsSavedAsSubmitted(t *testing.T) {
	s := exclusionScopeStore(t)
	submitScopeRequests(t, s)

	for _, r := range scopeRequests {
		got, err := s.Request(r.id)
		if err != nil {
			t.Fatalf("request %s: %v", r.id, err)
		}
		if got.ProductID != r.productID || got.FaultCode != r.faultCode {
			t.Fatalf("request %s = (%q, %q), want (%q, %q)",
				r.id, got.ProductID, got.FaultCode, r.productID, r.faultCode)
		}
	}
}

// TestExclusionScopeEvaluatePerProduct 同一故障代码在两件产品上的资格结果
// 应相反：命中本产品清单的请求不合格、除外标记为真、拒绝原因只有
// fault_code_excluded；仅在另一产品清单中出现的故障应合格、没有拒绝原因。
// 资格依据中的购买时刻、保修天数与截止时刻来自请求关联的产品。
func TestExclusionScopeEvaluatePerProduct(t *testing.T) {
	s := exclusionScopeStore(t)
	submitScopeRequests(t, s)

	wantExpiry := t0.Add(30 * day)
	for _, r := range scopeRequests {
		e, err := s.Evaluate(r.id, nowOK)
		if err != nil {
			t.Fatalf("evaluate %s: %v", r.id, err)
		}
		// 资格依据来自请求关联的产品。
		if e.ProductID != r.productID || e.FaultCode != r.faultCode {
			t.Fatalf("%s: identity = (%q, %q), want (%q, %q)",
				r.id, e.ProductID, e.FaultCode, r.productID, r.faultCode)
		}
		if !e.PurchaseTime.Equal(t0) || e.WarrantyDays != 30 || !e.WarrantyExpiry.Equal(wantExpiry) {
			t.Fatalf("%s: basis = (%v, %d, %v), want (%v, 30, %v)",
				r.id, e.PurchaseTime, e.WarrantyDays, e.WarrantyExpiry, t0, wantExpiry)
		}
		if r.excluded {
			if e.Eligible || !e.Excluded {
				t.Fatalf("%s: want ineligible and excluded, got %+v", r.id, e)
			}
			if len(e.Reasons) != 1 || e.Reasons[0] != ReasonFaultExcluded {
				t.Fatalf("%s: reasons = %v, want only [fault_code_excluded]", r.id, e.Reasons)
			}
		} else {
			if !e.Eligible || e.Excluded || len(e.Reasons) != 0 {
				t.Fatalf("%s: want eligible with no reasons, got %+v", r.id, e)
			}
		}

		// 按请求查看的资格与直接查询一致。
		view, err := s.RequestView(r.id, nowOK)
		if err != nil {
			t.Fatalf("request view %s: %v", r.id, err)
		}
		ve := view.Eligibility
		if ve.Eligible != e.Eligible || ve.Excluded != e.Excluded || len(ve.Reasons) != len(e.Reasons) {
			t.Fatalf("%s: view %+v disagrees with evaluate %+v", r.id, ve, e)
		}
	}
}

// TestExclusionScopeReserveAndStock 四张请求分别用不同承诺编号申请三件共用
// 备件：两张除外请求返回 ErrIneligible，查不到对应承诺，也不占用数量；另外
// 两张各自成功预留三件，承诺归属各自的原请求。全部提交后实物仍为十件、
// 有效占用六件、可承诺四件，备件明细只出现两笔成功承诺。
func TestExclusionScopeReserveAndStock(t *testing.T) {
	s := exclusionScopeStore(t)
	submitScopeRequests(t, s)

	commitOf := map[string]string{
		"rA-seal":  "cA-seal",
		"rA-water": "cA-water",
		"rB-seal":  "cB-seal",
		"rB-water": "cB-water",
	}
	for _, r := range scopeRequests {
		commitID := commitOf[r.id]
		c, err := s.Reserve(commitID, r.id, "part1", 3, expiryOK, nowOK)
		if r.excluded {
			if !errors.Is(err, ErrIneligible) {
				t.Fatalf("reserve %s: got %v, want ErrIneligible", r.id, err)
			}
			// 查不到对应承诺。
			if _, cerr := s.Commitment(commitID); !errors.Is(cerr, ErrNotFound) {
				t.Fatalf("rejected reserve %s created commitment: %v", commitID, cerr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("reserve %s: %v", r.id, err)
		}
		// 承诺归属各自的原请求。
		if c.ID != commitID || c.RequestID != r.id || c.PartID != "part1" ||
			c.Quantity != 3 || c.Used != 0 {
			t.Fatalf("commitment for %s: %+v", r.id, c)
		}
	}

	// 被拒的申请不增加占用、不扣减实物。
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 6 || st.Committable != 4 {
		t.Fatalf("stock math = (%d, %d, %d), want (10, 6, 4)",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	// 备件明细只能出现两笔成功承诺。
	if len(st.Details) != 2 {
		t.Fatalf("details = %d entries, want 2: %+v", len(st.Details), st.Details)
	}
	seen := map[string]string{}
	for _, d := range st.Details {
		seen[d.CommitmentID] = d.RequestID
		if d.OriginalQuantity != 3 || d.RemainingQuantity != 3 || d.Status != CommitmentActive {
			t.Fatalf("detail %+v: want active commitment of 3 unused", d)
		}
	}
	if seen["cA-water"] != "rA-water" || seen["cB-seal"] != "rB-seal" {
		t.Fatalf("details ownership = %v, want cA-water->rA-water and cB-seal->rB-seal", seen)
	}
}

// TestExclusionScopeHistoryIsolation 每张请求的预留历史只保存自己的处理
// 结果：除外失败记录保留本请求的产品、故障及拒绝依据，成功记录显示本产品
// 允许该故障，不能串用另一产品的资格快照。
func TestExclusionScopeHistoryIsolation(t *testing.T) {
	s := exclusionScopeStore(t)
	submitScopeRequests(t, s)

	commitOf := map[string]string{
		"rA-seal":  "cA-seal",
		"rA-water": "cA-water",
		"rB-seal":  "cB-seal",
		"rB-water": "cB-water",
	}
	for _, r := range scopeRequests {
		_, _ = s.Reserve(commitOf[r.id], r.id, "part1", 3, expiryOK, nowOK)
	}

	for _, r := range scopeRequests {
		h, err := s.RequestHistory(r.id)
		if err != nil {
			t.Fatalf("history %s: %v", r.id, err)
		}
		if len(h) != 1 {
			t.Fatalf("%s: history = %d records, want 1: %+v", r.id, len(h), h)
		}
		rec := h[0]
		if rec.Seq != 1 || rec.CommitID != commitOf[r.id] || rec.PartID != "part1" ||
			rec.Quantity != 3 || !rec.Expiry.Equal(expiryOK) || !rec.Now.Equal(nowOK) {
			t.Fatalf("%s: record skeleton = %+v", r.id, rec)
		}
		// 资格快照必须是本请求自己的，不能串用另一产品的结果。
		if rec.Eligibility == nil {
			t.Fatalf("%s: missing eligibility snapshot", r.id)
		}
		es := rec.Eligibility
		if es.RequestID != r.id || es.ProductID != r.productID || es.FaultCode != r.faultCode {
			t.Fatalf("%s: snapshot identity = (%q, %q, %q), want own (%q, %q, %q)",
				r.id, es.RequestID, es.ProductID, es.FaultCode, r.id, r.productID, r.faultCode)
		}
		if r.excluded {
			if rec.Success || rec.Error != HistoryErrorIneligible {
				t.Fatalf("%s: record = (success=%v, error=%q), want ineligible failure",
					r.id, rec.Success, rec.Error)
			}
			if es.Eligible || !es.Excluded ||
				len(es.Reasons) != 1 || es.Reasons[0] != ReasonFaultExcluded {
				t.Fatalf("%s: failure snapshot = %+v, want excluded with only fault_code_excluded",
					r.id, es)
			}
		} else {
			if !rec.Success || rec.Error != "" {
				t.Fatalf("%s: record = (success=%v, error=%q), want success",
					r.id, rec.Success, rec.Error)
			}
			if !es.Eligible || es.Excluded || len(es.Reasons) != 0 {
				t.Fatalf("%s: success snapshot = %+v, want eligible with no reasons", r.id, es)
			}
		}
	}
}

// TestDuplicateExcludedCodeRegisteredOnce 首次登记产品时除外清单重复列出同一
// 代码：登记仍成功，命中后的拒绝原因只出现一次，预留结果和库存数量与只列
// 一次相同。
func TestDuplicateExcludedCodeRegisteredOnce(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("pDup", t0, 30, []string{"BROKEN_SEAL", "BROKEN_SEAL"}); err != nil {
		t.Fatalf("register product with duplicated code: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	mustSubmitScope(t, s, "rDup", "pDup", "BROKEN_SEAL")

	// 取回的产品资料中该代码只算一个。
	p, err := s.Product("pDup")
	if err != nil {
		t.Fatalf("product: %v", err)
	}
	if len(p.ExcludedCodes) != 1 {
		t.Fatalf("excluded codes = %v, want exactly one entry", p.ExcludedCodes)
	}
	if _, ok := p.ExcludedCodes["BROKEN_SEAL"]; !ok {
		t.Fatalf("excluded codes = %v, want BROKEN_SEAL", p.ExcludedCodes)
	}

	// 命中后的拒绝原因只出现一次。
	e, err := s.Evaluate("rDup", nowOK)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if e.Eligible || !e.Excluded {
		t.Fatalf("want ineligible and excluded, got %+v", e)
	}
	if len(e.Reasons) != 1 || e.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("reasons = %v, want fault_code_excluded exactly once", e.Reasons)
	}

	// 预留结果与只列一次相同：ErrIneligible，不产生承诺、不占用库存。
	_, err = s.Reserve("cDup", "rDup", "part1", 3, expiryOK, nowOK)
	if !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve: got %v, want ErrIneligible", err)
	}
	if _, cerr := s.Commitment("cDup"); !errors.Is(cerr, ErrNotFound) {
		t.Fatalf("rejected reserve created commitment: %v", cerr)
	}
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 10 || st.ActiveOccupied != 0 || st.Committable != 10 {
		t.Fatalf("stock math = (%d, %d, %d), want (10, 0, 10)",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	if len(st.Details) != 0 {
		t.Fatalf("details = %+v, want none", st.Details)
	}

	// 失败记录中拒绝原因同样只出现一次。
	h, err := s.RequestHistory("rDup")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 || h[0].Success || h[0].Error != HistoryErrorIneligible {
		t.Fatalf("history = %+v, want one ineligible failure", h)
	}
	es := h[0].Eligibility
	if es == nil || len(es.Reasons) != 1 || es.Reasons[0] != ReasonFaultExcluded {
		t.Fatalf("history snapshot reasons = %+v, want fault_code_excluded exactly once", es)
	}
}
