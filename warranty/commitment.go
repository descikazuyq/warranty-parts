package warranty

import (
	"fmt"
	"time"
)

// activeOccupiedLocked 返回某备件当前仍有效承诺的未用总量。
// 已取消或已确认到期的承诺不计入占用；到期一经确认不可逆，与本次传入的
// 当前时刻无关，因此这里不再接收当前时刻，时刻相关的到期确认由调用方
// 在核算前对本次涉及的承诺显式完成。
func (s *Store) activeOccupiedLocked(partID string) int {
	total := 0
	for _, c := range s.commitments {
		if c.PartID != partID {
			continue
		}
		if c.Canceled || c.Expired {
			continue
		}
		total += c.Quantity - c.Used
	}
	return total
}

// Reserve 为请求预留一种备件，数量为正整数，到期时刻必须晚于本次提交的当前时刻。
// 每次首次预留都按当次时刻重新判断资格；可承诺数量等于剩余实物库存扣除所有
// 有效承诺的未用数量。库存不足、未知请求或备件、不合格请求都不占用数量。
//
// 新预留的库存核算会先按本次当前时刻确认该备件下已到期的承诺：当前时刻达到
// 其到期时刻的未取消承诺被永久标记到期，只释放未用占用、不增加实物库存；该
// 结果不可逆，之后即使传入更早时刻核算也不再计算其未用数量，不能再次占用已
// 释放给其他请求的数量。空编号、非正数量等参数非法，或请求、备件（含资格所
// 需产品）不存在而提前失败的调用，不用本次时刻确认任何承诺。
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

	// 本次提交的历史记录骨架：承诺编号、备件、数量、到期时刻与当前时刻在各分支
	// 一致，统一在这里填写，各分支不再重复。record 按统一规则留痕：成功标记与
	// 失败类别互斥，资格与库存依据快照原样保存（提前失败时为 nil，不用合格资格
	// 或零库存替代缺失）；请求为空或尚未登记时不创建请求和历史。
	base := HistoryRecord{
		CommitID: commitID,
		PartID:   partID,
		Quantity: quantity,
		Expiry:   expiry,
		Now:      now,
	}
	record := func(success bool, herr HistoryError, elig *Eligibility, basis *StockBasis) {
		rec := base
		rec.Success = success
		rec.Error = herr
		rec.Eligibility = cloneEligibility(elig)
		rec.StockBasis = basis
		s.recordReserveLocked(requestID, rec)
	}

	// 已成功的编号优先按重复提交处理，优先于本次参数校验：原样重试不因本次当前
	// 时刻晚于到期时刻等参数问题失败；改内容则一律冲突。
	if first, ok := s.firstResults[commitID]; ok {
		if first.RequestID != requestID || first.PartID != partID ||
			first.Quantity != quantity || !first.Expiry.Equal(expiry) {
			// 冲突：在本次提交指定的已知请求下记录失败，不挂到原承诺所属请求。
			record(false, HistoryErrorConflict, nil, nil)
			return Commitment{}, fmt.Errorf("%w: commit id %q reused with different content", ErrConflict, commitID)
		}
		// 原样重试：返回首次成功时的承诺快照，不反映后来的使用、取消或到期。
		return first, nil
	}

	if commitID == "" || requestID == "" || partID == "" {
		record(false, HistoryErrorInvalidParam, nil, nil)
		return Commitment{}, fmt.Errorf("%w: commit/request/part id must not be empty", ErrInvalidParam)
	}
	if quantity <= 0 {
		record(false, HistoryErrorInvalidParam, nil, nil)
		return Commitment{}, fmt.Errorf("%w: reserve quantity must be a positive integer", ErrInvalidParam)
	}
	if !expiry.After(now) {
		record(false, HistoryErrorInvalidParam, nil, nil)
		return Commitment{}, fmt.Errorf("%w: expiry must be after current time", ErrInvalidParam)
	}

	req, ok := s.requests[requestID]
	if !ok {
		return Commitment{}, fmt.Errorf("%w: request %q", ErrNotFound, requestID)
	}
	part, ok := s.parts[partID]
	if !ok {
		record(false, HistoryErrorPartNotFound, nil, nil)
		return Commitment{}, fmt.Errorf("%w: part %q", ErrNotFound, partID)
	}
	elig, err := s.evaluateLocked(req, now)
	if err != nil {
		// 产品未登记：资格依据明确为空，不用合格替代缺失。
		record(false, HistoryErrorProductNotFound, nil, nil)
		return Commitment{}, err
	}

	// 库存核算前先按本次时刻确认该备件已到期的承诺。到期确认不可逆，之后
	// 即使传入更早时刻也不再计入占用。放在参数与引用校验之后：提前失败的
	// 非法或缺失引用调用不确认任何承诺。
	s.confirmPartExpiriesLocked(partID, now)

	// 库存依据：本次提交处理前的实物剩余、有效占用与可承诺数量。
	stockBasis := &StockBasis{
		PhysicalRemaining: part.Stock,
		ActiveOccupied:    s.activeOccupiedLocked(partID),
	}
	stockBasis.Committable = stockBasis.PhysicalRemaining - stockBasis.ActiveOccupied

	if !elig.Eligible {
		record(false, HistoryErrorIneligible, elig, stockBasis)
		return Commitment{}, fmt.Errorf("%w: %v", ErrIneligible, elig.Reasons)
	}

	if quantity > stockBasis.Committable {
		record(false, HistoryErrorInsufficientStock, elig, stockBasis)
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
	record(true, "", elig, stockBasis)
	return *c, nil
}

// Use 使用一笔承诺，数量为正整数，分批扣减该承诺未用数量和剩余实物库存。
// 数量超过未用数量时整次失败，不改变任何记录。
//
// 使用编号全局唯一：相同编号且相同内容的重试返回首次成功结果，不再次扣减，
// 也不借本次时刻确认任何承诺到期；编号一旦已有成功记录，后续提交的承诺编号或
// 数量与首次成功内容不同即返回 ErrConflict——即使新内容本身非法（空承诺编号、
// 指向不存在的承诺、数量为零或负数、超过承诺余量），或新指定的承诺已取消或
// 到期，也一律按编号冲突处理：不扣减实物库存、不改变任何承诺的已用数量、不
// 释放占用，也不借本次时刻确认原承诺或新承诺到期，更不会用新内容覆盖原记录。
// 首次成功与编号绑定在同一临界区内原子完成：多个相同内容的并发提交全部成功并
// 返回同一份完整使用记录，但承诺累计已用数量与实物库存只随首次成功变化一次
// （恰好耗尽余量时其余提交仍取回首次结果，不会被当作新的超量使用）；首次成功
// 前两组不同内容并发抢占同一编号时，只有一份内容成为成功记录，与其相同的提交
// 全部取回该结果，另一组全部 ErrConflict，扣减量只对应胜出内容，落败承诺不变。
// 已经成功的使用在承诺取消或到期后原样重试，仍返回原结果，已确认到期的承诺
// 也不会因此重新开放。新使用（非重试）会按本次当前时刻确认这一笔承诺：已取消
// 或已确认到期，或本次时刻达到其到期时刻的，一律返回 ErrCommitmentClosed；到期
// 一经确认不可逆，之后即使传入更早时刻也仍被拒绝。尚无成功记录的编号按本次
// 参数处理：空编号、空承诺编号、非正数量返回 ErrInvalidParam，引用未知承诺返回
// ErrNotFound，这些提前失败不用本次时刻确认到期，也不占用编号，修正内容后仍可
// 提交。已全部使用的承诺不再接受新使用。
func (s *Store) Use(usageID, commitID string, quantity int, now time.Time) (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 已成功的编号优先按重复提交处理，优先于本次参数校验：沿用成功编号但把
	// 承诺编号改成空串或未知承诺、把数量改成零或负数等本身非法的内容，仍属于
	// 对已成功提交内容的改动，按 ErrConflict 处理，与普通非法输入区分。成功
	// 使用的编号必非空，因此空编号不会命中这里，继续走参数校验。
	if existing, ok := s.usages[usageID]; ok {
		if existing.CommitmentID != commitID || existing.Quantity != quantity {
			return Usage{}, fmt.Errorf("%w: usage id %q reused with different content", ErrConflict, usageID)
		}
		return *existing, nil
	}

	if usageID == "" || commitID == "" {
		return Usage{}, fmt.Errorf("%w: usage/commit id must not be empty", ErrInvalidParam)
	}
	if quantity <= 0 {
		return Usage{}, fmt.Errorf("%w: usage quantity must be a positive integer", ErrInvalidParam)
	}

	c, ok := s.commitments[commitID]
	if !ok {
		return Usage{}, fmt.Errorf("%w: commitment %q", ErrNotFound, commitID)
	}
	// 新使用判断：按本次时刻确认这一笔承诺是否到期。一经确认不可逆，之后
	// 即使传入更早时刻也一律关闭。已取消的承诺不确认到期，仍保持 canceled。
	s.confirmExpiryLocked(c, now)
	if c.Canceled {
		return Usage{}, fmt.Errorf("%w: commitment %q is canceled", ErrCommitmentClosed, commitID)
	}
	if c.Expired {
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
