package warranty

import (
	"fmt"
	"sort"
	"time"
)

// CommitmentDetail 是承诺的明细视图，已取消或已到期的记录仍可追查。
type CommitmentDetail struct {
	// CommitmentID 是承诺编号。
	CommitmentID string
	// RequestID 是关联请求编号。
	RequestID string
	// PartID 是备件编号。
	PartID string
	// OriginalQuantity 是原定承诺数量。
	OriginalQuantity int
	// UsedQuantity 是已用数量。
	UsedQuantity int
	// RemainingQuantity 是剩余未用数量。
	RemainingQuantity int
	// Expiry 是到期时刻。
	Expiry time.Time
	// Status 是在查询时刻的状态（active/canceled/expired）。
	Status CommitmentStatus
}

// PartStatus 是按备件查询的库存与占用视图。
type PartStatus struct {
	// PartID 是备件编号。
	PartID string
	// PhysicalRemaining 是剩余实物库存（初始库存扣除全部成功使用量）。
	PhysicalRemaining int
	// ActiveOccupied 是有效承诺的未用总量。
	ActiveOccupied int
	// Committable 是还可承诺数量，等于剩余实物库存扣除有效承诺未用总量。
	Committable int
	// Details 是该备件的全部承诺明细（含已取消、已到期），按承诺编号排序。
	Details []CommitmentDetail
}

// RequestView 是按请求查询的资格与承诺视图。
type RequestView struct {
	// Eligibility 是资格依据与拒绝原因。
	Eligibility *Eligibility
	// Commitments 是该请求关联的全部承诺明细（含已取消、已到期），按承诺编号排序。
	Commitments []CommitmentDetail
}

// toDetailLocked 生成承诺在指定时刻的明细。
func toDetailLocked(c *Commitment, now time.Time) CommitmentDetail {
	return CommitmentDetail{
		CommitmentID:      c.ID,
		RequestID:         c.RequestID,
		PartID:            c.PartID,
		OriginalQuantity:  c.Quantity,
		UsedQuantity:      c.Used,
		RemainingQuantity: c.Quantity - c.Used,
		Expiry:            c.Expiry,
		Status:            c.Status(now),
	}
}

// confirmExpiryForRequestLocked 在已持锁的情况下，将指定请求关联的、在当前时刻
// 已到期的未取消承诺确认为已到期。到期不可撤销：确认后即使传入更早的当前时刻，
// 这些承诺也不再恢复占用。本函数只处理该请求涉及的承诺。
func (s *Store) confirmExpiryForRequestLocked(requestID string, now time.Time) {
	for _, c := range s.commitments {
		if c.RequestID != requestID || c.Canceled || c.Expired {
			continue
		}
		if !now.Before(c.Expiry) {
			c.Expired = true
		}
	}
}

// RequestView 返回指定请求在当前时刻的资格依据、拒绝原因和关联承诺。
func (s *Store) RequestView(requestID string, now time.Time) (*RequestView, error) {
	if requestID == "" {
		return nil, fmt.Errorf("%w: request id must not be empty", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.requests[requestID]
	if !ok {
		return nil, fmt.Errorf("%w: request %q", ErrNotFound, requestID)
	}
	// 明细查询确认本请求涉及的已到期承诺，到期释放不可撤销。
	s.confirmExpiryForRequestLocked(requestID, now)
	elig, err := s.evaluateLocked(req, now)
	if err != nil {
		return nil, err
	}
	view := &RequestView{Eligibility: elig}
	for _, c := range s.commitments {
		if c.RequestID != requestID {
			continue
		}
		view.Commitments = append(view.Commitments, toDetailLocked(c, now))
	}
	sort.Slice(view.Commitments, func(i, j int) bool {
		return view.Commitments[i].CommitmentID < view.Commitments[j].CommitmentID
	})
	return view, nil
}

// PartStatus 返回指定备件在当前时刻的实物剩余、有效占用、可承诺数量和占用明细。
func (s *Store) PartStatus(partID string, now time.Time) (*PartStatus, error) {
	if partID == "" {
		return nil, fmt.Errorf("%w: part id must not be empty", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	part, ok := s.parts[partID]
	if !ok {
		return nil, fmt.Errorf("%w: part %q", ErrNotFound, partID)
	}
	// 库存查询确认本备件涉及的已到期承诺，到期释放不可撤销。
	s.confirmExpiryLocked(partID, now)
	view := &PartStatus{PartID: partID, PhysicalRemaining: part.Stock}
	for _, c := range s.commitments {
		if c.PartID != partID {
			continue
		}
		view.Details = append(view.Details, toDetailLocked(c, now))
		if !c.Canceled && !c.Expired && now.Before(c.Expiry) {
			view.ActiveOccupied += c.Quantity - c.Used
		}
	}
	sort.Slice(view.Details, func(i, j int) bool {
		return view.Details[i].CommitmentID < view.Details[j].CommitmentID
	})
	view.Committable = view.PhysicalRemaining - view.ActiveOccupied
	return view, nil
}
