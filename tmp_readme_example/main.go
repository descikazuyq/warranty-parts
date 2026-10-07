package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/warranty-parts/warranty"
)

func main() {
	s := warranty.NewStore()
	// must 用于预期成功的操作：遇到意外错误立即停止，不继续输出成功结果。
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	// expectErr 用于预期拒绝：错误必须恰好是目标原因，否则立即停止，
	// 不把真正的意外错误当成“预期拒绝”略过。
	expectErr := func(err error, target error) {
		if !errors.Is(err, target) {
			panic(fmt.Sprintf("want %v, got %v", target, err))
		}
	}

	// 固定时刻：2026-01-01 购买、保修三百六十五天，示例全程在保；
	// 全部操作取同一时刻，承诺全程未取消且未到期，故障代码 NOISE 不在除外清单。
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := purchase.AddDate(0, 0, 10)    // 2026-01-11，保修期内
	expiry := purchase.AddDate(0, 0, 40) // 2026-02-10，晚于全部操作时刻

	// 登记：产品始终在保、故障代码不命中除外；备件初始库存十件。
	must(s.RegisterProduct("P1", purchase, 365, []string{"BROKEN_SEAL"}))
	must(s.RegisterPart("PART-A", 10))
	must(s.SubmitRequest("REQ-1", "P1", "NOISE"))

	// 为这个已知请求预留五件。
	_, err := s.Reserve("COMMIT-1", "REQ-1", "PART-A", 5, expiry, now)
	must(err)

	// state 打印承诺数量（原定/已用/未用）、三项库存账目
	// （实物剩余/有效占用/可承诺）与该承诺的成功使用明细。
	state := func(label string) {
		c, err := s.Commitment("COMMIT-1")
		must(err)
		ps, err := s.PartStatus("PART-A", now)
		must(err)
		fmt.Printf("%-8s commit=%d/%d/%d stock=%d/%d/%d\n",
			label, c.Quantity, c.Used, c.Unused(),
			ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)
		us, err := s.CommitmentUsages("COMMIT-1")
		must(err)
		text := "(none)"
		sum := 0
		if len(us) > 0 {
			text = ""
			for i, u := range us {
				if i > 0 {
					text += " "
				}
				text += fmt.Sprintf("%s@%s:%d", u.ID, u.CommitmentID, u.Quantity)
				sum += u.Quantity
			}
		}
		fmt.Printf("%-8s usages count=%d sum=%d: %s\n", label, len(us), sum, text)
	}

	// 先用另一个使用编号成功领取两件。
	u2, err := s.Use("USE-1", "COMMIT-1", 2, now)
	must(err)
	fmt.Printf("use      : %s@%s qty=%d\n", u2.ID, u2.CommitmentID, u2.Quantity)
	// 承诺已用两件、未用三件；实物剩余八件、有效占用三件、可承诺五件；
	// 成功使用明细只有这两件一次。
	state("setup:")

	// 用一个从未成功过的使用编号领取四件：本笔承诺未用仅三件。
	// 即使实物足够（八件），也是超过本笔承诺未用数量的新使用，
	// 必须整次失败：不能先领走允许的三件，也不能提前占住 USE-2。
	_, err = s.Use("USE-2", "COMMIT-1", 4, now)
	exceeded := errors.Is(err, warranty.ErrUsageExceeded)
	expectErr(err, warranty.ErrUsageExceeded) // 是超量拒绝，不是编号冲突
	fmt.Println("exceeded :", exceeded)
	// 承诺数量与三项库存账目保持原值；成功使用明细仍只有此前两件。
	state("denied4:")

	// 沿用刚才失败的编号，仍指向原承诺，把数量改成三件：
	// 失败不占编号，这是合法的新使用，应当成功。
	u3, err := s.Use("USE-2", "COMMIT-1", 3, now)
	must(err)
	fmt.Printf("use      : %s@%s qty=%d\n", u3.ID, u3.CommitmentID, u3.Quantity)
	// 承诺累计已用五件、未用零件；实物五件、有效占用零件、可承诺五件；
	// 成功使用明细是两件和三件两次领取，合计五件。
	state("took3:")

	// 再次按相同编号、承诺和三件数量提交：取回同一成功结果，
	// 不再扣减，也不增加明细。
	u3again, err := s.Use("USE-2", "COMMIT-1", 3, now)
	must(err)
	fmt.Printf("retry    : %s@%s qty=%d same=%v\n",
		u3again.ID, u3again.CommitmentID, u3again.Quantity, u3again == u3)
	state("retry3:")

	// 再把数量改回最初被拒绝的四件：编号内容已由三件的首次成功确定，
	// 这时才是编号冲突；三件的成功结果与全部账目都保留。
	_, err = s.Use("USE-2", "COMMIT-1", 4, now)
	conflict := errors.Is(err, warranty.ErrConflict)
	expectErr(err, warranty.ErrConflict)
	fmt.Println("conflict :", conflict)
	state("final:")
}
