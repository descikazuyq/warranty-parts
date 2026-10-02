package warranty

import (
	"fmt"
	"time"
)

// activeOccupiedLocked 返回某备件在指定当前时刻的有效承诺未用总量。
// 已取消或已到期的承诺不计入占用。
func (s *Store) activeOccupiedLocked(partID string, now time.Time) int {
	total := 0
	for _, c := range s.commitments {
		if c.PartID != partID {
			continue
		}
		if c.Canceled || !now.Before(c.Expiry) {
			continue
		}
		total += c.Quantity - c.Used
	}
	return total
}

// Reserve 为请求预留一种备件，数量为正整数，到期时刻必须晚于当前时刻。
// 每次预留都按当次时刻重新判断资格；可承诺数量等于剩余实物库存扣除所有
// 有效承诺的未用数量。库存不足、未知请求或备件、不合格请求都不占用数量。
//
// 提交编号全局唯一：相同编号且相同内容的重试返回同一承诺，不追加历史；
// 换请求、备件、数量或到期时刻则返回 ErrConflict，并在本次提交指定的已知
// 请求下记录失败。每次首次预留成功及每次失败提交都会在对应请求下留下记录。
func (s *Store) Reserve(commitID, requestID, partID string, quantity int, expiry, now time.Time) (Commitment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if commitID == "" || requestID == "" || partID == "" {
		s.recordInvalidParamLocked(requestID, commitID, partID, quantity, expiry, now)
		return Commitment{}, fmt.Errorf("%w: commit/request/part id must not be empty", ErrInvalidParam)
	}
	if quantity <= 0 {
		s.recordInvalidParamLocked(requestID, commitID, partID, quantity, expiry, now)
		return Commitment{}, fmt.Errorf("%w: reserve quantity must be a positive integer", ErrInvalidParam)
	}
	if !expiry.After(now) {
		s.recordInvalidParamLocked(requestID, commitID, partID, quantity, expiry, now)
		return Commitment{}, fmt.Errorf("%w: expiry must be after current time", ErrInvalidParam)
	}

	// 幂等重试：同编号同内容返回首次结果，不追加历史，不重复占用。
	if existing, ok := s.commitments[commitID]; ok {
		if existing.RequestID != requestID || existing.PartID != partID ||
			existing.Quantity != quantity || !existing.Expiry.Equal(expiry) {
			// 冲突：在本次提交指定的已知请求下记录失败，不挂到原承诺所属请求。
			if _, known := s.requests[requestID]; known {
				s.appendHistoryLocked(requestID, HistoryRecord{
					CommitID: commitID,
					PartID:   partID,
					Quantity: quantity,
					Expiry:   expiry,
					Now:      now,
					Error:    HistoryErrorConflict,
				})
			}
			return Commitment{}, fmt.Errorf("%w: commit id %q reused with different content", ErrConflict, commitID)
		}
		return *existing, nil
	}

	req, ok := s.requests[requestID]
	if !ok {
		return Commitment{}, fmt.Errorf("%w: request %q", ErrNotFound, requestID)
	}
	part, ok := s.parts[partID]
	if !ok {
		s.appendHistoryLocked(requestID, HistoryRecord{
			CommitID: commitID,
			PartID:   partID,
			Quantity: quantity,
			Expiry:   expiry,
			Now:      now,
			Error:    HistoryErrorPartNotFound,
		})
		return Commitment{}, fmt.Errorf("%w: part %q", ErrNotFound, partID)
	}
	elig, err := s.evaluateLocked(req, now)
	if err != nil {
		// 产品未登记：资格依据明确为空，不用合格替代缺失。
		s.appendHistoryLocked(requestID, HistoryRecord{
			CommitID: commitID,
			PartID:   partID,
			Quantity: quantity,
			Expiry:   expiry,
			Now:      now,
			Error:    HistoryErrorProductNotFound,
		})
		return Commitment{}, err
	}

	// 库存依据：本次提交处理前的实物剩余、有效占用与可承诺数量。
	stockBasis := &StockBasis{
		PhysicalRemaining: part.Stock,
		ActiveOccupied:    s.activeOccupiedLocked(partID, now),
	}
	stockBasis.Committable = stockBasis.PhysicalRemaining - stockBasis.ActiveOccupied

	if !elig.Eligible {
		s.appendHistoryLocked(requestID, HistoryRecord{
			CommitID:    commitID,
			PartID:      partID,
			Quantity:    quantity,
			Expiry:      expiry,
			Now:         now,
			Error:       HistoryErrorIneligible,
			Eligibility: cloneEligibility(elig),
			StockBasis:  stockBasis,
		})
		return Commitment{}, fmt.Errorf("%w: %v", ErrIneligible, elig.Reasons)
	}

	if quantity > stockBasis.Committable {
		s.appendHistoryLocked(requestID, HistoryRecord{
			CommitID:    commitID,
			PartID:      partID,
			Quantity:    quantity,
			Expiry:      expiry,
			Now:         now,
			Error:       HistoryErrorInsufficientStock,
			Eligibility: cloneEligibility(elig),
			StockBasis:  stockBasis,
		})
		return Commitment{}, fmt.Errorf("%w: need %d, committable %d", ErrInsufficientStock, quantity, stockBasis.Committable)
	}

	c := &Commitment{
		ID:        commitID,
		RequestID: requestID,
		PartID:    partID,
		Quantity:  quantity,
		Expiry:    expiry,
	}
	// 成功记录与承诺在同一把锁内同时落库，查询不会看到只占用库存却没有
	// 成功记录的中间状态。
	s.commitments[commitID] = c
	s.appendHistoryLocked(requestID, HistoryRecord{
		CommitID:    commitID,
		PartID:      partID,
		Quantity:    quantity,
		Expiry:      expiry,
		Now:         now,
		Success:     true,
		Eligibility: cloneEligibility(elig),
		StockBasis:  stockBasis,
	})
	return *c, nil
}

// Use 使用一笔承诺，数量为正整数，分批扣减该承诺未用数量和剩余实物库存。
// 数量超过未用数量时整次失败，不改变任何记录。
//
// 使用编号全局唯一：相同编号且相同内容的重试返回首次成功结果，不再次扣减；
// 改数量或改承诺返回 ErrConflict。已经成功的使用在承诺取消或到期后重试，
// 仍返回原结果。已全部使用的承诺不再接受新使用。
func (s *Store) Use(usageID, commitID string, quantity int, now time.Time) (Usage, error) {
	if usageID == "" || commitID == "" {
		return Usage{}, fmt.Errorf("%w: usage/commit id must not be empty", ErrInvalidParam)
	}
	if quantity <= 0 {
		return Usage{}, fmt.Errorf("%w: usage quantity must be a positive integer", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.usages[usageID]; ok {
		if existing.CommitmentID != commitID || existing.Quantity != quantity {
			return Usage{}, fmt.Errorf("%w: usage id %q reused with different content", ErrConflict, usageID)
		}
		return *existing, nil
	}

	c, ok := s.commitments[commitID]
	if !ok {
		return Usage{}, fmt.Errorf("%w: commitment %q", ErrNotFound, commitID)
	}
	if c.Canceled {
		return Usage{}, fmt.Errorf("%w: commitment %q is canceled", ErrCommitmentClosed, commitID)
	}
	if !now.Before(c.Expiry) {
		return Usage{}, fmt.Errorf("%w: commitment %q expired at %v", ErrCommitmentClosed, commitID, c.Expiry)
	}
	if quantity > c.Quantity-c.Used {
		return Usage{}, fmt.Errorf("%w: need %d, unused %d", ErrUsageExceeded, quantity, c.Quantity-c.Used)
	}

	c.Used += quantity
	s.parts[c.PartID].Stock -= quantity
	u := &Usage{ID: usageID, CommitmentID: commitID, Quantity: quantity}
	s.usages[usageID] = u
	return *u, nil
}

// Cancel 取消承诺，只释放未用数量，不影响已扣减的实物库存。
// 重复取消不增加库存；承诺已到期时余量已自动失效，取消同样不产生变化。
// 取消后的承诺不能继续使用。
func (s *Store) Cancel(commitID string, now time.Time) (Commitment, error) {
	if commitID == "" {
		return Commitment{}, fmt.Errorf("%w: commit id must not be empty", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.commitments[commitID]
	if !ok {
		return Commitment{}, fmt.Errorf("%w: commitment %q", ErrNotFound, commitID)
	}
	if !c.Canceled {
		c.Canceled = true
	}
	return *c, nil
}
