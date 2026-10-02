// Package warranty 提供可由本机程序调用的保修资格判断与备件承诺能力。
//
// 所有时刻均由调用方提供，Service 本身不读取系统时钟，因此结果完全可复现、可测试。
// 一个 Service 实例即为一个独立的登记与承诺现场，其方法可被多个 goroutine 并发调用。
package warranty

import (
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// 哨兵错误，可用 errors.Is 判断。
var (
	// ErrConflict 表示幂等键（预留提交编号或使用编号）曾用于另一组不同内容。
	ErrConflict = errors.New("warranty: idempotency conflict")
	// ErrDuplicate 表示产品、备件或保修请求编号重复登记。
	ErrDuplicate = errors.New("warranty: duplicate identifier")
	// ErrInvalidArgument 表示传入了非法参数（空编号、非正天数/数量、
	// 购买时刻晚于当前时刻、预留到期时刻不晚于当前时刻等）。
	ErrInvalidArgument = errors.New("warranty: invalid argument")
	// ErrUnknown 表示引用了未登记的产品、备件、请求或承诺。
	ErrUnknown = errors.New("warranty: unknown identifier")
	// ErrNotEligible 表示请求在当前时刻不合格（过保/除外）。
	ErrNotEligible = errors.New("warranty: request not eligible")
	// ErrInsufficient 表示可承诺数量不足。
	ErrInsufficient = errors.New("warranty: insufficient stock")
	// ErrCommitmentInactive 表示承诺已失效（取消或到期），不能继续使用。
	ErrCommitmentInactive = errors.New("warranty: commitment inactive")
)

// Reason 标记资格被拒的一个原因。
type Reason string

const (
	// ReasonExpired 表示当前时刻已到或超过保修截止时刻（购买时刻 + 天数×24h）。
	ReasonExpired Reason = "expired"
	// ReasonExcluded 表示故障代码落在产品登记的除外故障代码集合内。
	ReasonExcluded Reason = "excluded"
)

// 承诺状态常量。
const (
	StatusActive   = "active"   // 有效：未取消、未到期且尚有剩余
	StatusUsedUp   = "used_up"  // 有效但已全部使用，不再接受新使用
	StatusCanceled = "canceled" // 已取消
	StatusExpired  = "expired"  // 已到期（当前时刻 >= 到期时刻）且此前未取消
)

// Product 是产品登记记录。
type Product struct {
	ID            string
	PurchasedAt   time.Time
	WarrantyDays  int
	ExcludedCodes map[string]struct{}
}

// Part 是备件登记记录。
type Part struct {
	ID           string
	InitialStock int
}

// Request 是保修请求登记记录。
type Request struct {
	ID        string
	ProductID string
	FaultCode string
}

// CommitmentView 是返回给调用方的承诺信息（快照）。
type CommitmentView struct {
	CommitmentID string
	RequestID    string
	PartID       string
	Quantity     int // 原定数量
	Used         int // 已用数量
	ExpiresAt    time.Time
	Status       string
}

// Remaining 返回当前快照下的剩余数量；已取消或到期的承诺剩余为 0。
func (c CommitmentView) Remaining() int {
	if c.Status == StatusCanceled || c.Status == StatusExpired {
		return 0
	}
	return c.Quantity - c.Used
}

// EligibilityView 是按请求查询资格所返回的信息。
type EligibilityView struct {
	RequestID string
	// Eligible 为 true 当且仅当当前时刻在保修期内且故障代码未被除外。
	Eligible bool
	// Reasons 为拒绝原因列表；合格时为空。同时过保与除外时按固定顺序列出两项。
	Reasons []Reason
	// Commitments 为该请求关联的所有承诺（含已取消/到期），按承诺编号字典序。
	Commitments []CommitmentView
}

// OccupancyItem 是按备件查询时单条占用明细。
type OccupancyItem struct {
	CommitmentID string
	RequestID    string
	PartID       string
	Quantity     int // 原定数量
	Used         int // 已用数量
	Remaining    int // 剩余数量（当前时刻下，失效为 0）
	ExpiresAt    time.Time
	Status       string
}

// PartView 是按备件查询所返回的快照。
type PartView struct {
	PartID            string
	PhysicalRemaining int // 实物剩余 = 初始库存 - 所有成功使用总量
	ActiveHeld        int // 有效占用 = 当前有效承诺的未用总量
	Available         int // 可承诺数量 = 实物剩余 - 有效占用
	Items             []OccupancyItem
}

// ---------- 内部模型 ----------

type product struct {
	id            string
	purchasedAt   time.Time
	warrantyDays  int
	excludedCodes map[string]struct{}
}

type part struct {
	id           string
	initialStock int
}

type request struct {
	id        string
	productID string
	faultCode string
}

type commitment struct {
	id        string
	requestID string
	partID    string
	quantity  int
	used      int
	expiresAt time.Time
	canceled  bool
}

// effectiveStatus 在给定当前时刻推导承诺状态，不修改任何数据。
func (c *commitment) effectiveStatus(now time.Time) string {
	if c.canceled {
		return StatusCanceled
	}
	if !now.Before(c.expiresAt) { // now >= expiresAt
		return StatusExpired
	}
	if c.used >= c.quantity {
		return StatusUsedUp
	}
	return StatusActive
}

// effectiveRemaining 返回在当前时刻仍被该承诺占用的未用量（失效为 0）。
func (c *commitment) effectiveRemaining(now time.Time) int {
	if c.canceled || !now.Before(c.expiresAt) {
		return 0
	}
	return c.quantity - c.used
}

// reservationKey 描述一次预留的内容，用于幂等与冲突检测。
type reservationKey struct {
	requestID string
	partID    string
	qty       int
	expiresAt time.Time
}

// usageKey 描述一次使用的内容。
type usageKey struct {
	commitmentID string
	qty          int
}

// Service 是登记、资格判断与备件承诺的入口。
// 一个实例就是一个独立现场；所有方法通过单一互斥锁串行化，
// 从而在并发预留同一备件时也保证有效承诺未用总量不超过剩余实物库存。
type Service struct {
	mu sync.Mutex

	products map[string]*product
	parts    map[string]*part
	requests map[string]*request

	commitments        map[string]*commitment
	requestCommitments map[string][]string // 请求 -> 承诺 ID（含失效记录）
	partCommitments    map[string][]string // 备件 -> 承诺 ID（含失效记录）
	commitmentSeq      int

	// 幂等表：提交编号 / 使用编号全局唯一，一旦使用不可更改。
	reservationsBySubmitID map[string]reservationKey
	submitCommitment       map[string]string // 提交编号 -> 承诺 ID
	usagesByID             map[string]usageKey
}

// NewService 创建一个空现场。
func NewService() *Service {
	return &Service{
		products:               map[string]*product{},
		parts:                  map[string]*part{},
		requests:               map[string]*request{},
		commitments:            map[string]*commitment{},
		requestCommitments:     map[string][]string{},
		partCommitments:        map[string][]string{},
		reservationsBySubmitID: map[string]reservationKey{},
		submitCommitment:       map[string]string{},
		usagesByID:             map[string]usageKey{},
	}
}

// ---------- 登记 ----------

// RegisterProduct 登记产品。购买时刻不得晚于 now；保修天数必须为正整数。
// 产品编号重复时返回 ErrDuplicate 并保留原记录。
func (s *Service) RegisterProduct(id string, purchasedAt time.Time, warrantyDays int, excludedCodes []string, now time.Time) error {
	if id == "" || warrantyDays <= 0 || purchasedAt.After(now) {
		return ErrInvalidArgument
	}
	excl := make(map[string]struct{}, len(excludedCodes))
	for _, c := range excludedCodes {
		if c == "" {
			return ErrInvalidArgument
		}
		excl[c] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.products[id]; ok {
		return ErrDuplicate
	}
	s.products[id] = &product{
		id:            id,
		purchasedAt:   purchasedAt,
		warrantyDays:  warrantyDays,
		excludedCodes: excl,
	}
	return nil
}

// RegisterPart 按唯一备件编号登记非负整数初始库存。编号重复时报错并保留原记录。
func (s *Service) RegisterPart(id string, initialStock int) error {
	if id == "" || initialStock < 0 {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.parts[id]; ok {
		return ErrDuplicate
	}
	s.parts[id] = &part{id: id, initialStock: initialStock}
	return nil
}

// RegisterRequest 提交带唯一编号的保修请求，引用已登记产品并给出非空故障代码。
// 编号重复返回 ErrDuplicate；未知产品返回 ErrUnknown；缺少故障代码返回
// ErrInvalidArgument。失败时不创建任何记录。
func (s *Service) RegisterRequest(id, productID, faultCode string) error {
	if id == "" || productID == "" || faultCode == "" {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.requests[id]; ok {
		return ErrDuplicate
	}
	if _, ok := s.products[productID]; !ok {
		return ErrUnknown
	}
	s.requests[id] = &request{id: id, productID: productID, faultCode: faultCode}
	return nil
}

// ---------- 资格 ----------

// warrantyEnd 返回保修截止时刻 = 购买时刻 + warrantyDays×24h。
func warrantyEnd(p *product) time.Time {
	return p.purchasedAt.Add(time.Duration(p.warrantyDays) * 24 * time.Hour)
}

// evaluate 必须在持锁状态下调用。返回是否合格与拒绝原因（固定顺序：先 expired 后 excluded）。
func (s *Service) evaluate(r *request, now time.Time) (bool, []Reason) {
	p := s.products[r.productID]
	var reasons []Reason
	if !now.Before(warrantyEnd(p)) { // now >= end：截止时刻开始算过保
		reasons = append(reasons, ReasonExpired)
	}
	if _, ok := p.excludedCodes[r.faultCode]; ok {
		reasons = append(reasons, ReasonExcluded)
	}
	return len(reasons) == 0, reasons
}

// Eligible 判断一个请求在 now 时刻是否合格并返回拒绝原因。未知请求返回 ErrUnknown。
func (s *Service) Eligible(requestID string, now time.Time) (bool, []Reason, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[requestID]
	if !ok {
		return false, nil, ErrUnknown
	}
	eligible, reasons := s.evaluate(r, now)
	return eligible, reasons, nil
}

// ---------- 预留 ----------

// Reserve 为某次保修请求预留一种备件。每次预留使用全局唯一的提交编号 submitID：
//   - 成功后，用相同 submitID 与相同内容（请求/备件/数量/到期时刻）重试，返回同一承诺；
//   - 用相同 submitID 但更换请求、备件、数量或到期时刻，返回 ErrConflict。
//
// 预留时按当次时刻 now 重新判断资格。未知请求或备件返回 ErrUnknown；
// 不合格返回可解出原因的 ErrNotEligible；数量非正、到期时刻不晚于 now 返回
// ErrInvalidArgument；可承诺数量不足返回 ErrInsufficient。任何失败都不占用数量。
func (s *Service) Reserve(submitID, requestID, partID string, quantity int, expiresAt, now time.Time) (string, error) {
	if submitID == "" || requestID == "" || partID == "" || quantity <= 0 {
		return "", ErrInvalidArgument
	}
	if !expiresAt.After(now) { // 到期必须严格晚于当前时刻
		return "", ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	want := reservationKey{requestID: requestID, partID: partID, qty: quantity, expiresAt: expiresAt}
	if prev, seen := s.reservationsBySubmitID[submitID]; seen {
		if prev != want {
			return "", ErrConflict
		}
		return s.submitCommitment[submitID], nil // 相同内容重试：返回同一承诺
	}

	r, ok := s.requests[requestID]
	if !ok {
		return "", ErrUnknown
	}
	if _, ok := s.parts[partID]; !ok {
		return "", ErrUnknown
	}

	if eligible, reasons := s.evaluate(r, now); !eligible {
		return "", &eligibilityError{reasons: reasons}
	}

	available := s.physicalRemaining(partID) - s.activeHeld(partID, now)
	if quantity > available {
		return "", &insufficientError{available: available}
	}

	cid := s.nextCommitmentID()
	s.commitments[cid] = &commitment{
		id:        cid,
		requestID: requestID,
		partID:    partID,
		quantity:  quantity,
		expiresAt: expiresAt,
	}
	s.requestCommitments[requestID] = append(s.requestCommitments[requestID], cid)
	s.partCommitments[partID] = append(s.partCommitments[partID], cid)
	s.reservationsBySubmitID[submitID] = want
	s.submitCommitment[submitID] = cid
	return cid, nil
}

// ---------- 使用 ----------

// Use 对某条承诺分批使用，带全局唯一的使用编号 usageID 及正整数数量：
//   - 成功时同时扣减该承诺未用数量与剩余实物库存；
//   - 数量超过当前未用数量时整次失败，不做任何扣减；
//   - 同一 usageID 与相同内容（承诺 + 数量）再次提交返回首次成功结果，不再扣减；
//   - 同一 usageID 改数量或改承诺返回 ErrConflict；
//   - 已成功的使用在承诺取消或到期后重试仍返回原结果；
//   - 失效（取消/到期）或已全部使用的承诺不再接受新使用。
func (s *Service) Use(usageID, commitmentID string, quantity int, now time.Time) error {
	if usageID == "" || commitmentID == "" || quantity <= 0 {
		return ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	want := usageKey{commitmentID: commitmentID, qty: quantity}
	if prev, seen := s.usagesByID[usageID]; seen {
		if prev != want {
			return ErrConflict
		}
		return nil // 相同使用重试：无论承诺此后状态如何，返回首次成功结果且不再扣减
	}

	c, ok := s.commitments[commitmentID]
	if !ok {
		return ErrUnknown
	}
	switch st := c.effectiveStatus(now); st {
	case StatusCanceled, StatusExpired:
		return &inactiveError{status: st}
	case StatusUsedUp:
		return ErrInvalidArgument
	}
	if quantity > c.quantity-c.used {
		return ErrInvalidArgument
	}
	if quantity > s.physicalRemaining(c.partID) {
		return &insufficientError{available: s.physicalRemaining(c.partID)}
	}

	c.used += quantity
	s.usagesByID[usageID] = want
	return nil
}

// ---------- 取消 ----------

// Cancel 取消一条承诺，只释放其当前未用且仍有效占用的数量。
// 取消是幂等的：重复取消不报错、不重复释放库存；已到期承诺本就不占库存，
// 取消仅标记状态。已使用的数量不回补。
func (s *Service) Cancel(commitmentID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.commitments[commitmentID]; !ok {
		return ErrUnknown
	}
	s.commitments[commitmentID].canceled = true
	return nil
}

// ---------- 查询：按请求 ----------

// QueryRequest 按请求查询资格依据、拒绝原因与关联承诺。
// 资格按 now 重新评估；关联承诺含已取消/到期记录，按承诺编号字典序排列。
func (s *Service) QueryRequest(requestID string, now time.Time) (EligibilityView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[requestID]
	if !ok {
		return EligibilityView{}, ErrUnknown
	}
	eligible, reasons := s.evaluate(r, now)
	ids := sortedCopy(s.requestCommitments[requestID])
	views := make([]CommitmentView, 0, len(ids))
	for _, cid := range ids {
		views = append(views, s.viewOf(s.commitments[cid], now))
	}
	return EligibilityView{
		RequestID:   requestID,
		Eligible:    eligible,
		Reasons:     reasons,
		Commitments: views,
	}, nil
}

// ---------- 查询：按备件 ----------

// QueryPart 按备件查询实物剩余、有效占用、可承诺数量与占用明细。
// 明细含请求、承诺、原定数量、已用数量、剩余数量、到期时刻与当前状态；
// 已取消或到期的记录同样列出以便追查，按承诺编号字典序排列。
func (s *Service) QueryPart(partID string, now time.Time) (PartView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.parts[partID]; !ok {
		return PartView{}, ErrUnknown
	}
	physical := s.physicalRemaining(partID)
	held := s.activeHeld(partID, now)

	ids := sortedCopy(s.partCommitments[partID])
	items := make([]OccupancyItem, 0, len(ids))
	for _, cid := range ids {
		c := s.commitments[cid]
		items = append(items, OccupancyItem{
			CommitmentID: c.id,
			RequestID:    c.requestID,
			PartID:       c.partID,
			Quantity:     c.quantity,
			Used:         c.used,
			Remaining:    c.effectiveRemaining(now),
			ExpiresAt:    c.expiresAt,
			Status:       c.effectiveStatus(now),
		})
	}
	return PartView{
		PartID:            partID,
		PhysicalRemaining: physical,
		ActiveHeld:        held,
		Available:         physical - held,
		Items:             items,
	}, nil
}

// ---------- 持锁辅助 ----------

// physicalRemaining = 初始库存 - 该备件所有成功使用总量（与承诺是否有效无关）。
func (s *Service) physicalRemaining(partID string) int {
	usedTotal := 0
	for _, cid := range s.partCommitments[partID] {
		usedTotal += s.commitments[cid].used
	}
	return s.parts[partID].initialStock - usedTotal
}

// activeHeld = 当前有效（未取消、未到期）承诺的未用总量。
func (s *Service) activeHeld(partID string, now time.Time) int {
	held := 0
	for _, cid := range s.partCommitments[partID] {
		held += s.commitments[cid].effectiveRemaining(now)
	}
	return held
}

func (s *Service) viewOf(c *commitment, now time.Time) CommitmentView {
	return CommitmentView{
		CommitmentID: c.id,
		RequestID:    c.requestID,
		PartID:       c.partID,
		Quantity:     c.quantity,
		Used:         c.used,
		ExpiresAt:    c.expiresAt,
		Status:       c.effectiveStatus(now),
	}
}

// nextCommitmentID 在持锁下生成唯一承诺编号。
func (s *Service) nextCommitmentID() string {
	s.commitmentSeq++
	return "C-" + strconv.Itoa(s.commitmentSeq)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ---------- 可解出附加信息的错误类型 ----------

type eligibilityError struct {
	reasons []Reason
}

func (e *eligibilityError) Error() string { return ErrNotEligible.Error() }
func (e *eligibilityError) Unwrap() error { return ErrNotEligible }

// EligibilityErrorReasons 从 Reserve 返回的不合格错误中取出拒绝原因。
func EligibilityErrorReasons(err error) ([]Reason, bool) {
	var e *eligibilityError
	if errors.As(err, &e) {
		return append([]Reason(nil), e.reasons...), true
	}
	return nil, false
}

type insufficientError struct {
	available int
}

func (e *insufficientError) Error() string { return ErrInsufficient.Error() }
func (e *insufficientError) Unwrap() error { return ErrInsufficient }

// AvailableFromError 从库存不足错误中取出当前可承诺数量。
func AvailableFromError(err error) (int, bool) {
	var e *insufficientError
	if errors.As(err, &e) {
		return e.available, true
	}
	return 0, false
}

type inactiveError struct {
	status string
}

func (e *inactiveError) Error() string { return ErrCommitmentInactive.Error() + ": " + e.status }
func (e *inactiveError) Unwrap() error { return ErrCommitmentInactive }

// InactiveStatusFromError 从失效错误中取出状态（canceled/expired）。
func InactiveStatusFromError(err error) (string, bool) {
	var e *inactiveError
	if errors.As(err, &e) {
		return e.status, true
	}
	return "", false
}
