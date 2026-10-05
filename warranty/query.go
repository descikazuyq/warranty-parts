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

// commitmentDetailsLocked 是两种查询入口共用的承诺明细处理规则：收集全部满足
// match 的承诺（含已取消、已到期、已全部使用的记录），按本次时刻生成明细，
// 并按承诺编号升序排序。按请求查询与按备件查询只传入不同的归属条件，过滤、
// 明细生成与排序规则不再分别维护。返回的明细是独立副本，与仓库记录互不影响。
func (s *Store) commitmentDetailsLocked(match func(*Commitment) bool, now time.Time) []CommitmentDetail {
	var details []CommitmentDetail
	for _, c := range s.commitments {
		if !match(c) {
			continue
		}
		details = append(details, toDetailLocked(c, now))
	}
	sort.Slice(details, func(i, j int) bool {
		return details[i].CommitmentID < details[j].CommitmentID
	})
	return details
}

// RequestView 返回指定请求在当前时刻的资格依据、拒绝原因和关联承诺。
// 查询会按本次当前时刻确认该请求下已到期的承诺：到期一经确认不可逆，之后
// 即使传入更早时刻，这些承诺也始终显示 expired、不再计入有效占用；到期时刻
// 前尚未确认失效的承诺仍按本次时刻显示 active。资格仍只按本次时刻判断，
// 不会用较大的历史时刻替换本次时刻。
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
	// 查询本身确认到期：本次时刻已达到到期时刻的关联承诺一经确认不可逆，
	// 之后即使传入更早时刻也不再计入占用、状态保持 expired。
	s.confirmRequestExpiriesLocked(requestID, now)
	elig, err := s.evaluateLocked(req, now)
	if err != nil {
		return nil, err
	}
	view := &RequestView{Eligibility: elig}
	view.Commitments = s.commitmentDetailsLocked(func(c *Commitment) bool {
		return c.RequestID == requestID
	}, now)
	return view, nil
}

// PartStatus 返回指定备件在当前时刻的实物剩余、有效占用、可承诺数量和占用明细。
// 查询会按本次当前时刻确认该备件下已到期的承诺：到期一经确认不可逆，只释放
// 未用占用、不增加实物库存，之后即使传入更早时刻，这些承诺也不再计入有效占用
// （可承诺数量不回落），明细持续显示 expired；已取消的记录仍显示 canceled。
func (s *Store) PartStatus(partID string, now time.Time) (*PartStatus, error) {
	if partID == "" {
		return nil, fmt.Errorf("%w: part id must not be empty", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.parts[partID]; !ok {
		return nil, fmt.Errorf("%w: part %q", ErrNotFound, partID)
	}
	// 查询本身确认到期：本次时刻已达到到期时刻的该备件承诺一经确认不可
	// 逆，之后即使传入更早时刻也不再计入占用。
	s.confirmPartExpiriesLocked(partID, now)
	// 实物剩余、有效占用与可承诺数量与新预留的库存核算共用同一条规则：
	// 占用只看持久状态，取消或已确认到期的承诺不占数量，不随本次时刻回退。
	account := s.stockAccountLocked(partID)
	view := &PartStatus{
		PartID:            partID,
		PhysicalRemaining: account.PhysicalRemaining,
		ActiveOccupied:    account.ActiveOccupied,
		Committable:       account.Committable,
	}
	view.Details = s.commitmentDetailsLocked(func(c *Commitment) bool {
		return c.PartID == partID
	}, now)
	return view, nil
}
