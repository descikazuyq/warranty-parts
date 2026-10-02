package warranty

import (
	"errors"
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

// cloneEligibilityLocked 返回资格依据的独立副本，拒绝原因切片也一并复制，
// 使保存进历史的快照不与任何返回给调用方的对象共享底层数组。
func cloneEligibilityLocked(e *Eligibility) *Eligibility {
	cp := *e
	if e.Reasons != nil {
		cp.Reasons = append([]RejectionReason(nil), e.Reasons...)
	}
	return &cp
}

// Reserve 为请求预留一种备件，数量为正整数，到期时刻必须晚于当前时刻。
// 每次预留都按当次时刻重新判断资格；可承诺数量等于剩余实物库存扣除所有
// 有效承诺的未用数量。库存不足、未知请求或备件、不合格请求都不占用数量。
//
// 提交编号全局唯一：相同编号且相同内容的重试返回同一承诺；换请求、备件、
// 数量或到期时刻则返回 ErrConflict。
//
// 对已登记的请求，首次预留成功及每次失败提交都会在该请求的预留历史中
// 留下记录，快照本次提交时刻的资格依据与处理前的库存依据；同编号同内容的
// 成功重试不追加记录、不重复占用。冲突失败记录挂在本次提交指定的已知请求下
// （编号原本属于另一请求时也不例外）。未知请求的提交不创建请求，也不留历史。
func (s *Store) Reserve(commitID, requestID, partID string, quantity int, expiry, now time.Time) (Commitment, error) {
	// 参数校验与原实现一致，按编号、数量、到期时刻的次序取第一项错误。
	invalidMsg := ""
	switch {
	case commitID == "" || requestID == "" || partID == "":
		invalidMsg = "commit/request/part id must not be empty"
	case quantity <= 0:
		invalidMsg = "reserve quantity must be a positive integer"
	case !expiry.After(now):
		invalidMsg = "expiry must be after current time"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if invalidMsg != "" {
		// 参数无效：仅当请求已登记时在该请求下留记录，依据明确为空。
		if requestID != "" {
			if _, ok := s.requests[requestID]; ok {
				s.appendHistoryLocked(requestID, &HistoryEntry{
					CommitmentID: commitID,
					PartID:       partID,
					Quantity:     quantity,
					Expiry:       expiry,
					SubmittedAt:  now,
					Outcome:      HistoryInvalidParam,
				})
			}
		}
		return Commitment{}, fmt.Errorf("%w: %s", ErrInvalidParam, invalidMsg)
	}

	if existing, ok := s.commitments[commitID]; ok {
		if existing.RequestID != requestID || existing.PartID != partID ||
			existing.Quantity != quantity || !existing.Expiry.Equal(expiry) {
			// 冲突失败记录挂在本次提交指定的已知请求下；该请求未知时按原约定报错且不留历史。
			if _, reqKnown := s.requests[requestID]; reqKnown {
				s.appendHistoryLocked(requestID, &HistoryEntry{
					CommitmentID: commitID,
					PartID:       partID,
					Quantity:     quantity,
					Expiry:       expiry,
					SubmittedAt:  now,
					Outcome:      HistoryConflict,
				})
			}
			return Commitment{}, fmt.Errorf("%w: commit id %q reused with different content", ErrConflict, commitID)
		}
		// 同编号同内容的成功重试：沿用已有行为，不追加历史、不重复占用。
		return *existing, nil
	}

	req, ok := s.requests[requestID]
	if !ok {
		// 未知请求不创建请求，也不创建历史。
		return Commitment{}, fmt.Errorf("%w: request %q", ErrNotFound, requestID)
	}

	entry := &HistoryEntry{
		CommitmentID: commitID,
		PartID:       partID,
		Quantity:     quantity,
		Expiry:       expiry,
		SubmittedAt:  now,
	}
	part, partKnown := s.parts[partID]
	if !partKnown {
		// 备件缺失（原实现先于资格判断）：资格与库存依据都明确为空，
		// 不能用合格或零库存代替缺失。
		entry.Outcome = HistoryNotFound
		s.appendHistoryLocked(requestID, entry)
		return Commitment{}, fmt.Errorf("%w: part %q", ErrNotFound, partID)
	}

	elig, err := s.evaluateLocked(req, now)
	if err != nil {
		// 缺少产品（或故障代码缺失）：依据无法计算，资格与库存依据留空。
		if errors.Is(err, ErrNotFound) {
			entry.Outcome = HistoryNotFound
		} else {
			entry.Outcome = HistoryInvalidParam
		}
		s.appendHistoryLocked(requestID, entry)
		return Commitment{}, err
	}

	// 资格可算且备件存在：成功、不合格、库存不足三种结果都保存完整依据。
	entry.Eligibility = cloneEligibilityLocked(elig)
	basis := &StockBasis{
		PhysicalRemaining: part.Stock,
		ActiveOccupied:    s.activeOccupiedLocked(partID, now),
	}
	basis.Committable = basis.PhysicalRemaining - basis.ActiveOccupied
	entry.Stock = basis

	if !elig.Eligible {
		entry.Outcome = HistoryIneligible
		s.appendHistoryLocked(requestID, entry)
		return Commitment{}, fmt.Errorf("%w: %v", ErrIneligible, elig.Reasons)
	}
	if quantity > basis.Committable {
		entry.Outcome = HistoryInsufficientStock
		s.appendHistoryLocked(requestID, entry)
		return Commitment{}, fmt.Errorf("%w: need %d, committable %d", ErrInsufficientStock, quantity, basis.Committable)
	}

	c := &Commitment{
		ID:        commitID,
		RequestID: requestID,
		PartID:    partID,
		Quantity:  quantity,
		Expiry:    expiry,
	}
	s.commitments[commitID] = c
	entry.Outcome = HistorySucceeded
	s.appendHistoryLocked(requestID, entry)
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
