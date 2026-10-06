package warranty

import (
	"errors"
	"sync"
	"testing"
)

// 本文件回归保障取消承诺释放库存与另一张保修请求提交新预留同时发生时的行为：
// 取消只释放旧承诺尚未使用的占用，已领取的实物不回到库存；新请求不能在取消
// 尚未生效时提前取得这部分数量。两种处理先后都合法——新预留在取消生效前被
// 处理时返回 ErrInsufficientStock 且不创建承诺，取消先释放未用占用时新预留
// 成功——但不允许出现预留成功却没有对应占用的结果。无论哪种先后，旧承诺最终
// 都显示已取消并保留原定数量、已用数量与原到期时刻，另一笔承诺的状态、数量
// 和归属保持不变，请求查询与备件明细与实际成功的预留一致。另覆盖取消释放后
// 申请数量超过可承诺数量的边界：整次拒绝，库存与其他承诺不变。
//
// 业务条件：备件初始库存十件，旧承诺 cOld 预留六件已成功使用两件，另一笔
// 承诺 cB 预留四件且尚未使用，此时实物八件、有效占用八件、可承诺零件。第三
// 张请求 rC 用尚未成功过的新编号申请预留四件，与取消 cOld 同时发生。所有
// 操作时刻都早于各承诺的到期时刻，参与预留的请求均符合保修资格。

// 本文件各测试共用的承诺编号与到期时刻（均晚于全部操作时刻 nowOK）。
var (
	expOld6 = t0.Add(40 * day) // cOld 的原到期时刻
	expB4r  = t0.Add(30 * day) // cB 的到期时刻
	expNew4 = t0.Add(35 * day) // rC 新承诺的到期时刻
)

// cancelRaceStore 构造保修 60 天的产品、初始库存 10 件的备件和三个保修资格
// 请求（rA、rB、rC），并落好两笔旧承诺：rA 的 cOld 预留 6 件且已成功使用
// 2 件，rB 的 cB 预留 4 件尚未使用。返回前核对账目为实物 8、有效占用 8、
// 可承诺 0。
func cancelRaceStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if err := s.RegisterProduct("p1", t0, 60, nil); err != nil {
		t.Fatalf("register product: %v", err)
	}
	if err := s.RegisterPart("part1", 10); err != nil {
		t.Fatalf("register part: %v", err)
	}
	for _, id := range []string{"rA", "rB", "rC"} {
		if err := s.SubmitRequest(id, "p1", "FAULTY"); err != nil {
			t.Fatalf("submit request %s: %v", id, err)
		}
	}
	if _, err := s.Reserve("cOld", "rA", "part1", 6, expOld6, nowOK); err != nil {
		t.Fatalf("reserve cOld: %v", err)
	}
	if _, err := s.Use("uOld-used", "cOld", 2, nowOK); err != nil {
		t.Fatalf("use 2 from cOld: %v", err)
	}
	if _, err := s.Reserve("cB", "rB", "part1", 4, expB4r, nowOK); err != nil {
		t.Fatalf("reserve cB: %v", err)
	}
	assertPartAccount(t, s, 8, 8, 0)
	return s
}

// assertCanceledOldCommitment 核对旧承诺 cOld 的最终状态：已取消，原定 6 件、
// 已用 2 件、未用 4 件及原到期时刻全部保留；请求甲的视图与备件明细中的取消
// 记录不缺失、内容一致。
func assertCanceledOldCommitment(t *testing.T, s *Store) {
	t.Helper()
	got, err := s.Commitment("cOld")
	if err != nil {
		t.Fatalf("commitment cOld: %v", err)
	}
	if !got.Canceled || got.Quantity != 6 || got.Used != 2 || got.Unused() != 4 ||
		got.RequestID != "rA" || got.PartID != "part1" || !got.Expiry.Equal(expOld6) {
		t.Fatalf("canceled cOld: %+v, want qty=6 used=2 unused=4 canceled=true expiry=%v",
			got, expOld6)
	}

	viewA, err := s.RequestView("rA", nowOK)
	if err != nil {
		t.Fatalf("request view rA: %v", err)
	}
	if len(viewA.Commitments) != 1 {
		t.Fatalf("rA commitments = %d, want 1 (canceled record still visible)", len(viewA.Commitments))
	}
	dOld := mustDetailByID(t, viewA.Commitments, "cOld")
	if dOld.Status != CommitmentCanceled || dOld.OriginalQuantity != 6 ||
		dOld.UsedQuantity != 2 || dOld.RemainingQuantity != 4 ||
		dOld.RequestID != "rA" || dOld.PartID != "part1" || !dOld.Expiry.Equal(expOld6) {
		t.Fatalf("rA view of cOld: %+v", dOld)
	}

	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	pdOld := mustDetailByID(t, st.Details, "cOld")
	if pdOld.Status != CommitmentCanceled || pdOld.OriginalQuantity != 6 ||
		pdOld.UsedQuantity != 2 || pdOld.RemainingQuantity != 4 || !pdOld.Expiry.Equal(expOld6) {
		t.Fatalf("part detail cOld: %+v", pdOld)
	}
}

// assertUntouchedBCommitment 核对另一笔四件承诺 cB 的状态、数量、归属与到期
// 时刻保持不变：请求乙的视图与备件明细中均为有效承诺。
func assertUntouchedBCommitment(t *testing.T, s *Store) {
	t.Helper()
	got, err := s.Commitment("cB")
	if err != nil {
		t.Fatalf("commitment cB: %v", err)
	}
	if got.Canceled || got.Expired || got.Quantity != 4 || got.Used != 0 ||
		got.RequestID != "rB" || got.PartID != "part1" || !got.Expiry.Equal(expB4r) {
		t.Fatalf("cB disturbed: %+v", got)
	}

	viewB, err := s.RequestView("rB", nowOK)
	if err != nil {
		t.Fatalf("request view rB: %v", err)
	}
	if len(viewB.Commitments) != 1 {
		t.Fatalf("rB commitments = %d, want 1", len(viewB.Commitments))
	}
	dB := mustDetailByID(t, viewB.Commitments, "cB")
	if dB.Status != CommitmentActive || dB.OriginalQuantity != 4 ||
		dB.UsedQuantity != 0 || dB.RemainingQuantity != 4 ||
		dB.RequestID != "rB" || dB.PartID != "part1" || !dB.Expiry.Equal(expB4r) {
		t.Fatalf("rB view of cB changed: %+v", dB)
	}
}

// assertNewCommitmentAbsent 核对失败的新预留不留任何痕迹：承诺不存在，请求
// 丙的视图没有关联承诺，备件明细只有两笔旧记录。
func assertNewCommitmentAbsent(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Commitment("cNew"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed reserve left commitment: got %v, want ErrNotFound", err)
	}
	viewC, err := s.RequestView("rC", nowOK)
	if err != nil {
		t.Fatalf("request view rC: %v", err)
	}
	if len(viewC.Commitments) != 0 {
		t.Fatalf("rC commitments = %+v, want none after failed reserve", viewC.Commitments)
	}
	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if len(st.Details) != 2 {
		t.Fatalf("part details = %d, want 2 (failed reserve leaves no detail)", len(st.Details))
	}
}

// assertNewCommitmentPresent 核对成功的新预留有对应占用与明细：承诺有效、
// 数量四件、归属请求丙，请求视图与备件明细一致。
func assertNewCommitmentPresent(t *testing.T, s *Store, c Commitment) {
	t.Helper()
	want := Commitment{ID: "cNew", RequestID: "rC", PartID: "part1", Quantity: 4, Expiry: expNew4}
	if c != want {
		t.Fatalf("reserve result = %+v, want %+v", c, want)
	}
	got, err := s.Commitment("cNew")
	if err != nil {
		t.Fatalf("commitment cNew: %v", err)
	}
	if got != want {
		t.Fatalf("stored cNew = %+v, want %+v", got, want)
	}

	viewC, err := s.RequestView("rC", nowOK)
	if err != nil {
		t.Fatalf("request view rC: %v", err)
	}
	if len(viewC.Commitments) != 1 {
		t.Fatalf("rC commitments = %d, want 1", len(viewC.Commitments))
	}
	dNew := mustDetailByID(t, viewC.Commitments, "cNew")
	if dNew.Status != CommitmentActive || dNew.OriginalQuantity != 4 ||
		dNew.UsedQuantity != 0 || dNew.RemainingQuantity != 4 ||
		dNew.RequestID != "rC" || dNew.PartID != "part1" || !dNew.Expiry.Equal(expNew4) {
		t.Fatalf("rC view of cNew: %+v", dNew)
	}

	st, err := s.PartStatus("part1", nowOK)
	if err != nil {
		t.Fatalf("part status: %v", err)
	}
	if len(st.Details) != 3 {
		t.Fatalf("part details = %d, want 3 (canceled record retained)", len(st.Details))
	}
	pdNew := mustDetailByID(t, st.Details, "cNew")
	if pdNew.Status != CommitmentActive || pdNew.RemainingQuantity != 4 {
		t.Fatalf("part detail cNew: %+v", pdNew)
	}
}

// TestConcurrentCancelAndNewReserveEitherOrderConsistent 让取消旧承诺 cOld 与
// 请求丙的新预留（新编号 cNew、四件）在同一道闸机后并发进入，不预设处理先后。
// 两种合法结果都被接受并分别核对：
//   - 新预留先被处理：可承诺为零，返回 ErrInsufficientStock，不创建承诺；取消
//     完成后实物八件、有效占用四件、可承诺四件。
//   - 取消先生效：释放旧承诺四件未用占用，新预留成功；最终实物八件、有效占用
//     八件、可承诺零件，成功的预留必有对应占用。
//
// 无论哪种先后，旧承诺都显示已取消并保留原定六件、已用两件、未用四件及原到期
// 时刻，另一笔四件承诺保持不变。
func TestConcurrentCancelAndNewReserveEitherOrderConsistent(t *testing.T) {
	for iter := 0; iter < 10; iter++ {
		t.Run("iteration", func(t *testing.T) {
			s := cancelRaceStore(t)

			var wg sync.WaitGroup
			start := make(chan struct{})
			var reserveC Commitment
			var reserveErr error
			var cancelC Commitment
			var cancelErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				reserveC, reserveErr = s.Reserve("cNew", "rC", "part1", 4, expNew4, nowOK)
			}()
			go func() {
				defer wg.Done()
				<-start
				cancelC, cancelErr = s.Cancel("cOld", nowOK)
			}()
			close(start)
			wg.Wait()

			// 取消总是成功：返回结果已取消，原定 6、已用 2、未用 4、原到期时刻保留。
			if cancelErr != nil {
				t.Fatalf("cancel cOld: %v", cancelErr)
			}
			if !cancelC.Canceled || cancelC.Quantity != 6 || cancelC.Used != 2 ||
				cancelC.Unused() != 4 || !cancelC.Expiry.Equal(expOld6) {
				t.Fatalf("cancel result: %+v, want qty=6 used=2 unused=4 canceled=true", cancelC)
			}

			switch {
			case errors.Is(reserveErr, ErrInsufficientStock):
				// 新预留在取消生效前被处理：可承诺为零，整次拒绝，不创建承诺，
				// 返回空承诺。取消完成后实物 8、有效占用 4、可承诺 4。
				if reserveC != (Commitment{}) {
					t.Fatalf("rejected reserve returned commitment %+v, want zero", reserveC)
				}
				assertPartAccount(t, s, 8, 4, 4)
				assertNewCommitmentAbsent(t, s)

				// 失败记录留在请求丙名下：库存不足类别，资格合格，库存依据
				// 是取消生效前的实物 8、占用 8、可承诺 0。
				hist, err := s.RequestHistory("rC")
				if err != nil {
					t.Fatalf("request history rC: %v", err)
				}
				if len(hist) != 1 {
					t.Fatalf("rC history len = %d, want 1", len(hist))
				}
				rec := hist[0]
				if rec.Seq != 1 || rec.Success || rec.Error != HistoryErrorInsufficientStock ||
					rec.CommitID != "cNew" || rec.PartID != "part1" || rec.Quantity != 4 ||
					!rec.Expiry.Equal(expNew4) || !rec.Now.Equal(nowOK) {
					t.Fatalf("rC failure record = %+v", rec)
				}
				if rec.Eligibility == nil || !rec.Eligibility.Eligible {
					t.Fatalf("rC failure eligibility = %+v, want eligible snapshot", rec.Eligibility)
				}
				if rec.StockBasis == nil || rec.StockBasis.PhysicalRemaining != 8 ||
					rec.StockBasis.ActiveOccupied != 8 || rec.StockBasis.Committable != 0 {
					t.Fatalf("rC failure stock basis = %+v, want phys=8 occupied=8 committable=0",
						rec.StockBasis)
				}

			case reserveErr == nil:
				// 取消先生效：释放的四件未用占用使新预留成功。最终实物 8、
				// 有效占用 8（cB 四件加 cNew 四件）、可承诺 0——成功的预留
				// 必有对应占用。
				assertPartAccount(t, s, 8, 8, 0)
				assertNewCommitmentPresent(t, s, reserveC)

				// 成功记录留在请求丙名下：资格合格，库存依据是取消生效后的
				// 实物 8、占用 4、可承诺 4。
				hist, err := s.RequestHistory("rC")
				if err != nil {
					t.Fatalf("request history rC: %v", err)
				}
				if len(hist) != 1 {
					t.Fatalf("rC history len = %d, want 1", len(hist))
				}
				rec := hist[0]
				if rec.Seq != 1 || !rec.Success || rec.Error != "" ||
					rec.CommitID != "cNew" || rec.PartID != "part1" || rec.Quantity != 4 ||
					!rec.Expiry.Equal(expNew4) || !rec.Now.Equal(nowOK) {
					t.Fatalf("rC success record = %+v", rec)
				}
				if rec.Eligibility == nil || !rec.Eligibility.Eligible {
					t.Fatalf("rC success eligibility = %+v, want eligible snapshot", rec.Eligibility)
				}
				if rec.StockBasis == nil || rec.StockBasis.PhysicalRemaining != 8 ||
					rec.StockBasis.ActiveOccupied != 4 || rec.StockBasis.Committable != 4 {
					t.Fatalf("rC success stock basis = %+v, want phys=8 occupied=4 committable=4",
						rec.StockBasis)
				}

			default:
				t.Fatalf("reserve returned unexpected error: %v (commitment %+v)", reserveErr, reserveC)
			}

			// 两种先后共用的不变量：旧承诺已取消且事实保留，另一笔承诺不变。
			assertCanceledOldCommitment(t, s)
			assertUntouchedBCommitment(t, s)
		})
	}
}

// TestCancelFreedQuantityBoundaryReserveFiveRejected 覆盖取消释放后、四件尚未
// 被新承诺占用时申请五件的边界：可承诺只有四件，整次返回 ErrInsufficientStock，
// 实物库存与其他承诺的占用均不改变，不创建新承诺；随后申请恰好四件可以成功，
// 证明拒绝只针对超出的数量。
func TestCancelFreedQuantityBoundaryReserveFiveRejected(t *testing.T) {
	s := cancelRaceStore(t)

	canceled, err := s.Cancel("cOld", nowOK)
	if err != nil {
		t.Fatalf("cancel cOld: %v", err)
	}
	if !canceled.Canceled || canceled.Quantity != 6 || canceled.Used != 2 || canceled.Unused() != 4 {
		t.Fatalf("cancel result: %+v, want qty=6 used=2 unused=4 canceled=true", canceled)
	}
	// 取消释放四件未用占用：实物 8、有效占用 4、可承诺 4。
	assertPartAccount(t, s, 8, 4, 4)

	// 申请五件超过可承诺的四件：整次拒绝。
	five, err := s.Reserve("cNew5", "rC", "part1", 5, expNew4, nowOK)
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("reserve 5 against committable 4: got %v, want ErrInsufficientStock", err)
	}
	if five != (Commitment{}) {
		t.Fatalf("rejected reserve returned commitment %+v, want zero", five)
	}

	// 实物库存与其他承诺的占用均不改变，不创建新承诺。
	assertPartAccount(t, s, 8, 4, 4)
	if _, err := s.Commitment("cNew5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected reserve left commitment: got %v, want ErrNotFound", err)
	}
	assertCanceledOldCommitment(t, s)
	assertUntouchedBCommitment(t, s)

	// 失败记录保留当次数量与库存依据：实物 8、占用 4、可承诺 4。
	hist, err := s.RequestHistory("rC")
	if err != nil {
		t.Fatalf("request history rC: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("rC history len = %d, want 1", len(hist))
	}
	rec := hist[0]
	if rec.Seq != 1 || rec.Success || rec.Error != HistoryErrorInsufficientStock ||
		rec.CommitID != "cNew5" || rec.Quantity != 5 ||
		rec.StockBasis == nil || rec.StockBasis.PhysicalRemaining != 8 ||
		rec.StockBasis.ActiveOccupied != 4 || rec.StockBasis.Committable != 4 {
		t.Fatalf("rC failure record = %+v", rec)
	}

	// 申请恰好四件（可承诺数量）可以成功：实物 8、有效占用 8、可承诺 0。
	four, err := s.Reserve("cNew4", "rC", "part1", 4, expNew4, nowOK)
	if err != nil {
		t.Fatalf("reserve exactly committable 4: %v", err)
	}
	if four.Quantity != 4 || four.Used != 0 || four.RequestID != "rC" || four.Canceled || four.Expired {
		t.Fatalf("boundary commitment: %+v", four)
	}
	assertPartAccount(t, s, 8, 8, 0)
	assertCanceledOldCommitment(t, s)
	assertUntouchedBCommitment(t, s)
}
