package warranty

import (
	"fmt"
	"math"
	"time"
)

// unixEpochToInternal 是 Unix 秒与 time.Time 内部绝对秒之间的固定偏移
// （公元 1 年 1 月 1 日到 1970 年 1 月 1 日的秒数）。
const unixEpochToInternal = 62135596800

// secondsPerDay 是每天的秒数；保修期每天固定二十四小时，与时区无关。
const secondsPerDay = 24 * 60 * 60

// warrantyExpiry 计算保修截止时刻：自购买时刻起完整 days 个二十四小时。
// 不经过 time.Duration（其上限约 106751 天），较长保修期限不会溢出回绕；
// 秒以下精度与购买时刻的时区表示原样保留。days 为正整数但截止时刻超出
// 时间类型的可表示范围时返回 ok=false。
func warrantyExpiry(purchase time.Time, days int) (expiry time.Time, ok bool) {
	d := int64(days)
	if d <= 0 || d > math.MaxInt64/secondsPerDay {
		return time.Time{}, false
	}
	add := d * secondsPerDay
	unix := purchase.Unix()
	// 可表示的最大 Unix 秒为 math.MaxInt64 - unixEpochToInternal；
	// 写成 unix > max-add 避免任何中间量溢出。
	if unix > math.MaxInt64-unixEpochToInternal-add {
		return time.Time{}, false
	}
	return time.Unix(unix+add, int64(purchase.Nanosecond())).In(purchase.Location()), true
}

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
	// 登记时已校验截止时刻可表示，这里一定成功。
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
