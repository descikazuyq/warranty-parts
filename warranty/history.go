package warranty

import (
	"fmt"
	"time"
)

// HistoryError 是预留提交失败的具体错误类别。
type HistoryError string

const (
	// HistoryErrorInvalidParam 表示入参不合法（如非正数量、到期时刻未晚于当前时刻）。
	HistoryErrorInvalidParam HistoryError = "invalid_param"
	// HistoryErrorConflict 表示提交编号被重复用于不同内容。
	HistoryErrorConflict HistoryError = "conflict"
	// HistoryErrorProductNotFound 表示请求关联的产品尚未登记，资格依据为空。
	HistoryErrorProductNotFound HistoryError = "product_not_found"
	// HistoryErrorPartNotFound 表示预留的备件尚未登记，库存依据为空。
	HistoryErrorPartNotFound HistoryError = "part_not_found"
	// HistoryErrorIneligible 表示请求在当次时刻不满足保修资格。
	HistoryErrorIneligible HistoryError = "ineligible"
	// HistoryErrorInsufficientStock 表示可承诺数量不足。
	HistoryErrorInsufficientStock HistoryError = "insufficient_stock"
)

// StockBasis 是提交处理前的库存依据快照。
type StockBasis struct {
	// PhysicalRemaining 是本次提交处理前的剩余实物库存。
	PhysicalRemaining int
	// ActiveOccupied 是本次提交处理前的有效承诺未用总量。
	ActiveOccupied int
	// Committable 是本次提交处理前的可承诺数量（实物剩余扣除有效占用）。
	Committable int
}

// HistoryRecord 是一次预留提交的处理历史记录。
// 每次首次预留成功及每次失败提交都会留下一条记录；同编号同内容的幂等重试不追加。
type HistoryRecord struct {
	// Seq 是记录在所属请求内的处理次序号，按处理次序严格递增。
	Seq int
	// CommitID 是本次提交的承诺编号。
	CommitID string
	// PartID 是本次提交预留的备件编号。
	PartID string
	// Quantity 是本次提交的数量。
	Quantity int
	// Expiry 是本次提交的到期时刻。
	Expiry time.Time
	// Now 是调用方本次提交给定的当前时刻。
	Now time.Time
	// Success 表示本次提交是否成功。
	Success bool
	// Error 是失败的具体错误类别；成功时为空。
	Error HistoryError
	// Eligibility 是资格依据快照；产品缺失等情况下明确为空（nil），不用合格替代缺失。
	Eligibility *Eligibility
	// StockBasis 是库存依据快照；备件缺失等情况下明确为空（nil），不用零库存替代缺失。
	StockBasis *StockBasis
}

// cloneEligibility 返回资格依据快照的深拷贝（含拒绝原因列表）。
func cloneEligibility(e *Eligibility) *Eligibility {
	if e == nil {
		return nil
	}
	cp := *e
	if len(e.Reasons) > 0 {
		cp.Reasons = append(make([]RejectionReason, 0, len(e.Reasons)), e.Reasons...)
	}
	return &cp
}

// cloneStockBasis 返回库存依据快照的深拷贝。
func cloneStockBasis(b *StockBasis) *StockBasis {
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

// appendHistoryLocked 在已持锁的情况下追加一条处理记录。
// 序号按处理次序严格递增；提交时刻相同或前后颠倒都不改变这个次序。
func (s *Store) appendHistoryLocked(requestID string, rec HistoryRecord) {
	rec.Seq = len(s.history[requestID]) + 1
	s.history[requestID] = append(s.history[requestID], rec)
}

// recordReserveLocked 在已持锁的情况下，仅当请求已知时为一次预留提交追加一条
// 处理记录。请求编号为空或尚未登记时不创建请求和历史；记录内容（成功标记或
// 失败类别、资格与库存依据快照）由调用方按当次处理结果给定，这里不补全或改写。
func (s *Store) recordReserveLocked(requestID string, rec HistoryRecord) {
	if _, ok := s.requests[requestID]; !ok {
		return
	}
	s.appendHistoryLocked(requestID, rec)
}

// RequestHistory 返回指定请求的预留处理历史，按处理次序排列，序号严格递增。
// 历史查询不需要提供当前时刻；已知请求没有记录时返回空列表。
// 返回的列表及记录均为副本，调用方修改不会影响已保存的历史与后续查询。
func (s *Store) RequestHistory(requestID string) ([]HistoryRecord, error) {
	if requestID == "" {
		return nil, fmt.Errorf("%w: request id must not be empty", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.requests[requestID]; !ok {
		return nil, fmt.Errorf("%w: request %q", ErrNotFound, requestID)
	}
	src := s.history[requestID]
	out := make([]HistoryRecord, len(src))
	for i, rec := range src {
		cp := rec
		cp.Eligibility = cloneEligibility(rec.Eligibility)
		cp.StockBasis = cloneStockBasis(rec.StockBasis)
		out[i] = cp
	}
	return out, nil
}
