package warranty

import (
	"fmt"
	"time"
)

// HistoryOutcome 是一次预留提交的处理结果类别。
type HistoryOutcome string

const (
	// HistorySucceeded 表示本次预留首次提交成功并创建了承诺。
	HistorySucceeded HistoryOutcome = "succeeded"
	// HistoryInvalidParam 表示本次提交参数不合法（缺少编号、数量非正整数、到期时刻不晚于当前时刻等）。
	HistoryInvalidParam HistoryOutcome = "invalid_param"
	// HistoryConflict 表示提交编号已用于不同内容。
	HistoryConflict HistoryOutcome = "conflict"
	// HistoryNotFound 表示请求、产品或备件尚未登记。
	HistoryNotFound HistoryOutcome = "not_found"
	// HistoryIneligible 表示请求在本次提交时刻不满足保修资格。
	HistoryIneligible HistoryOutcome = "ineligible"
	// HistoryInsufficientStock 表示本次提交时刻可承诺数量不足。
	HistoryInsufficientStock HistoryOutcome = "insufficient_stock"
)

// StockBasis 是本次提交处理前的库存依据。
type StockBasis struct {
	// PhysicalRemaining 是处理前的剩余实物库存（初始库存扣除全部成功使用量）。
	PhysicalRemaining int
	// ActiveOccupied 是处理前的有效承诺未用总量。
	ActiveOccupied int
	// Committable 是处理前的可承诺数量，等于剩余实物库存扣除有效承诺未用总量。
	Committable int
}

// HistoryEntry 是某请求下一次预留提交的处理记录。记录只增不改，
// 快照提交当时的资格与库存依据；承诺随后被使用、取消或到期，记录内容也不变化。
type HistoryEntry struct {
	// Seq 是该请求内严格递增的处理序号，从 1 开始，按实际处理次序赋值。
	Seq int
	// CommitmentID 是本次提交使用的承诺编号（失败提交同样保留，编号不被占用）。
	CommitmentID string
	// PartID 是本次提交指定的备件编号。
	PartID string
	// Quantity 是本次提交请求的数量（参数不合法时可能为零或负）。
	Quantity int
	// Expiry 是本次提交指定的到期时刻。
	Expiry time.Time
	// SubmittedAt 是调用方给定的当前时刻。
	SubmittedAt time.Time
	// Outcome 是本次提交的结果类别。
	Outcome HistoryOutcome

	// Eligibility 是资格与库存相关提交（成功、不合格、库存不足）当次的资格依据快照；
	// 缺少产品或备件、参数无效、编号冲突时为 nil，明确表示依据缺失，
	// 不会用合格或零库存依据代替。
	Eligibility *Eligibility
	// Stock 是成功、不合格、库存不足记录在处理前的库存依据快照；
	// 其余结果类别为 nil。
	Stock *StockBasis
}

// ReservationHistory 返回指定请求的预留处理历史，按处理次序排列，序号严格递增。
// 历史不需要当前时刻：记录的是每次提交当时的依据与结果，不随后续状态变化。
// 已知请求没有记录时返回空列表；空请求编号返回 ErrInvalidParam；
// 未知请求返回 ErrNotFound。
//
// 返回的切片及其中的拒绝原因均为快照副本，调用方修改不影响保存的历史、
// 后续查询或任何业务记录。
func (s *Store) ReservationHistory(requestID string) ([]HistoryEntry, error) {
	if requestID == "" {
		return nil, fmt.Errorf("%w: request id must not be empty", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.requests[requestID]; !ok {
		return nil, fmt.Errorf("%w: request %q", ErrNotFound, requestID)
	}
	entries := s.history[requestID]
	out := make([]HistoryEntry, len(entries))
	for i, e := range entries {
		cp := *e
		if e.Eligibility != nil {
			ec := *e.Eligibility
			if e.Eligibility.Reasons != nil {
				ec.Reasons = append([]RejectionReason(nil), e.Eligibility.Reasons...)
			}
			cp.Eligibility = &ec
		}
		if e.Stock != nil {
			sc := *e.Stock
			cp.Stock = &sc
		}
		out[i] = cp
	}
	return out, nil
}

// appendHistoryLocked 在已持锁的情况下追加一条历史记录并返回其序号。
func (s *Store) appendHistoryLocked(requestID string, e *HistoryEntry) {
	e.Seq = len(s.history[requestID]) + 1
	s.history[requestID] = append(s.history[requestID], e)
}
