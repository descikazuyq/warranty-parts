package warranty

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// historyStore 构造一个带产品/备件/请求的仓库。
func historyStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit request: %v", err)
	}
	return s
}

func TestHistoryEmptyAndValidation(t *testing.T) {
	s := historyStore(t)

	// 已知请求没有记录时返回空列表（非 nil）。
	h, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if h == nil || len(h) != 0 {
		t.Fatalf("expected empty non-nil history, got %v", h)
	}

	// 空请求编号 → ErrInvalidParam。
	if _, err := s.RequestHistory(""); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty request id: got %v", err)
	}
	// 未知请求 → ErrNotFound。
	if _, err := s.RequestHistory("rMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v", err)
	}
}

func TestHistorySuccessRecord(t *testing.T) {
	s := historyStore(t)
	c, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	h, err := s.RequestHistory("r1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1", len(h))
	}
	rec := h[0]
	if rec.Seq != 1 || !rec.Success || rec.Error != "" {
		t.Fatalf("bad success marker: seq=%d success=%v error=%q", rec.Seq, rec.Success, rec.Error)
	}
	if rec.CommitID != c.ID || rec.PartID != "part1" || rec.Quantity != 4 {
		t.Fatalf("bad submission fields: %+v", rec)
	}
	if !rec.Expiry.Equal(expiryOK) || !rec.Now.Equal(nowOK) {
		t.Fatalf("bad times: expiry=%v now=%v", rec.Expiry, rec.Now)
	}
	// 资格依据快照。
	if rec.Eligibility == nil {
		t.Fatal("expected eligibility snapshot")
	}
	if !rec.Eligibility.Eligible || rec.Eligibility.PurchaseTime != t0 ||
		rec.Eligibility.WarrantyDays != 30 || rec.Eligibility.WarrantyExpiry != t0.Add(30*day) ||
		rec.Eligibility.Excluded || len(rec.Eligibility.Reasons) != 0 {
		t.Fatalf("bad eligibility snapshot: %+v", rec.Eligibility)
	}
	// 库存依据：处理前实物 10、占用 0、可承诺 10。
	if rec.StockBasis == nil {
		t.Fatal("expected stock basis snapshot")
	}
	if rec.StockBasis.PhysicalRemaining != 10 || rec.StockBasis.ActiveOccupied != 0 ||
		rec.StockBasis.Committable != 10 {
		t.Fatalf("bad stock basis: %+v", rec.StockBasis)
	}
}

func TestHistoryIneligibleRecord(t *testing.T) {
	s := historyStore(t)
	if err := s.SubmitRequest("rBad", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// 第 31 天：过保且除外，两项原因都保留。
	if _, err := s.Reserve("c1", "rBad", "part1", 1, expiryOK, t0.Add(31*day)); !errors.Is(err, ErrIneligible) {
		t.Fatalf("reserve: got %v", err)
	}
	h, _ := s.RequestHistory("rBad")
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1", len(h))
	}
	rec := h[0]
	if rec.Success || rec.Error != HistoryErrorIneligible {
		t.Fatalf("bad error category: success=%v error=%q", rec.Success, rec.Error)
	}
	if rec.Eligibility == nil {
		t.Fatal("expected eligibility snapshot for ineligible record")
	}
	if rec.Eligibility.Eligible || !rec.Eligibility.Excluded {
		t.Fatalf("expected ineligible+excluded: %+v", rec.Eligibility)
	}
	got := map[RejectionReason]bool{}
	for _, r := range rec.Eligibility.Reasons {
		got[r] = true
	}
	if !got[ReasonWarrantyExpired] || !got[ReasonFaultExcluded] {
		t.Fatalf("reasons = %v, want both warranty_expired and fault_code_excluded", rec.Eligibility.Reasons)
	}
	// 库存依据仍保留（备件存在）。
	if rec.StockBasis == nil || rec.StockBasis.Committable != 10 {
		t.Fatalf("stock basis: %+v", rec.StockBasis)
	}
}

func TestHistoryInsufficientStockRecord(t *testing.T) {
	s := historyStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 11, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve: got %v", err)
	}
	h, _ := s.RequestHistory("r1")
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1", len(h))
	}
	rec := h[0]
	if rec.Success || rec.Error != HistoryErrorInsufficientStock {
		t.Fatalf("bad error category: %q", rec.Error)
	}
	if rec.Eligibility == nil || !rec.Eligibility.Eligible {
		t.Fatalf("expected eligible snapshot: %+v", rec.Eligibility)
	}
	if rec.StockBasis == nil {
		t.Fatal("expected stock basis")
	}
	if rec.StockBasis.PhysicalRemaining != 10 || rec.StockBasis.ActiveOccupied != 0 ||
		rec.StockBasis.Committable != 10 {
		t.Fatalf("bad stock basis: %+v", rec.StockBasis)
	}
}

func TestHistoryMissingProductAndPart(t *testing.T) {
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	// rNoProduct 关联尚未登记的产品；rNoPart 关联已登记产品但备件缺失。
	if err := s.SubmitRequest("rNoProduct", "pMissing", "F"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := s.SubmitRequest("rNoPart", "p1", "F"); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 产品缺失：已知请求的失败历史可查，资格与库存依据明确为空。
	if _, err := s.Reserve("c1", "rNoProduct", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve missing product: got %v", err)
	}
	h, err := s.RequestHistory("rNoProduct")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 1 || h[0].Error != HistoryErrorProductNotFound {
		t.Fatalf("missing product history: %+v", h)
	}
	if h[0].Eligibility != nil || h[0].StockBasis != nil {
		t.Fatalf("expected nil eligibility & stock basis, got %+v", h[0])
	}

	// 备件缺失：库存依据明确为空。
	if _, err := s.Reserve("c2", "rNoPart", "partMissing", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve missing part: got %v", err)
	}
	h2, err := s.RequestHistory("rNoPart")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h2) != 1 || h2[0].Error != HistoryErrorPartNotFound {
		t.Fatalf("missing part history: %+v", h2)
	}
	if h2[0].Eligibility != nil || h2[0].StockBasis != nil {
		t.Fatalf("expected nil eligibility & stock basis, got %+v", h2[0])
	}
}

func TestHistoryInvalidParamRecord(t *testing.T) {
	s := historyStore(t)
	// 数量非正：请求已知，记录参数无效。
	if _, err := s.Reserve("c1", "r1", "part1", 0, expiryOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero quantity: got %v", err)
	}
	h, _ := s.RequestHistory("r1")
	if len(h) != 1 || h[0].Error != HistoryErrorInvalidParam {
		t.Fatalf("invalid param history: %+v", h)
	}
	if h[0].Eligibility != nil || h[0].StockBasis != nil {
		t.Fatalf("expected nil basis for invalid param, got %+v", h[0])
	}

	// 到期时刻未晚于当前时刻：同样记录。
	if _, err := s.Reserve("c2", "r1", "part1", 1, nowOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("expiry equal now: got %v", err)
	}
	h, _ = s.RequestHistory("r1")
	if len(h) != 2 || h[1].Error != HistoryErrorInvalidParam {
		t.Fatalf("second invalid param history: %+v", h)
	}
}

func TestHistoryConflictRoutesToSubmissionRequest(t *testing.T) {
	s := historyStore(t)
	if err := s.SubmitRequest("r2", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}
	// c1 挂在 r1 下。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 同编号改内容（换请求）→ 冲突，失败记录挂到本次提交指定的 r2，不挂 r1。
	if _, err := s.Reserve("c1", "r2", "part1", 5, expiryOK, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: got %v", err)
	}
	h1, _ := s.RequestHistory("r1")
	if len(h1) != 1 || !h1[0].Success {
		t.Fatalf("r1 history should only have the success, got %+v", h1)
	}
	h2, _ := s.RequestHistory("r2")
	if len(h2) != 1 || h2[0].Success || h2[0].Error != HistoryErrorConflict {
		t.Fatalf("r2 history should have the conflict failure, got %+v", h2)
	}
	if h2[0].Eligibility != nil || h2[0].StockBasis != nil {
		t.Fatalf("conflict record should have nil basis, got %+v", h2[0])
	}
}

func TestHistoryIdempotentRetryAppendsNothing(t *testing.T) {
	s := historyStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 同编号同内容重试：不追加历史。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("retry: %v", err)
	}
	h, _ := s.RequestHistory("r1")
	if len(h) != 1 {
		t.Fatalf("idempotent retry appended history: len=%d", len(h))
	}
}

func TestHistoryFailedSubmissionReusableAndNotOverwritten(t *testing.T) {
	s := historyStore(t)
	// 库存不足失败：不占用承诺编号。
	if _, err := s.Reserve("c1", "r1", "part1", 11, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("over-stock: got %v", err)
	}
	// 条件满足后用同编号重新提交成功。
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve after restock: %v", err)
	}
	h, _ := s.RequestHistory("r1")
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2 (failure then success)", len(h))
	}
	if h[0].Success || h[0].Error != HistoryErrorInsufficientStock {
		t.Fatalf("first record should be failure: %+v", h[0])
	}
	if !h[1].Success || h[1].Error != "" {
		t.Fatalf("second record should be success: %+v", h[1])
	}
	// 承诺编号只被占用一次。
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 4 {
		t.Fatalf("active occupied = %d, want 4", st.ActiveOccupied)
	}
}

func TestHistoryOrderingAndSeq(t *testing.T) {
	s := historyStore(t)
	if err := s.SubmitRequest("r2", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}
	// 用前后颠倒的时刻提交，次序仍按处理次序排列。
	times := []time.Time{nowOK, nowOK.Add(-time.Hour), nowOK.Add(time.Hour), nowOK}
	for i, tm := range times {
		if _, err := s.Reserve(fmt.Sprintf("c%02d", i), "r1", "part1", 1, expiryOK, tm); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	// r2 也提交一笔，验证序号按请求独立递增。
	if _, err := s.Reserve("c10", "r2", "part1", 1, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve r2: %v", err)
	}
	h1, _ := s.RequestHistory("r1")
	if len(h1) != 4 {
		t.Fatalf("r1 history len = %d, want 4", len(h1))
	}
	for i, rec := range h1 {
		if rec.Seq != i+1 {
			t.Fatalf("record %d seq = %d, want %d", i, rec.Seq, i+1)
		}
	}
	h2, _ := s.RequestHistory("r2")
	if len(h2) != 1 || h2[0].Seq != 1 {
		t.Fatalf("r2 history: %+v", h2)
	}
}

func TestHistoryDefensiveCopy(t *testing.T) {
	s := historyStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	h, _ := s.RequestHistory("r1")
	// 修改返回的列表与拒绝原因，不影响已保存历史。
	h[0].Success = false
	h[0].Error = HistoryErrorConflict
	h[0].Eligibility.Reasons = append(h[0].Eligibility.Reasons, ReasonWarrantyExpired)
	h[0].StockBasis.Committable = 999
	h2, _ := s.RequestHistory("r1")
	if !h2[0].Success || h2[0].Error != "" {
		t.Fatalf("stored history mutated: %+v", h2[0])
	}
	if len(h2[0].Eligibility.Reasons) != 0 || h2[0].StockBasis.Committable != 10 {
		t.Fatalf("stored snapshot mutated: %+v %+v", h2[0].Eligibility, h2[0].StockBasis)
	}
}

func TestHistorySnapshotUnchangedAfterUseCancelExpire(t *testing.T) {
	s := historyStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	h, _ := s.RequestHistory("r1")
	before := h[0]
	// 使用、取消、到期都不改变历史中的依据与数量。
	if _, err := s.Use("u1", "c1", 3, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	h2, _ := s.RequestHistory("r1")
	after := h2[0]
	if after.Quantity != before.Quantity || after.StockBasis.PhysicalRemaining != before.StockBasis.PhysicalRemaining ||
		after.StockBasis.ActiveOccupied != before.StockBasis.ActiveOccupied ||
		after.StockBasis.Committable != before.StockBasis.Committable {
		t.Fatalf("history changed after use/cancel: before=%+v after=%+v", before, after)
	}
	if !after.Success || after.Eligibility == nil {
		t.Fatalf("success marker changed: %+v", after)
	}
}

func TestHistoryConcurrentReservesMatchResults(t *testing.T) {
	s := historyStore(t)
	const n = 30
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, failed := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Reserve(fmt.Sprintf("c%02d", i), "r1", "part1", 1, expiryOK, nowOK)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if errors.Is(err, ErrInsufficientStock) {
				failed++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if success != 10 || failed != 20 {
		t.Fatalf("success=%d failed=%d, want 10/20", success, failed)
	}
	h, _ := s.RequestHistory("r1")
	if len(h) != n {
		t.Fatalf("history len = %d, want %d (every submission recorded)", len(h), n)
	}
	succ, fail := 0, 0
	for _, rec := range h {
		if rec.Success {
			succ++
			// 成功记录的库存依据必须反映处理前可承诺量 >= 1。
			if rec.StockBasis == nil || rec.StockBasis.Committable < 1 {
				t.Fatalf("success record with bad stock basis: %+v", rec)
			}
		} else {
			fail++
			if rec.Error != HistoryErrorInsufficientStock {
				t.Fatalf("unexpected failure category: %q", rec.Error)
			}
			// 失败记录的库存依据必须反映处理前可承诺量 < 1。
			if rec.StockBasis == nil || rec.StockBasis.Committable >= 1 {
				t.Fatalf("failure record with bad stock basis: %+v", rec)
			}
		}
	}
	if succ != 10 || fail != 20 {
		t.Fatalf("history success/fail = %d/%d, want 10/20", succ, fail)
	}
	// 序号严格递增。
	for i, rec := range h {
		if rec.Seq != i+1 {
			t.Fatalf("record %d seq = %d", i, rec.Seq)
		}
	}
}

func TestHistoryUnknownRequestSubmissionNoRecord(t *testing.T) {
	s := historyStore(t)
	// 未知请求的提交按原约定报错，不创建请求或历史。
	if _, err := s.Reserve("c1", "rMissing", "part1", 1, expiryOK, nowOK); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: got %v", err)
	}
	if _, err := s.RequestHistory("rMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request history: got %v, want ErrNotFound", err)
	}
}
