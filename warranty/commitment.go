package warranty

import (
	"fmt"
	"time"
)

// activeOccupiedLocked 返回某备件在指定当前时刻的有效承诺未用总量。
// 已取消或已确认到期的承诺不计入占用；到期不可撤销，确认后即使传入更早的
// 当前时刻也不再计入。
func (s *Store) activeOccupiedLocked(partID string, now time.Time) int {
	total := 0
	for _, c := range s.commitments {
		if c.PartID != partID || c.Canceled || c.Expired {
			continue
		}
		if !now.Before(c.Expiry) {
			continue
		}
		total += c.Quantity - c.Used
	}
	return total
}

// confirmExpiryLocked 在已持锁的情况下，将指定备件在当前时刻已到期的未取消
// 承诺确认为已到期。到期只释放未用占用，不抹去已用数量，且不可撤销：确认后
// 即使后续调用传入更早的当前时刻，这些承诺也不再恢复占用，新使用一律被拒。
// 本函数只处理该备件涉及的承诺；原样重试与提前失败的调用不调用它。
func (s *Store) confirmExpiryLocked(partID string, now time.Time) {
	for _, c := range s.commitments {
		if c.PartID != partID || c.Canceled || c.Expired {
			continue
		}
		if !now.Before(c.Expiry) {
			c.Expired = true
		}
	}
}

// Reserve 为请求预留一种备件，数量为正整数，到期时刻必须晚于本次提交的当前时刻。
// 每次首次预留都按当次时刻重新判断资格；可承诺数量等于剩余实物库存扣除所有
// 有效承诺的未用数量。库存不足、未知请求或备件、不合格请求都不占用数量。
//
// 提交编号全局唯一。已成功的编号：
//   - 原样重试（请求编号、备件编号、数量和到期时刻一致；当前时刻不属于提交内容，
//     到期时刻按实际时刻比较，换时区表示仍算一致）只取回首次预留成功的完整承诺，
//     其中已用数量仍为零、取消标记仍为否，不重新判断资格与库存，不重复占用，不
//     追加历史，也不恢复已释放占用或补回已扣减的实物库存；无论请求后来过保、库存
//     不足，还是承诺已分批使用、全部使用、取消或到期，即使本次当前时刻等于或晚于
//     原到期时刻，均如此。
//   - 改请求、备件、数量或到期时刻任一项即返回 ErrConflict，即使新参数本身非法
//     （空请求、未知备件、零数量、已过去的到期时刻）也按编号冲突处理；失败记录挂在
//     本次提交指定的已知请求下，请求为空或不存在时不创建请求和历史。
//
// 尚未成功占用的编号继续按本次参数、资格和库存判断；失败记录照常保留，失败后允许
// 用该编号再次提交。每次首次预留成功及每次失败提交都会在对应请求下留下记录。
func (s *Store) Reserve(commitID, requestID, partID string, quantity int, expiry, now time.Time) (Commitment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 已成功的编号优先按重复提交处理，优先于本次参数校验：原样重试不因本次当前
	// 时刻晚于到期时刻等参数问题失败；改内容则一律冲突。
	if first, ok := s.firstResults[commitID]; ok {
		if first.RequestID != requestID || first.PartID != partID ||
			first.Quantity != quantity || !first.Expiry.Equal(expiry) {
			// 冲突：在本次提交指定的已知请求下记录失败，不挂到原承诺所属请求，
			// 请求为空或不存在时不创建请求和历史。
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
		// 原样重试：返回首次成功时的承诺快照，不反映后来的使用、取消或到期。
		return first, nil
	}

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

	// 库存核算：先确认本备件在当前时刻已到期的承诺，到期释放不可撤销；
	// 库存不足等后续失败也不改变已确认的到期事实。
	s.confirmExpiryLocked(partID, now)

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
	// 成功记录、承诺与首次结果快照在同一把锁内同时落库，查询不会看到只占用库存
	// 却没有成功记录的中间状态；快照独立于活动承诺，之后的使用、取消或到期不
	// 会改变原样重试取回的值。
	s.commitments[commitID] = c
	s.firstResults[commitID] = *c
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
	if c.Expired {
		// 已确认到期：即使本次时刻早于到期时刻也不再开放，新使用一律拒绝。
		return Usage{}, fmt.Errorf("%w: commitment %q is expired", ErrCommitmentClosed, commitID)
	}
	if !now.Before(c.Expiry) {
		// 本次使用判断确认承诺到期：到期只释放未用占用，不抹去已用数量，
		// 且不可撤销——之后即使传入更早的时刻也不能再次使用或占用。
		c.Expired = true
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
