package warranty

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestReserveRetryAfterExpiryReturnsFirstResult 验证到期时刻（或更晚）原样重试
// 不再报参数错误，而是取回首次成功时的承诺。
func TestReserveRetryAfterExpiryReturnsFirstResult(t *testing.T) {
	s := newStore(t)
	first, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 当前时刻恰好等于到期时刻：首次提交的参数规则不再适用。
	atExpiry, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, expiryOK)
	if err != nil {
		t.Fatalf("retry at expiry: %v", err)
	}
	if atExpiry != first || atExpiry.Used != 0 || atExpiry.Canceled {
		t.Fatalf("retry at expiry = %+v, want first result %+v", atExpiry, first)
	}
	// 晚于到期时刻重试同样取回首次结果。
	after := expiryOK.Add(10 * day)
	later, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, after)
	if err != nil {
		t.Fatalf("retry after expiry: %v", err)
	}
	if later != first {
		t.Fatalf("retry after expiry = %+v, want %+v", later, first)
	}
}

// TestReserveRetryAcrossTimeZones 验证到期时刻按实际时刻比较，换时区表示仍算一致。
func TestReserveRetryAcrossTimeZones(t *testing.T) {
	s := newStore(t)
	first, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	loc := time.FixedZone("UTC+8", 8*60*60)
	expiryTZ := expiryOK.In(loc)
	if expiryTZ.Equal(expiryOK) && expiryTZ.String() == expiryOK.String() {
		t.Fatalf("test setup: timezone representation did not differ")
	}
	got, err := s.Reserve("c1", "r1", "part1", 5, expiryTZ, nowOK.Add(5*day))
	if err != nil {
		t.Fatalf("retry with same instant in another zone: %v", err)
	}
	if got != first {
		t.Fatalf("timezone retry = %+v, want %+v", got, first)
	}
}

// TestReserveRetryAfterUseAndCancelReturnsFirstResult 对应用户给出的典型例子：
// 库存十件，预留五件，使用两件后取消，原样重试仍返回首次的五件承诺；查询继续
// 显示已用两件、已取消、实物剩余八件、有效占用为零。
func TestReserveRetryAfterUseAndCancelReturnsFirstResult(t *testing.T) {
	s := newStore(t)
	first, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "c1", 2, nowOK); err != nil {
		t.Fatalf("use: %v", err)
	}
	if _, err := s.Cancel("c1", nowOK); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	retry, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK.Add(day))
	if err != nil {
		t.Fatalf("retry after use+cancel: %v", err)
	}
	// 取回首次成功时的完整承诺：已用数量仍为零、取消标记仍为否。
	if retry != first || retry.Used != 0 || retry.Canceled || retry.Quantity != 5 {
		t.Fatalf("retry = %+v, want first %+v", retry, first)
	}

	// 查询继续反映承诺的当前状态。
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 8 {
		t.Fatalf("physical remaining = %d, want 8", st.PhysicalRemaining)
	}
	if st.ActiveOccupied != 0 || st.Committable != 8 {
		t.Fatalf("occupancy not released: occupied=%d committable=%d", st.ActiveOccupied, st.Committable)
	}
	if len(st.Details) != 1 {
		t.Fatalf("details = %d, want 1", len(st.Details))
	}
	d := st.Details[0]
	if d.Status != CommitmentCanceled || d.UsedQuantity != 2 || d.RemainingQuantity != 3 {
		t.Fatalf("current detail = %+v, want canceled/used=2/remaining=3", d)
	}
	// 重试没有恢复占用或补回实物。
	if st.PhysicalRemaining != 8 || st.ActiveOccupied != 0 {
		t.Fatalf("retry restored released state: %+v", st)
	}
}

// TestReserveRetryAfterFullyUsedReturnsFirstResult 验证全部使用后重试仍返回首次承诺。
func TestReserveRetryAfterFullyUsedReturnsFirstResult(t *testing.T) {
	s := newStore(t)
	first, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	for i, qty := range []int{2, 3} {
		if _, err := s.Use(fmt.Sprintf("u%d", i), "c1", qty, nowOK); err != nil {
			t.Fatalf("use %d: %v", qty, err)
		}
	}
	retry, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("retry after full use: %v", err)
	}
	if retry != first || retry.Used != 0 {
		t.Fatalf("retry after full use = %+v, want first %+v", retry, first)
	}
}

// TestReserveRetryIgnoresLaterEligibilityAndStock 验证请求过保、库存不足后，
// 原样重试仍只取回旧结果，首次成功的资格与库存依据不被重试时刻覆盖。
func TestReserveRetryIgnoresLaterEligibilityAndStock(t *testing.T) {
	s := newStore(t)
	first, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 另一笔承诺占满剩余可承诺量。
	if _, err := s.Reserve("c2", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve c2: %v", err)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.Committable != 0 {
		t.Fatalf("setup: committable = %d, want 0", st.Committable)
	}
	// 请求此时已过保（保修 30 天，now = 第 31 天），且无库存可承诺。
	retry, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, t0.Add(31*day))
	if err != nil {
		t.Fatalf("retry while expired & out of stock: %v", err)
	}
	if retry != first {
		t.Fatalf("retry = %+v, want first %+v", retry, first)
	}
	// 重试不追加历史，首次成功依据保持不变。
	h, _ := s.RequestHistory("r1")
	if len(h) != 2 || !h[0].Success {
		t.Fatalf("history after retry = %+v", h)
	}
	if h[0].Eligibility == nil || !h[0].Eligibility.Eligible ||
		h[0].StockBasis == nil || h[0].StockBasis.PhysicalRemaining != 10 {
		t.Fatalf("first basis overwritten: %+v", h[0])
	}
}

// TestReserveConflictTakesPrecedenceOverInvalidParams 验证已成功编号只要改了
// 请求、备件、数量或到期时刻，即使新参数本身非法，也一律按编号冲突处理。
func TestReserveConflictTakesPrecedenceOverInvalidParams(t *testing.T) {
	s := newStore(t)
	if _, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	cases := []struct {
		name      string
		requestID string
		partID    string
		quantity  int
		expiry    time.Time
	}{
		{"empty request", "", "part1", 5, expiryOK},
		{"unknown request", "rMissing", "part1", 5, expiryOK},
		{"unknown part", "r1", "partMissing", 5, expiryOK},
		{"zero quantity", "r1", "part1", 0, expiryOK},
		{"negative quantity", "r1", "part1", -1, expiryOK},
		{"past expiry", "r1", "part1", 5, nowOK.Add(-time.Second)},
		{"expiry equal now", "r1", "part1", 5, nowOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Reserve("c1", tc.requestID, tc.partID, tc.quantity, tc.expiry, nowOK)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("got %v, want ErrConflict", err)
			}
		})
	}

	// 冲突不改变承诺与库存。
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 5 || st.PhysicalRemaining != 10 {
		t.Fatalf("conflict changed state: %+v", st)
	}
	// 指定已知请求的冲突各记一条；空请求与未知请求不创建请求和历史。
	h1, _ := s.RequestHistory("r1")
	// 1 条成功 + 未知备件/零数量/负数量/过去到期/等于到期 共 5 条冲突。
	if len(h1) != 6 {
		t.Fatalf("r1 history len = %d, want 6", len(h1))
	}
	for _, rec := range h1[1:] {
		if rec.Success || rec.Error != HistoryErrorConflict ||
			rec.Eligibility != nil || rec.StockBasis != nil {
			t.Fatalf("bad conflict record: %+v", rec)
		}
	}
	if _, err := s.RequestHistory("rMissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request should not be created by conflict: %v", err)
	}
}

// TestReserveRetryResultIsImmutable 验证调用方修改首次返回值或重试返回值，
// 都不能改变以后取回的首次结果、当前承诺和预留历史。
func TestReserveRetryResultIsImmutable(t *testing.T) {
	s := newStore(t)
	first, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	first.Used = 99
	first.Canceled = true
	first.Quantity = 1
	retry, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.Used != 0 || retry.Canceled || retry.Quantity != 5 {
		t.Fatalf("first return value mutation leaked: %+v", retry)
	}
	retry.Used = 88
	again, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, expiryOK)
	if err != nil {
		t.Fatalf("retry again: %v", err)
	}
	if again.Used != 0 || again.Canceled || again.Quantity != 5 {
		t.Fatalf("retry return value mutation leaked: %+v", again)
	}
	cur, _ := s.Commitment("c1")
	if cur.Used != 0 || cur.Canceled || cur.Quantity != 5 {
		t.Fatalf("current commitment mutated: %+v", cur)
	}
	h, _ := s.RequestHistory("r1")
	if len(h) != 1 || h[0].Quantity != 5 || !h[0].Success {
		t.Fatalf("history mutated: %+v", h)
	}
}

// TestReserveFailedSubmissionRemainsReusable 验证尚未成功占用的编号继续按本次
// 参数、资格和库存判断，失败后允许用该编号再次提交，失败不固定成永久结果。
func TestReserveFailedSubmissionRemainsReusable(t *testing.T) {
	s := newStore(t)
	// 参数非法失败。
	if _, err := s.Reserve("c1", "r1", "part1", 0, expiryOK, nowOK); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero quantity: %v", err)
	}
	// 库存不足失败。
	if _, err := s.Reserve("c1", "r1", "part1", 11, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("over stock: %v", err)
	}
	// 请求过保失败。
	if _, err := s.Reserve("c1", "r1", "part1", 1, expiryOK, t0.Add(31*day)); !errors.Is(err, ErrIneligible) {
		t.Fatalf("ineligible: %v", err)
	}
	// 条件满足后同编号首次成功。
	first, err := s.Reserve("c1", "r1", "part1", 4, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve after failures: %v", err)
	}
	if first.Quantity != 4 {
		t.Fatalf("commitment = %+v", first)
	}
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 4 {
		t.Fatalf("occupied = %d, want 4", st.ActiveOccupied)
	}
	// 成功之后，再用曾经失败过的参数提交即按冲突处理。
	if _, err := s.Reserve("c1", "r1", "part1", 11, expiryOK, nowOK); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused id after success: got %v, want ErrConflict", err)
	}
}

// TestReserveRetryConcurrentWithUseAndCancel 验证并发原样重试只能取回首次成功值，
// 且只产生一次占用和一条成功历史；查询反映实际完成的使用与取消。
func TestReserveRetryConcurrentWithUseAndCancel(t *testing.T) {
	s := newStore(t)
	first, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	const retryN = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	var badRetries int
	useOK := 0
	for i := 0; i < retryN; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := s.Reserve("c1", "r1", "part1", 5, expiryOK, expiryOK.Add(time.Duration(i)*time.Hour))
			mu.Lock()
			defer mu.Unlock()
			if err != nil || got != first {
				badRetries++
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Use(fmt.Sprintf("u%d", i), "c1", 1, nowOK)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				useOK++
			case errors.Is(err, ErrCommitmentClosed), errors.Is(err, ErrUsageExceeded):
			default:
				t.Errorf("unexpected use error: %v", err)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := s.Cancel("c1", nowOK); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}()
	wg.Wait()

	if badRetries != 0 {
		t.Fatalf("%d retries did not return the first result", badRetries)
	}
	// 只有一条成功历史，重试不追加。
	h, _ := s.RequestHistory("r1")
	if len(h) != 1 || !h[0].Success {
		t.Fatalf("history = %+v, want single success", h)
	}
	// 查询反映实际完成的操作：取消后占用为零，实物只扣减成功使用量。
	st, _ := s.PartStatus("part1", nowOK)
	if st.ActiveOccupied != 0 || st.PhysicalRemaining != 10-useOK {
		t.Fatalf("state after concurrent retry/use/cancel: used=%d status=%+v", useOK, st)
	}
	d := st.Details[0]
	if d.Status != CommitmentCanceled || d.UsedQuantity != useOK {
		t.Fatalf("detail = %+v, want canceled with used=%d", d, useOK)
	}
}
