package warranty

import (
	"errors"
	"testing"
	"time"
)

// 备件重复登记的回归保障：现有检查只覆盖刚登记的备件原样重报，本文件补上备件
// 已被合格请求承诺、且承诺已分批使用之后的情形。
//
// 主例数据（与用户描述一致）：备件 part-dup 最初登记十二件，两个在保请求分别
// 取得七件（c-dup-seven，属于 r-dup-seven）和三件（c-dup-three，属于
// r-dup-three）的有效承诺，再从七件承诺中使用两件。此后实物剩余十件、有效占用
// 八件（5+3）、可承诺两件。全部操作时刻早于两笔承诺到期时刻，库存差异只来自
// 真实使用，不是到期释放。
//
// 以原编号再次提交明显不同的初始库存（与原库存相同、更大、更小的非负库存、零、
// 负数）时，编号重复必须优先于本次库存校验：一律 ErrDuplicateID——负数在首次
// 登记时虽属非法，但编号已存在时只能报重复，不能改报 ErrInvalidParam。失败提交
// 既不是补货也不是清零：直接查询备件与查询库存占用都继续反映原来的库存事实，
// 两笔承诺仍属于原请求，原定数量、已用数量、未用数量和到期时刻均不变。
//
// 重复登记被拒绝后，新来的合格请求仍按原来的两件可承诺数量处理：申请三件返回
// ErrInsufficientStock，不生成承诺、不增加占用；申请两件可以成功，成功后实物
// 剩余仍是十件、有效占用十件、可承诺数量为零。
//
// 另外区分首次登记失败与重复登记：未登记的非空编号提交负库存返回
// ErrInvalidParam 且查询仍报不存在；随后用同一编号提交零库存可以正常登记，
// 实物剩余、有效占用和可承诺数量均为零。

const (
	dupPartID    = "part-dup"
	dupProductID = "p-dup"
	dupSevenReq  = "r-dup-seven"
	dupThreeReq  = "r-dup-three"
	dupNewReq    = "r-dup-new"
	dupSevenCmt  = "c-dup-seven"
	dupThreeCmt  = "c-dup-three"
	dupStock     = 12
)

// dupPartNewStore 构造主例状态：十二件初始库存、两笔合格请求分别预留七件和
// 三件，七件承诺已使用两件。产品保修期足够长，承诺到期时刻为 expiryOK，
// 本文件全部操作时刻 nowOK 仍在保且承诺均未到期。
func dupPartNewStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct(dupProductID, t0, 120, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart(dupPartID, dupStock); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest(dupSevenReq, dupProductID, "NOISE"); err != nil {
		t.Fatalf("submit seven request: %v", err)
	}
	if err := s.SubmitRequest(dupThreeReq, dupProductID, "NOISE"); err != nil {
		t.Fatalf("submit three request: %v", err)
	}
	if _, err := s.Reserve(dupSevenCmt, dupSevenReq, dupPartID, 7, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve seven: %v", err)
	}
	if _, err := s.Reserve(dupThreeCmt, dupThreeReq, dupPartID, 3, expiryOK, nowOK); err != nil {
		t.Fatalf("reserve three: %v", err)
	}
	if _, err := s.Use("u-dup-two", dupSevenCmt, 2, nowOK); err != nil {
		t.Fatalf("use two from seven commitment: %v", err)
	}
	return s
}

// assertDupPartStock 核对直接查询备件得到的实物库存与按备件查询的占用账目。
func assertDupPartStock(t *testing.T, s *Store, physical, occupied, committable int) {
	t.Helper()
	if p, err := s.Part(dupPartID); err != nil || p.Stock != physical {
		t.Fatalf("part stock = %d (err %v), want %d", p.Stock, err, physical)
	}
	st, err := s.PartStatus(dupPartID, nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != physical || st.ActiveOccupied != occupied || st.Committable != committable {
		t.Fatalf("stock account phys=%d occupied=%d committable=%d, want phys=%d occupied=%d committable=%d",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable, physical, occupied, committable)
	}
}

// assertDupCommitmentUnchanged 直接核对一笔承诺的归属、原定数量、已用数量、
// 未用数量、到期时刻与活动状态全部保持原值。
func assertDupCommitmentUnchanged(t *testing.T, s *Store, commitID, requestID string, quantity, used int, expiry time.Time) {
	t.Helper()
	c, err := s.Commitment(commitID)
	if err != nil {
		t.Fatalf("commitment %q: %v", commitID, err)
	}
	if c.RequestID != requestID || c.PartID != dupPartID || c.Quantity != quantity ||
		c.Used != used || c.Unused() != quantity-used || !c.Expiry.Equal(expiry) ||
		c.Canceled || c.Expired {
		t.Fatalf("commitment %q changed: %+v, want req=%s part=%s qty=%d used=%d unused=%d expiry=%v active",
			commitID, c, requestID, dupPartID, quantity, used, quantity-used, expiry)
	}
}

// assertDupRequestCommitments 按请求查看承诺：每个原请求只能看到属于自己的
// 那一笔，且原定数量、已用数量、未用数量、到期时刻与状态保持原值。
func assertDupRequestCommitments(t *testing.T, s *Store) {
	t.Helper()
	want := map[string]struct {
		commitID         string
		quantity, unused int
	}{
		dupSevenReq: {dupSevenCmt, 7, 5},
		dupThreeReq: {dupThreeCmt, 3, 3},
	}
	for reqID, w := range want {
		view, err := s.RequestView(reqID, nowOK)
		if err != nil {
			t.Fatalf("request view %q: %v", reqID, err)
		}
		if !view.Eligibility.Eligible {
			t.Fatalf("request %q no longer eligible: %+v", reqID, view.Eligibility)
		}
		if len(view.Commitments) != 1 {
			t.Fatalf("request %q commitments = %+v, want exactly its own one", reqID, view.Commitments)
		}
		d := view.Commitments[0]
		if d.CommitmentID != w.commitID || d.RequestID != reqID || d.PartID != dupPartID ||
			d.OriginalQuantity != w.quantity || d.RemainingQuantity != w.unused ||
			d.UsedQuantity != w.quantity-w.unused || d.Status != CommitmentActive ||
			!d.Expiry.Equal(expiryOK) {
			t.Fatalf("request %q detail = %+v, want commitment %s qty=%d used=%d unused=%d active expiry=%v",
				reqID, d, w.commitID, w.quantity, w.quantity-w.unused, w.unused, expiryOK)
		}
	}
}

// assertDupPartDetails 按备件查看占用明细：恰好两笔承诺，按承诺编号排序，
// 归属、原定数量、已用数量、未用数量、到期时刻与活动状态全部保留。
func assertDupPartDetails(t *testing.T, s *Store) {
	t.Helper()
	st, err := s.PartStatus(dupPartID, nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if len(st.Details) != 2 {
		t.Fatalf("details = %+v, want exactly the two original commitments", st.Details)
	}
	d7, d3 := st.Details[0], st.Details[1]
	if d7.CommitmentID != dupSevenCmt || d3.CommitmentID != dupThreeCmt {
		t.Fatalf("details order = %q, %q, want sorted %q, %q",
			d7.CommitmentID, d3.CommitmentID, dupSevenCmt, dupThreeCmt)
	}
	if d7.RequestID != dupSevenReq || d7.OriginalQuantity != 7 || d7.UsedQuantity != 2 ||
		d7.RemainingQuantity != 5 || d7.Status != CommitmentActive || !d7.Expiry.Equal(expiryOK) {
		t.Fatalf("seven detail changed: %+v", d7)
	}
	if d3.RequestID != dupThreeReq || d3.OriginalQuantity != 3 || d3.UsedQuantity != 0 ||
		d3.RemainingQuantity != 3 || d3.Status != CommitmentActive || !d3.Expiry.Equal(expiryOK) {
		t.Fatalf("three detail changed: %+v", d3)
	}
}

// assertDupFactsPreserved 在一次被拒绝的重复登记之后，全面核对主例的库存事实
// 与两笔承诺事实没有任何变化。
func assertDupFactsPreserved(t *testing.T, s *Store) {
	t.Helper()
	// 实物剩余十件、有效占用八件、可承诺两件。
	assertDupPartStock(t, s, 10, 8, 2)
	// 直接查询两笔承诺：七件已用两件剩五件，三件未动用。
	assertDupCommitmentUnchanged(t, s, dupSevenCmt, dupSevenReq, 7, 2, expiryOK)
	assertDupCommitmentUnchanged(t, s, dupThreeCmt, dupThreeReq, 3, 0, expiryOK)
	// 按请求查看承诺与按备件查看占用，保留同样的事实。
	assertDupRequestCommitments(t, s)
	assertDupPartDetails(t, s)
}

// TestDuplicatePartAfterCommitmentsAndUses 覆盖已被承诺和使用的备件重复登记：
// 相同库存、更大或更小的非负库存、零、负数全部报 ErrDuplicateID（负数不得改
// 报 ErrInvalidParam），失败后库存与承诺事实原样保留，不补货、不清零。
func TestDuplicatePartAfterCommitmentsAndUses(t *testing.T) {
	cases := []struct {
		name  string
		stock int
	}{
		{"same stock", dupStock},        // 与原初始库存相同的十二件
		{"much larger stock", 100},      // 明显更大，不得当作补货
		{"smaller non-negative stock", 4}, // 明显更小的非负库存，不得覆盖或清零
		{"zero stock", 0},               // 零同样报重复而不是登记一个零库存备件
		{"negative stock", -1},          // 编号已存在时负数报重复，不能改报参数非法
		{"large negative stock", -100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := dupPartNewStore(t)

			err := s.RegisterPart(dupPartID, tc.stock)
			if !errors.Is(err, ErrDuplicateID) {
				t.Fatalf("re-register with stock %d: got %v, want ErrDuplicateID", tc.stock, err)
			}
			if errors.Is(err, ErrInvalidParam) {
				t.Fatalf("duplicate id must win over stock validation, got %v", err)
			}

			// 失败后直接查询备件、查询库存占用、按请求与按备件查看承诺，
			// 全部继续反映原来的库存事实。
			assertDupFactsPreserved(t, s)

			// 重复登记失败不占用编号之外的任何状态：再用同样的非法内容提交，
			// 结论仍是重复，事实仍不变。
			if err := s.RegisterPart(dupPartID, tc.stock); !errors.Is(err, ErrDuplicateID) {
				t.Fatalf("repeat rejected duplicate with stock %d: got %v, want ErrDuplicateID", tc.stock, err)
			}
			assertDupFactsPreserved(t, s)
		})
	}
}

// TestReserveAfterDuplicatePartRejectionUsesOriginalStock 覆盖重复登记被拒绝后
// 的新预留：仍按两件可承诺数量核算。申请三件库存不足、不留承诺不增占用；申请
// 两件成功，成功后实物剩余十件、有效占用十件、可承诺为零。失败提交中的初始
// 库存既没有被当成补货，也没有把库存清零。
func TestReserveAfterDuplicatePartRejectionUsesOriginalStock(t *testing.T) {
	s := dupPartNewStore(t)

	// 连续用差异极大的初始库存重复登记：补货量级（100）、清零（0）、负数。
	for _, stock := range []int{100, 0, -1} {
		if err := s.RegisterPart(dupPartID, stock); !errors.Is(err, ErrDuplicateID) {
			t.Fatalf("re-register with stock %d: got %v, want ErrDuplicateID", stock, err)
		}
	}
	assertDupFactsPreserved(t, s)

	// 新来的合格请求。
	if err := s.SubmitRequest(dupNewReq, dupProductID, "NOISE"); err != nil {
		t.Fatalf("submit new request: %v", err)
	}

	// 可承诺只有两件：申请三件返回 ErrInsufficientStock。
	if _, err := s.Reserve("c-dup-new-three", dupNewReq, dupPartID, 3, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve 3 against committable 2: got %v, want ErrInsufficientStock", err)
	}
	// 失败不生成承诺。
	if _, err := s.Commitment("c-dup-new-three"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected reserve must not create a commitment, got %v", err)
	}
	// 失败不增加占用，库存账目仍是 10/8/2。
	assertDupFactsPreserved(t, s)

	// 申请两件可以成功。
	c, err := s.Reserve("c-dup-new-two", dupNewReq, dupPartID, 2, expiryOK, nowOK)
	if err != nil {
		t.Fatalf("reserve exact committable 2: %v", err)
	}
	if c.Quantity != 2 || c.Used != 0 || c.Unused() != 2 || c.RequestID != dupNewReq ||
		c.Canceled || c.Expired || !c.Expiry.Equal(expiryOK) {
		t.Fatalf("new commitment = %+v, want qty=2 unused=2 for %s active", c, dupNewReq)
	}

	// 成功后实物剩余仍是十件（预留不扣实物），有效占用变为十件，可承诺为零。
	assertDupPartStock(t, s, 10, 10, 0)
	// 新请求只看到自己的两笔尝试中的成功那一笔。
	view, err := s.RequestView(dupNewReq, nowOK)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	if len(view.Commitments) != 1 || view.Commitments[0].CommitmentID != "c-dup-new-two" ||
		view.Commitments[0].OriginalQuantity != 2 || view.Commitments[0].RemainingQuantity != 2 ||
		view.Commitments[0].Status != CommitmentActive {
		t.Fatalf("new request view = %+v, want only the successful 2-piece commitment", view.Commitments)
	}
	// 原来的两笔承诺仍完整保留，且仍属于原请求。
	assertDupCommitmentUnchanged(t, s, dupSevenCmt, dupSevenReq, 7, 2, expiryOK)
	assertDupCommitmentUnchanged(t, s, dupThreeCmt, dupThreeReq, 3, 0, expiryOK)

	// 可承诺已经为零：再小的申请也无法成功，不能把失败提交当初始库存补货。
	if _, err := s.Reserve("c-dup-new-one", dupNewReq, dupPartID, 1, expiryOK, nowOK); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve after committable exhausted: got %v, want ErrInsufficientStock", err)
	}
	if _, err := s.Commitment("c-dup-new-one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected reserve must not create a commitment, got %v", err)
	}
	assertDupPartStock(t, s, 10, 10, 0)
}

// TestFirstPartRegistrationFailureIsNotDuplicate 区分首次登记失败与重复登记：
// 未登记过的非空编号提交负库存返回 ErrInvalidParam，查询该编号仍报不存在；
// 随后用同一编号提交零库存正常登记，实物剩余、有效占用和可承诺数量均为零；
// 只有登记成功之后再提交才报重复。
func TestFirstPartRegistrationFailureIsNotDuplicate(t *testing.T) {
	const freshID = "part-dup-fresh"
	s := NewStore()

	for _, stock := range []int{-1, -100} {
		err := s.RegisterPart(freshID, stock)
		if !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("first registration with stock %d: got %v, want ErrInvalidParam", stock, err)
		}
		if errors.Is(err, ErrDuplicateID) {
			t.Fatalf("unregistered id with stock %d must not report duplicate, got %v", stock, err)
		}
		// 失败不保存备件记录、不占用编号：直接查询与库存查询都报不存在。
		if _, err := s.Part(freshID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("part %q after failed first registration: got %v, want ErrNotFound", freshID, err)
		}
		if _, err := s.PartStatus(freshID, nowOK); !errors.Is(err, ErrNotFound) {
			t.Fatalf("part status %q after failed first registration: got %v, want ErrNotFound", freshID, err)
		}
	}

	// 同一编号随后以零库存正常登记。
	if err := s.RegisterPart(freshID, 0); err != nil {
		t.Fatalf("register with zero stock after failed attempts: %v", err)
	}
	if p, err := s.Part(freshID); err != nil || p.Stock != 0 {
		t.Fatalf("part = %+v (err %v), want stock 0", p, err)
	}
	st, err := s.PartStatus(freshID, nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if st.PhysicalRemaining != 0 || st.ActiveOccupied != 0 || st.Committable != 0 {
		t.Fatalf("zero-stock part account = %+v, want 0/0/0", st)
	}
	if len(st.Details) != 0 {
		t.Fatalf("fresh part details = %+v, want none", st.Details)
	}

	// 登记成功之后再提交（无论库存是否合法）才报重复，零库存记录保留。
	for _, stock := range []int{0, 12, -1} {
		if err := s.RegisterPart(freshID, stock); !errors.Is(err, ErrDuplicateID) {
			t.Fatalf("duplicate after successful registration with stock %d: got %v, want ErrDuplicateID", stock, err)
		}
		if p, err := s.Part(freshID); err != nil || p.Stock != 0 {
			t.Fatalf("stock %d duplicate changed zero-stock record: %+v err %v", stock, p, err)
		}
	}
}
