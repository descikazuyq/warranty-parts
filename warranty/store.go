package warranty

import (
	"fmt"
	"sync"
	"time"
)

// Product 是登记的产品保修信息。
type Product struct {
	// ID 是唯一产品编号。
	ID string
	// PurchaseTime 是购买时刻。
	PurchaseTime time.Time
	// WarrantyDays 是保修天数，必须为正整数；保修期自购买时刻起，每天按二十四小时计算。
	WarrantyDays int
	// ExcludedCodes 是除外故障代码集合；命中代码的请求一律拒绝。
	ExcludedCodes map[string]struct{}
}

// Part 是登记的备件及其库存。
type Part struct {
	// ID 是唯一备件编号。
	ID string
	// Stock 是当前剩余实物库存，被成功使用的承诺分批扣减。
	Stock int
}

// Request 是提交的保修请求，指定产品和故障代码。
type Request struct {
	// ID 是唯一请求编号。
	ID string
	// ProductID 是关联的产品编号。
	ProductID string
	// FaultCode 是故障代码，不能为空。
	FaultCode string
}

// CommitmentStatus 是承诺的当前状态。
type CommitmentStatus string

const (
	// CommitmentActive 表示承诺有效：未取消且未到期。
	CommitmentActive CommitmentStatus = "active"
	// CommitmentCanceled 表示承诺已被取消。
	CommitmentCanceled CommitmentStatus = "canceled"
	// CommitmentExpired 表示承诺已到期，余量自动释放。
	CommitmentExpired CommitmentStatus = "expired"
)

// Commitment 是一笔备件承诺。
type Commitment struct {
	// ID 是全局唯一的提交编号。
	ID string
	// RequestID 是关联的请求编号。
	RequestID string
	// PartID 是预留的备件编号。
	PartID string
	// Quantity 是原定承诺数量（正整数）。
	Quantity int
	// Used 是已成功使用的累计数量。
	Used int
	// Expiry 是到期时刻；当前时刻等于或晚于它时承诺到期，余量释放。
	Expiry time.Time
	// Canceled 表示是否已被取消。
	Canceled bool
	// Expired 表示到期是否已被确认。到期确认是不可逆的结果：一经确认，
	// 未用占用永久释放，之后即使传入更早的当前时刻也仍视为到期，不能重新
	// 占用已释放给其他请求的数量。未确认前仅在当次时刻达到到期时刻时
	// （库存查询、请求查询、新预留核算、新使用判断）就本次涉及的承诺确认。
	// 幂等重试、参数非法或引用对象不存在而提前失败的调用不确认到期。
	Expired bool
}

// Unused 返回未用数量。
func (c *Commitment) Unused() int { return c.Quantity - c.Used }

// confirmExpiryLocked 在已持锁的情况下按本次当前时刻确认单笔承诺到期。
// 仅确认本次操作涉及的承诺：未取消、尚未确认且当前时刻已达到到期时刻时
// 置位 Expired；该结果不可逆。已取消的承诺保持 canceled，不确认到期。
func (s *Store) confirmExpiryLocked(c *Commitment, now time.Time) {
	if c.Canceled || c.Expired {
		return
	}
	if !now.Before(c.Expiry) {
		c.Expired = true
	}
}

// confirmPartExpiriesLocked 确认指定备件下全部已到期但尚未确认的承诺。
func (s *Store) confirmPartExpiriesLocked(partID string, now time.Time) {
	for _, c := range s.commitments {
		if c.PartID == partID {
			s.confirmExpiryLocked(c, now)
		}
	}
}

// confirmRequestExpiriesLocked 确认指定请求下全部已到期但尚未确认的承诺。
func (s *Store) confirmRequestExpiriesLocked(requestID string, now time.Time) {
	for _, c := range s.commitments {
		if c.RequestID == requestID {
			s.confirmExpiryLocked(c, now)
		}
	}
}

// Status 返回承诺的当前状态。
// 已确认到期不可逆：即使传入更早的当前时刻也显示 expired；状态不要求
// 调用时刻递增。取消优先于到期，已取消的记录始终显示 canceled。
func (c *Commitment) Status(now time.Time) CommitmentStatus {
	switch {
	case c.Canceled:
		return CommitmentCanceled
	case c.Expired:
		return CommitmentExpired
	case !now.Before(c.Expiry):
		return CommitmentExpired
	default:
		return CommitmentActive
	}
}

// Usage 是一次成功的承诺使用记录。
type Usage struct {
	// ID 是全局唯一的使用编号。
	ID string
	// CommitmentID 是被扣减的承诺编号。
	CommitmentID string
	// Quantity 是本次使用的正整数数量。
	Quantity int
}

// Store 保存全部登记信息与承诺。所有方法均为并发安全。
type Store struct {
	mu          sync.Mutex
	products    map[string]*Product
	parts       map[string]*Part
	requests    map[string]*Request
	commitments map[string]*Commitment
	usages      map[string]*Usage
	// firstResults 保存每个承诺编号首次预留成功时返回的承诺快照（已用数量为零、
	// 未取消）。原样重试取回它，不随分批使用、取消或到期改变。
	firstResults map[string]Commitment
	// history 保存按请求分组的预留处理历史，仅存在于当前仓库实例中。
	history map[string][]HistoryRecord
}

// NewStore 创建一个空的保修备件仓库。
func NewStore() *Store {
	return &Store{
		products:     make(map[string]*Product),
		parts:        make(map[string]*Part),
		requests:     make(map[string]*Request),
		commitments:  make(map[string]*Commitment),
		usages:       make(map[string]*Usage),
		firstResults: make(map[string]Commitment),
		history:      make(map[string][]HistoryRecord),
	}
}

// RegisterProduct 登记产品：购买时刻、保修天数和除外故障代码。
// 编号重复时返回 ErrDuplicateID 且保留原记录。保修天数必须为正整数，且自购买
// 时刻起完整天数（每天二十四小时）的截止时刻必须落在时间类型的可表示范围内，
// 否则返回 ErrInvalidParam，不保存产品记录。
func (s *Store) RegisterProduct(id string, purchaseTime time.Time, warrantyDays int, excludedCodes []string) error {
	if id == "" {
		return fmt.Errorf("%w: product id must not be empty", ErrInvalidParam)
	}
	if warrantyDays <= 0 {
		return fmt.Errorf("%w: warranty days must be a positive integer", ErrInvalidParam)
	}
	// 截止时刻超出时间类型可表示范围的期限无法登记，不保存产品记录；
	// 之后以同一编号提交可表示的条款仍可正常登记。
	if _, ok := warrantyExpiry(purchaseTime, warrantyDays); !ok {
		return fmt.Errorf("%w: warranty expiry for %d days is not representable", ErrInvalidParam, warrantyDays)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.products[id]; ok {
		return fmt.Errorf("%w: product %q", ErrDuplicateID, id)
	}
	codes := make(map[string]struct{}, len(excludedCodes))
	for _, c := range excludedCodes {
		if c == "" {
			return fmt.Errorf("%w: excluded fault code must not be empty", ErrInvalidParam)
		}
		codes[c] = struct{}{}
	}
	s.products[id] = &Product{
		ID:            id,
		PurchaseTime:  purchaseTime,
		WarrantyDays:  warrantyDays,
		ExcludedCodes: codes,
	}
	return nil
}

// RegisterPart 登记备件及其非负整数初始库存。
// 编号重复时返回 ErrDuplicateID 且保留原记录。
func (s *Store) RegisterPart(id string, initialStock int) error {
	if id == "" {
		return fmt.Errorf("%w: part id must not be empty", ErrInvalidParam)
	}
	if initialStock < 0 {
		return fmt.Errorf("%w: initial stock must be a non-negative integer", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.parts[id]; ok {
		return fmt.Errorf("%w: part %q", ErrDuplicateID, id)
	}
	s.parts[id] = &Part{ID: id, Stock: initialStock}
	return nil
}

// SubmitRequest 提交保修请求，指定产品和故障代码。
// 编号重复时返回 ErrDuplicateID 且保留原记录。
func (s *Store) SubmitRequest(id, productID, faultCode string) error {
	if id == "" {
		return fmt.Errorf("%w: request id must not be empty", ErrInvalidParam)
	}
	if faultCode == "" {
		return fmt.Errorf("%w: fault code must not be empty", ErrInvalidParam)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.requests[id]; ok {
		return fmt.Errorf("%w: request %q", ErrDuplicateID, id)
	}
	s.requests[id] = &Request{ID: id, ProductID: productID, FaultCode: faultCode}
	return nil
}

// Product 返回指定编号的产品副本，不存在时返回 ErrNotFound。
func (s *Store) Product(id string) (Product, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.products[id]
	if !ok {
		return Product{}, fmt.Errorf("%w: product %q", ErrNotFound, id)
	}
	cp := *p
	cp.ExcludedCodes = make(map[string]struct{}, len(p.ExcludedCodes))
	for k := range p.ExcludedCodes {
		cp.ExcludedCodes[k] = struct{}{}
	}
	return cp, nil
}

// Part 返回指定编号的备件副本，不存在时返回 ErrNotFound。
func (s *Store) Part(id string) (Part, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.parts[id]
	if !ok {
		return Part{}, fmt.Errorf("%w: part %q", ErrNotFound, id)
	}
	return *p, nil
}

// Request 返回指定编号的请求副本，不存在时返回 ErrNotFound。
func (s *Store) Request(id string) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[id]
	if !ok {
		return Request{}, fmt.Errorf("%w: request %q", ErrNotFound, id)
	}
	return *r, nil
}

// Commitment 返回指定编号的承诺副本，不存在时返回 ErrNotFound。
func (s *Store) Commitment(id string) (Commitment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.commitments[id]
	if !ok {
		return Commitment{}, fmt.Errorf("%w: commitment %q", ErrNotFound, id)
	}
	return *c, nil
}
