package warranty

import (
	"errors"
	"testing"
	"time"
)

// expStore 构造一个带产品（保修 30 天）、指定库存备件和两个合格请求的仓库。
func expStore(t *testing.T, stock int) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 30, []string{"FAULTX"}); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", stock); err != nil {
		t.Fatalf("register part: %v", err)
	}
	if err := s.SubmitRequest("r1", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r1: %v", err)
	}
	if err := s.SubmitRequest("r2", "p1", "FAULTY"); err != nil {
		t.Fatalf("submit r2: %v", err)
	}
	return s
}

func detailByID(st *PartStatus, id string) (CommitmentDetail, bool) {
	for _, d := range st.Details {
		if d.CommitmentID == id {
			return d, true
		}
	}
	return CommitmentDetail{}, false
}

// TestExpiryReleaseSurvivesClockRollback 对应用户给出的主例：实物库存五件，
// 甲预留五件并于十二点到期；十二点零一分乙又预留同一备件五件；此后用
// 十一点五十九分查询或使用，甲不得重新占用或继续扣减。
func TestExpiryReleaseSurvivesClockRollback(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)

	// 甲预留五件，十二点到期。
	first, err := s.Reserve("jia", "r1", "part1", 5, noon, before)
	if err != nil {
		t.Fatalf("reserve jia: %v", err)
	}
	// 十二点零一分为合格请求创建乙：甲到期释放，乙能预留同一批五件。
	if _, err := s.Reserve("yi", "r2", "part1", 5, expiryOK, after); err != nil {
		t.Fatalf("reserve yi after jia expiry: %v", err)
	}

	// 用十一点五十九分查询：甲已永久到期，只看到乙占用五件，可承诺为零。
	st, err := s.PartStatus("part1", before)
	if err != nil {
		t.Fatalf("part status at earlier time: %v", err)
	}
	if st.PhysicalRemaining != 5 || st.ActiveOccupied != 5 || st.Committable != 0 {
		t.Fatalf("rollback stock: phys=%d occupied=%d committable=%d, want 5/5/0",
			st.PhysicalRemaining, st.ActiveOccupied, st.Committable)
	}
	dj, ok := detailByID(st, "jia")
	if !ok || dj.Status != CommitmentExpired || dj.RemainingQuantity != 5 {
		t.Fatalf("jia at rollback: %+v ok=%v, want expired/remaining=5", dj, ok)
	}
	dy, _ := detailByID(st, "yi")
	if dy.Status != CommitmentActive || dy.RemainingQuantity != 5 {
		t.Fatalf("yi at rollback: %+v, want active/remaining=5", dy)
	}

	// 请求视图在较早时刻也保持甲到期。
	view, err := s.RequestView("r1", before)
	if err != nil {
		t.Fatalf("request view: %v", err)
	}
	var foundJia bool
	for _, d := range view.Commitments {
		if d.CommitmentID == "jia" {
			foundJia = true
			if d.Status != CommitmentExpired || d.OriginalQuantity != 5 ||
				d.UsedQuantity != 0 || d.RemainingQuantity != 5 || !d.Expiry.Equal(noon) {
				t.Fatalf("jia detail at rollback: %+v", d)
			}
		}
	}
	if !foundJia {
		t.Fatal("jia missing from request view")
	}

	// 尝试在十一点五十九分使用甲：一律拒绝，且不改变乙的占用与实物数量。
	if _, err := s.Use("u-jia", "jia", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use jia after rollback: got %v, want ErrCommitmentClosed", err)
	}
	st2, _ := s.PartStatus("part1", before)
	if st2.PhysicalRemaining != 5 || st2.ActiveOccupied != 5 || st2.Committable != 0 {
		t.Fatalf("failed jia use changed stock: %+v", st2)
	}
	cj, _ := s.Commitment("jia")
	if cj.Used != 0 || !cj.Expired || cj.Canceled {
		t.Fatalf("jia mutated by failed use: %+v", cj)
	}
	cy, _ := s.Commitment("yi")
	if cy.Used != 0 || cy.Expired || cy.Canceled {
		t.Fatalf("yi mutated by failed jia use: %+v", cy)
	}

	// 时钟回退不影响正常承诺：乙在较早时刻仍可使用。
	if _, err := s.Use("u-yi", "yi", 1, before); err != nil {
		t.Fatalf("use yi at earlier time: %v", err)
	}
	st3, _ := s.PartStatus("part1", before)
	if st3.PhysicalRemaining != 4 || st3.ActiveOccupied != 4 {
		t.Fatalf("yi use: phys=%d occupied=%d, want 4/4", st3.PhysicalRemaining, st3.ActiveOccupied)
	}
	// 首次预留快照不反映这些变化。
	if first.Used != 0 || first.Expired || first.Canceled {
		t.Fatalf("first snapshot changed: %+v", first)
	}
}

// TestRequestViewConfirmsExpiryIrreversibly 验证请求明细查询也是到期确认入口。
func TestRequestViewConfirmsExpiryIrreversibly(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	v, err := s.RequestView("r1", after)
	if err != nil {
		t.Fatalf("request view after: %v", err)
	}
	if v.Commitments[0].Status != CommitmentExpired {
		t.Fatalf("status after expiry: %q", v.Commitments[0].Status)
	}
	// 回退时刻：仍到期。
	v2, _ := s.RequestView("r1", before)
	if v2.Commitments[0].Status != CommitmentExpired {
		t.Fatalf("status rolled back to %q, want expired", v2.Commitments[0].Status)
	}
	st, _ := s.PartStatus("part1", before)
	if st.ActiveOccupied != 0 || st.Committable != 5 {
		t.Fatalf("confirmed expiry still occupies at rollback: %+v", st)
	}
	// 取回当前承诺后按较早时刻查看，状态保持到期。
	c, _ := s.Commitment("jia")
	if c.Status(before) != CommitmentExpired {
		t.Fatalf("commitment Status at earlier time = %q, want expired", c.Status(before))
	}
}

// TestUseConfirmsExpiryIrreversibly 验证针对已知承诺的新使用判断确认到期，
// 一经确认，回退时刻的新使用也一律拒绝。
func TestUseConfirmsExpiryIrreversibly(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 到期时刻的新使用：确认到期并拒绝。
	if _, err := s.Use("u1", "jia", 1, noon); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use at expiry: got %v, want ErrCommitmentClosed", err)
	}
	// 回退时刻的新使用：仍拒绝，且不扣减。
	if _, err := s.Use("u2", "jia", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use after rollback: got %v, want ErrCommitmentClosed", err)
	}
	c, _ := s.Commitment("jia")
	if c.Used != 0 || !c.Expired {
		t.Fatalf("jia changed by rejected uses: %+v", c)
	}
	st, _ := s.PartStatus("part1", before)
	if st.PhysicalRemaining != 5 || st.ActiveOccupied != 0 || st.Committable != 5 {
		t.Fatalf("stock after confirmed expiry: %+v", st)
	}
}

// TestExpiryBoundaryOneSecond 验证到期时刻前一秒仍可用，到期时刻确认后
// 回退到前一秒也不再占用，且只释放未用部分、不补回已扣实物。
func TestExpiryBoundaryOneSecond(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, noon.Add(-time.Minute)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 到期前一秒：仍有效，使用一件。
	if _, err := s.Use("u1", "jia", 1, noon.Add(-time.Second)); err != nil {
		t.Fatalf("use one second before expiry: %v", err)
	}
	// 到期时刻：确认到期并拒绝。
	if _, err := s.Use("u2", "jia", 1, noon); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use at expiry: %v", err)
	}
	// 回退到前一秒：未用四件已永久释放，实物因已用一件剩四件。
	st, _ := s.PartStatus("part1", noon.Add(-time.Second))
	if st.PhysicalRemaining != 4 || st.ActiveOccupied != 0 || st.Committable != 4 {
		t.Fatalf("rollback after partial use+expiry: %+v", st)
	}
	d, _ := detailByID(st, "jia")
	if d.Status != CommitmentExpired || d.OriginalQuantity != 5 ||
		d.UsedQuantity != 1 || d.RemainingQuantity != 4 {
		t.Fatalf("jia detail: %+v", d)
	}
}

// TestReserveRetryDoesNotConfirmExpiry 验证原样预留重试只取回首次快照，
// 不借本次时刻确认到期，承诺在较早时刻仍开放使用。
func TestReserveRetryDoesNotConfirmExpiry(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	first, err := s.Reserve("jia", "r1", "part1", 5, noon, before)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 晚于到期时刻原样重试：仍返回首次快照，不确认到期。
	got, err := s.Reserve("jia", "r1", "part1", 5, noon, after)
	if err != nil {
		t.Fatalf("idempotent retry after expiry: %v", err)
	}
	if got != first || got.Expired {
		t.Fatalf("retry = %+v, want first %+v", got, first)
	}
	c, _ := s.Commitment("jia")
	if c.Expired {
		t.Fatal("idempotent retry confirmed expiry")
	}
	// 较早时刻仍有效、占用五件。
	st, _ := s.PartStatus("part1", before)
	if st.ActiveOccupied != 5 || st.Committable != 0 {
		t.Fatalf("retry released occupancy: %+v", st)
	}
	if d, _ := detailByID(st, "jia"); d.Status != CommitmentActive {
		t.Fatalf("jia status = %q, want active", d.Status)
	}
	// 较早时刻新使用仍成功：重试没有关闭承诺。
	if _, err := s.Use("u1", "jia", 1, before); err != nil {
		t.Fatalf("use after non-confirming retry: %v", err)
	}
}

// TestUseRetryDoesNotConfirmExpiry 验证原样使用重试只取回首次使用结果，
// 不确认到期；只有新使用才会确认。
func TestUseRetryDoesNotConfirmExpiry(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	u1, err := s.Use("u1", "jia", 2, before)
	if err != nil {
		t.Fatalf("use: %v", err)
	}
	// 晚于到期时刻原样重试：取回首次结果，不确认到期、不再次扣减。
	got, err := s.Use("u1", "jia", 2, after)
	if err != nil || got != u1 {
		t.Fatalf("use retry after expiry: %+v err %v", got, err)
	}
	c, _ := s.Commitment("jia")
	if c.Used != 2 || c.Expired {
		t.Fatalf("retry changed commitment: %+v", c)
	}
	// 到期前新使用仍成功，证明未确认。
	if _, err := s.Use("u2", "jia", 1, noon.Add(-time.Second)); err != nil {
		t.Fatalf("new use before expiry after retry: %v", err)
	}
	// 对照：新使用编号在到期时刻确认并拒绝。
	if _, err := s.Use("u3", "jia", 1, noon); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("fresh use at expiry: got %v", err)
	}
}

// TestExpiredUseRetryReturnsFirstAtEarlierTime 验证已确认到期后，原样使用
// 重试即使传入更早时刻也只返回首次结果，不重新开放、不再次扣减。
func TestExpiredUseRetryReturnsFirstAtEarlierTime(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "jia", 2, before); err != nil {
		t.Fatalf("use: %v", err)
	}
	// 由库存查询确认到期。
	if _, err := s.PartStatus("part1", after); err != nil {
		t.Fatalf("part status: %v", err)
	}
	// 回退时刻原样重试：仍返回首次结果。
	u, err := s.Use("u1", "jia", 2, before)
	if err != nil || u.Quantity != 2 {
		t.Fatalf("retry after confirmed expiry: %+v err %v", u, err)
	}
	c, _ := s.Commitment("jia")
	if c.Used != 2 || !c.Expired {
		t.Fatalf("commitment changed: %+v", c)
	}
	st, _ := s.PartStatus("part1", before)
	if st.PhysicalRemaining != 3 || st.ActiveOccupied != 0 {
		t.Fatalf("stock: %+v, want phys=3 occupied=0", st)
	}
	// 回退时刻新使用仍被拒。
	if _, err := s.Use("u2", "jia", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("new use after rollback: %v", err)
	}
}

// TestFailedCallsDoNotConfirmExpiry 验证参数非法或引用对象不存在而提前失败
// 的调用，即使传入晚于到期的时刻，也不确认任何承诺到期。
func TestFailedCallsDoNotConfirmExpiry(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// 预留：参数非法。
	if _, err := s.Reserve("", "r1", "part1", 1, expiryOK, after); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty commit id: %v", err)
	}
	if _, err := s.Reserve("x1", "r1", "part1", 0, expiryOK, after); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero quantity: %v", err)
	}
	if _, err := s.Reserve("x2", "r1", "part1", 1, before, after); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("past expiry: %v", err)
	}
	// 预留：引用对象不存在。
	if _, err := s.Reserve("x3", "rMissing", "part1", 1, expiryOK, after); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: %v", err)
	}
	if _, err := s.Reserve("x4", "r1", "partMissing", 1, expiryOK, after); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown part: %v", err)
	}
	// 使用：参数非法 / 承诺不存在。
	if _, err := s.Use("", "jia", 1, after); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty usage id: %v", err)
	}
	if _, err := s.Use("y1", "jia", 0, after); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("zero usage quantity: %v", err)
	}
	if _, err := s.Use("y2", "cMissing", 1, after); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown commitment: %v", err)
	}
	// 查询：引用对象不存在。
	if _, err := s.PartStatus("partMissing", after); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown part status: %v", err)
	}
	if _, err := s.RequestView("rMissing", after); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request view: %v", err)
	}

	// 甲始终未确认：较早时刻仍有效、占用五件，新使用成功。
	st, _ := s.PartStatus("part1", before)
	if st.ActiveOccupied != 5 || st.Committable != 0 {
		t.Fatalf("failed calls released occupancy: %+v", st)
	}
	if d, _ := detailByID(st, "jia"); d.Status != CommitmentActive {
		t.Fatalf("jia status = %q, want active", d.Status)
	}
	if _, err := s.Use("ok", "jia", 1, before); err != nil {
		t.Fatalf("jia closed by a non-confirming failed call: %v", err)
	}
}

// TestCanceledStaysCanceled 验证已取消的承诺不会被确认成 expired：晚时刻、
// 早时刻都显示 canceled，占用始终为零，重复取消不产生数量变化。
func TestCanceledStaysCanceled(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Use("u1", "jia", 2, before); err != nil {
		t.Fatalf("use: %v", err)
	}
	if _, err := s.Cancel("jia", before); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	for _, tm := range []time.Time{after, before} {
		st, err := s.PartStatus("part1", tm)
		if err != nil {
			t.Fatalf("part status: %v", err)
		}
		d, _ := detailByID(st, "jia")
		if d.Status != CommitmentCanceled {
			t.Fatalf("at %v status = %q, want canceled", tm, d.Status)
		}
		if st.ActiveOccupied != 0 || st.Committable != 3 || st.PhysicalRemaining != 3 {
			t.Fatalf("at %v stock = %+v, want phys=3 occupied=0 committable=3", tm, st)
		}
	}
	c, _ := s.Commitment("jia")
	if !c.Canceled || c.Expired {
		t.Fatalf("canceled commitment marked expired: %+v", c)
	}
	// 重复取消不释放更多、不改变标记。
	if _, err := s.Cancel("jia", after); err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	c2, _ := s.Commitment("jia")
	if !c2.Canceled || c2.Expired || c2.Used != 2 {
		t.Fatalf("repeat cancel changed commitment: %+v", c2)
	}
	// 新使用（无论时刻）一律拒绝。
	if _, err := s.Use("u2", "jia", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use canceled at earlier time: %v", err)
	}
}

// TestExpiryConfirmationAppendsNoHistory 验证确认到期不覆盖预留历史，
// 也不追加到期历史。
func TestExpiryConfirmationAppendsNoHistory(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Reserve("yi", "r2", "part1", 5, expiryOK, after); err != nil {
		t.Fatalf("reserve yi: %v", err)
	}
	// 多轮确认到期：库存查询、请求查询、被拒使用。
	if _, err := s.PartStatus("part1", before); err != nil {
		t.Fatalf("part status: %v", err)
	}
	if _, err := s.RequestView("r1", before); err != nil {
		t.Fatalf("request view: %v", err)
	}
	if _, err := s.Use("u-jia", "jia", 1, before); !errors.Is(err, ErrCommitmentClosed) {
		t.Fatalf("use jia: %v", err)
	}
	h1, _ := s.RequestHistory("r1")
	if len(h1) != 1 || !h1[0].Success || h1[0].CommitID != "jia" {
		t.Fatalf("r1 history = %+v, want single jia success", h1)
	}
	if h1[0].StockBasis == nil || h1[0].StockBasis.PhysicalRemaining != 5 ||
		h1[0].StockBasis.ActiveOccupied != 0 || h1[0].StockBasis.Committable != 5 {
		t.Fatalf("first stock basis overwritten: %+v", h1[0].StockBasis)
	}
	h2, _ := s.RequestHistory("r2")
	if len(h2) != 1 || !h2[0].Success || h2[0].CommitID != "yi" {
		t.Fatalf("r2 history = %+v, want single yi success", h2)
	}
}

// TestNewReserveAtEarlierTimeRejudgesFresh 验证两点：已确认到期的承诺在较早
// 时刻的新预留核算中不复活；保修资格只按本次时刻判断，不被此前较大的历史
// 时刻替换。
func TestNewReserveAtEarlierTimeRejudgesFresh(t *testing.T) {
	s := expStore(t, 5)
	jiaExpiry := t0.Add(12 * day)
	// 第十一天预留甲，第十二天到期。
	if _, err := s.Reserve("jia", "r1", "part1", 5, jiaExpiry, t0.Add(11*day)); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}
	// 第三十一天（请求此时已过保）库存查询：确认甲到期。
	st, err := s.PartStatus("part1", t0.Add(31*day))
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if d, _ := detailByID(st, "jia"); d.Status != CommitmentExpired {
		t.Fatalf("jia = %q, want expired", d.Status)
	}
	// 回到第十一天（保修期内）为同一请求新预留五件：资格按第十一天判合格；
	// 甲虽在其到期时刻之前，但已确认到期，不复活、不占用，乙预留成功。
	if _, err := s.Reserve("bing", "r1", "part1", 5, expiryOK, t0.Add(11*day)); err != nil {
		t.Fatalf("reserve bing at earlier eligible time: %v", err)
	}
	early, _ := s.PartStatus("part1", t0.Add(11*day))
	if early.ActiveOccupied != 5 || early.Committable != 0 {
		t.Fatalf("early stock = %+v, want occupied=5 committable=0", early)
	}
	dj, _ := detailByID(early, "jia")
	db, _ := detailByID(early, "bing")
	if dj.Status != CommitmentExpired {
		t.Fatalf("jia resurrected at earlier time: %q", dj.Status)
	}
	if db.Status != CommitmentActive || db.RemainingQuantity != 5 {
		t.Fatalf("bing not active at earlier time: %+v", db)
	}
}

// TestNewReserveStockMathConfirmsEvenWhenReserveFails 验证引用有效的新预留
// 即使因资格不合格或库存不足而失败，其库存核算仍确认该备件到期承诺；且
// 库存依据记录的是确认释放后的可承诺量。
func TestNewReserveStockMathConfirmsEvenWhenReserveFails(t *testing.T) {
	s := expStore(t, 5)
	// 第三个请求命中除外代码，始终不合格。
	if err := s.SubmitRequest("r3", "p1", "FAULTX"); err != nil {
		t.Fatalf("submit r3: %v", err)
	}
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	// 甲占满五件。
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}

	// 晚时刻为不合格请求预留：库存核算先确认甲到期（释放五件），随后资格拒绝。
	if _, err := s.Reserve("bad1", "r3", "part1", 1, expiryOK, after); !errors.Is(err, ErrIneligible) {
		t.Fatalf("ineligible reserve: got %v", err)
	}
	// 回退时刻：甲已永久释放。
	st1, _ := s.PartStatus("part1", before)
	if st1.ActiveOccupied != 0 || st1.Committable != 5 {
		t.Fatalf("ineligible reserve did not confirm expiry: %+v", st1)
	}

	// 另一仓库：合格请求但库存不足（要求超过五件），核算同样确认到期，
	// 失败记录的库存依据反映释放后的可承诺量。
	s2 := expStore(t, 5)
	if _, err := s2.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}
	if _, err := s2.Reserve("big", "r2", "part1", 6, expiryOK.Add(2*day), after); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("over-stock reserve: got %v, want ErrInsufficientStock", err)
	}
	h, _ := s2.RequestHistory("r2")
	if len(h) != 1 || h[0].Error != HistoryErrorInsufficientStock ||
		h[0].StockBasis == nil || h[0].StockBasis.ActiveOccupied != 0 ||
		h[0].StockBasis.Committable != 5 {
		t.Fatalf("insufficient-stock record basis: %+v", h)
	}
	st2, _ := s2.PartStatus("part1", before)
	if st2.ActiveOccupied != 0 || st2.Committable != 5 {
		t.Fatalf("insufficient-stock reserve did not confirm expiry: %+v", st2)
	}
}

// TestRequestViewEligibilityUsesCurrentTimeAfterConfirmation 验证确认到期不
// 改变资格规则：曾用较大（过保）时刻查询确认到期，回退到保修期内查询时，
// 资格仍按本次较早时刻判合格，不被历史大时刻替换。
func TestRequestViewEligibilityUsesCurrentTimeAfterConfirmation(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	afterWarranty := t0.Add(31 * day) // 第 31 天：已过保。
	withinWarranty := t0.Add(11 * day)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, t0.Add(9*day)); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}
	// 过保时刻查询：确认甲到期，资格不合格。
	v1, err := s.RequestView("r1", afterWarranty)
	if err != nil {
		t.Fatalf("view after warranty: %v", err)
	}
	if v1.Eligibility.Eligible {
		t.Fatalf("expected ineligible at day 31: %+v", v1.Eligibility)
	}
	// 回退到保修期内：甲保持到期，但资格按本次时刻判合格。
	v2, err := s.RequestView("r1", withinWarranty)
	if err != nil {
		t.Fatalf("view within warranty: %v", err)
	}
	if !v2.Eligibility.Eligible || len(v2.Eligibility.Reasons) != 0 {
		t.Fatalf("eligibility not re-judged at earlier time: %+v", v2.Eligibility)
	}
	if v2.Commitments[0].Status != CommitmentExpired {
		t.Fatalf("jia status = %q, want expired", v2.Commitments[0].Status)
	}
}

// TestPartiallyUsedExpiredOnlyReleasesRemainder 验证到期只释放未用部分：
// 甲用掉两件后到期，实物只剩三件、可再承诺三件，无法按五件重复预留。
func TestPartiallyUsedExpiredOnlyReleasesRemainder(t *testing.T) {
	s := expStore(t, 5)
	noon := t0.Add(10 * day)
	before := noon.Add(-time.Minute)
	after := noon.Add(time.Minute)
	if _, err := s.Reserve("jia", "r1", "part1", 5, noon, before); err != nil {
		t.Fatalf("reserve jia: %v", err)
	}
	if _, err := s.Use("u1", "jia", 2, before); err != nil {
		t.Fatalf("use jia: %v", err)
	}
	// 到期后乙只能预留剩余实物三件。
	if _, err := s.Reserve("yi", "r2", "part1", 3, expiryOK, after); err != nil {
		t.Fatalf("reserve yi 3: %v", err)
	}
	if _, err := s.Reserve("bing", "r2", "part1", 1, expiryOK.Add(time.Hour), after); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve bing 1: got %v, want ErrInsufficientStock", err)
	}
	// 较早时刻回退查询：仍只有乙占用三件。
	st, _ := s.PartStatus("part1", before)
	if st.PhysicalRemaining != 3 || st.ActiveOccupied != 3 || st.Committable != 0 {
		t.Fatalf("rollback stock = %+v, want phys=3 occupied=3 committable=0", st)
	}
}
