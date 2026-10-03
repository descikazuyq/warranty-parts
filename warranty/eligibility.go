package warranty

import (
	"fmt"
	"math"
	"time"
)

// RejectionReason 是资格拒绝原因。
type RejectionReason string

const (
	// ReasonPurchaseInFuture 表示购买时刻晚于当前时刻。
	ReasonPurchaseInFuture RejectionReason = "purchase_time_in_future"
	// ReasonWarrantyExpired 表示当前时刻已达到或超过保修截止时刻。
	ReasonWarrantyExpired RejectionReason = "warranty_expired"
	// ReasonFaultExcluded 表示故障代码在除外清单中。
	ReasonFaultExcluded RejectionReason = "fault_code_excluded"
)

// Eligibility 是按请求查询的资格依据与拒绝原因。
type Eligibility struct {
	// RequestID 是请求编号。
	RequestID string
	// ProductID 是产品编号。
	ProductID string
	// FaultCode 是故障代码。
	FaultCode string
	// Eligible 表示在当次时刻是否合格。
	Eligible bool
	// Reasons 列出全部拒绝原因；过保与除外同时成立时会列出两项。
	Reasons []RejectionReason
	// PurchaseTime 是购买时刻（资格依据）。
	PurchaseTime time.Time
	// WarrantyDays 是保修天数（资格依据）。
	WarrantyDays int
	// WarrantyExpiry 是保修截止时刻，自购买时刻起按每天二十四小时计算；
	// 当前时刻达到该时刻即算过保。
	WarrantyExpiry time.Time
	// Excluded 表示故障代码是否命中除外清单。
	Excluded bool
}

// warrantyExpiry 计算保修截止时刻：自购买时刻起 days 个完整的二十四小时，
// 每天固定二十四小时，不按自然日对齐，秒以下精度保留，时区只影响表示。
// days 必须为正整数。单次 time.Duration 只能表示约 292 年的纳秒数，更长的
// 期限分段累加以避免溢出。若截止时刻超出现有时间类型的可表示范围
// （time.Time 内部以自元年起的 int64 秒表示），返回 ok=false。
func warrantyExpiry(purchase time.Time, days int) (expiry time.Time, ok bool) {
	const secondsPerDay = 24 * 60 * 60
	// time.Time 内部秒数自元年起算，1970 年 1 月 1 日距元年 62135596800 秒，
	// 因此可表示的最大 Unix 秒数要预留这段偏移。
	const maxUnix = math.MaxInt64 - 62135596800
	d := int64(days)
	if d > maxUnix/secondsPerDay || purchase.Unix() > maxUnix-d*secondsPerDay {
		return time.Time{}, false
	}
	// 每段 100000 天约 273.8 年，纳秒数远在 int64 范围内。
	const chunkDays = 100000
	t := purchase
	for remaining := days; remaining > 0; remaining -= chunkDays {
		n := remaining
		if n > chunkDays {
			n = chunkDays
		}
		t = t.Add(time.Duration(n) * 24 * time.Hour)
	}
	return t, true
}

// evaluateLocked 在已持锁的情况下计算请求在当前时刻的资格。
// 未知产品返回 ErrNotFound，缺少故障代码返回 ErrInvalidParam。
func (s *Store) evaluateLocked(req *Request, now time.Time) (*Eligibility, error) {
	prod, ok := s.products[req.ProductID]
	if !ok {
		return nil, fmt.Errorf("%w: product %q", ErrNotFound, req.ProductID)
	}
	if req.FaultCode == "" {
		return nil, fmt.Errorf("%w: fault code must not be empty", ErrInvalidParam)
	}
	// 登记时已校验期限可表示，这里不会失败。
	expiry, _ := warrantyExpiry(prod.PurchaseTime, prod.WarrantyDays)
	e := &Eligibility{
		RequestID:      req.ID,
		ProductID:      prod.ID,
		FaultCode:      req.FaultCode,
		PurchaseTime:   prod.PurchaseTime,
		WarrantyDays:   prod.WarrantyDays,
		WarrantyExpiry: expiry,
	}
	if prod.PurchaseTime.After(now) {
		e.Reasons = append(e.Reasons, ReasonPurchaseInFuture)
	}
	if !now.Before(e.WarrantyExpiry) {
		e.Reasons = append(e.Reasons, ReasonWarrantyExpired)
	}
	if _, excluded := prod.ExcludedCodes[req.FaultCode]; excluded {
		e.Excluded = true
		e.Reasons = append(e.Reasons, ReasonFaultExcluded)
	}
	e.Eligible = len(e.Reasons) == 0
	return e, nil
}

// Evaluate 返回指定请求在当前时刻的资格依据与拒绝原因。
// 未知产品或缺少故障代码时明确失败，不产生任何承诺。
func (s *Store) Evaluate(requestID string, now time.Time) (*Eligibility, error) {
	if requestID == "" {
		return nil, fmt.Errorf("%w: request id must not be empty", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[requestID]
	if !ok {
		return nil, fmt.Errorf("%w: request %q", ErrNotFound, requestID)
	}
	return s.evaluateLocked(req, now)
}
