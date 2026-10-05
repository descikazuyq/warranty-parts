# 本地保修资格与备件承诺

本机程序调用的 Go 包：登记产品保修信息、备件库存与保修请求，按调用方给定的
当前时刻判断保修资格，并对备件进行承诺预留、分批使用与取消。全部状态保存在
进程内，使用一把互斥锁保证并发安全；`Ready()` 基线行为保持不变。

## 使用

```bash
go test ./...
```

## 典型流程

```go
s := warranty.NewStore()

// 1. 登记：产品（购买时刻、保修天数、除外故障代码）、备件（初始库存）、请求。
_ = s.RegisterProduct("P1", purchaseTime, 365, []string{"BROKEN_SEAL"})
_ = s.RegisterPart("PART-A", 100)
_ = s.SubmitRequest("REQ-1", "P1", "NOISE")

// 2. 按当次时刻预留承诺：每次首次预留都重新判断资格；到期时刻必须晚于当前时刻。
//    同一编号原样重试（请求、备件、数量、到期时刻一致；当前时刻不属于提交内容）
//    始终取回首次成功时的承诺快照，与本次当前时刻及承诺后来的使用、取消、到期无关。
c, err := s.Reserve("COMMIT-1", "REQ-1", "PART-A", 10, expiry, now)

// 3. 分批使用：每次带全局唯一使用编号，扣减承诺未用数量与实物库存。
u, err := s.Use("USE-1", "COMMIT-1", 4, now)

// 4. 取消：只释放未用数量；到期自动失效并释放余量，无需另做清理。
c, err = s.Cancel("COMMIT-1", now)

// 5. 查询：按请求看资格依据/拒绝原因/关联承诺；按备件看实物剩余/占用/可承诺/明细。
rv, _ := s.RequestView("REQ-1", now)
ps, _ := s.PartStatus("PART-A", now)

// 6. 历史：按请求查看每次首次预留成功与每次失败提交的处理记录（无需当前时刻）。
//    记录含提交编号、备件、数量、到期时刻、当次当前时刻、成功或错误类别，
//    以及当次的资格依据快照与库存依据快照；序号严格递增，失败记录不被成功覆盖。
h, _ := s.RequestHistory("REQ-1")
```

## 过保后使用既有备件承诺

保修资格与承诺有效是两个互相独立的期限，作用阶段不同，不能互相替代：

- **保修截止时刻只约束“新承诺的首次预留”**。每次 `Reserve` 都按当次调用传入的
  当前时刻重新判断资格（是否过保、故障代码是否命中除外清单）；资格判断只在首次
  预留成功所需的那一次起作用。
- **承诺一旦预留成功，后续使用只受承诺自己约束**：本次当前时刻是否达到承诺自身
  的到期时刻、承诺是否已被取消、本次数量是否超过承诺的未用数量。使用旧承诺不会
  重新判断产品保修资格。
- 产品后来过保，既不会自动取消已经成立的承诺，也不会提前释放其未用占用：过保后
  用**新承诺编号**发起的首次预留一律返回 `ErrIneligible` 且不增加占用；而对旧承诺
  的 `Use` 是另一种操作，仍按旧承诺自身的到期时刻、取消状态和未用数量执行。
- 承诺到期（或取消）释放的只是**未用数量对应的占用**，可承诺数量相应回升；已经
  通过 `Use` 领走的实物不会因此退回库存。承诺明细仍保留原定数量、已用数量和未用
  数量，状态显示为 `expired`（取消则显示 `canceled`）。

下面的完整示例采用固定购买时刻、三十天保修期和十件初始库存，故障代码不在除外
清单中：保修截止前成功预留四件，承诺到期安排在保修截止之后；随后依次走到保修
截止时刻与承诺自身的到期时刻。

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/warranty-parts/warranty"
)

func main() {
	loc := time.UTC
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	warrantyEnd := time.Date(2026, 1, 31, 0, 0, 0, 0, loc) // 购买时刻 + 30×24h
	reserveAt := time.Date(2026, 1, 20, 12, 0, 0, 0, loc)
	commitmentExpiry := time.Date(2026, 2, 10, 0, 0, 0, 0, loc) // 晚于保修截止

	s := warranty.NewStore()
	// 登记：30 天保修，除外清单不含本次故障代码 NOISE；备件初始库存 10 件。
	must(s.RegisterProduct("P1", purchase, 30, []string{"BROKEN_SEAL"}))
	must(s.RegisterPart("PART-A", 10))
	must(s.SubmitRequest("REQ-1", "P1", "NOISE"))

	// 保修截止前成功预留 4 件，承诺到期安排在保修截止之后。
	c, err := s.Reserve("COMMIT-1", "REQ-1", "PART-A", 4, commitmentExpiry, reserveAt)
	must(err)
	fmt.Printf("reserved: id=%s qty=%d unused=%d\n", c.ID, c.Quantity, c.Unused())

	// 恰好到达保修截止时刻：资格查询显示不合格，拒绝原因只有“过保”。
	elig, err := s.Evaluate("REQ-1", warrantyEnd)
	must(err)
	fmt.Printf("eligible at warranty end: %v reasons=%v\n", elig.Eligible, elig.Reasons)

	// 原承诺尚未到自己的到期时刻，仍有效：实物 10、有效占用 4、可承诺 6。
	ps, err := s.PartStatus("PART-A", warrantyEnd)
	must(err)
	fmt.Printf("part at warranty end: physical=%d occupied=%d committable=%d\n",
		ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)

	// 用新承诺编号再预留：产品已过保，返回 ErrIneligible，不增加占用。
	_, err = s.Reserve("COMMIT-2", "REQ-1", "PART-A", 1, commitmentExpiry, warrantyEnd)
	fmt.Println("reserve after warranty end:", err)
	fmt.Println("reserve matches ErrIneligible:", errors.Is(err, warranty.ErrIneligible))
	ps, err = s.PartStatus("PART-A", warrantyEnd)
	must(err)
	fmt.Printf("part after rejected reserve: physical=%d occupied=%d committable=%d\n",
		ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)

	// 使用旧承诺是另一种操作：只看承诺自己的到期时刻、取消状态和未用数量，
	// 不再判断保修资格。在保修截止时刻用新的使用编号领取 2 件，成功。
	u, err := s.Use("USE-1", "COMMIT-1", 2, warrantyEnd)
	must(err)
	fmt.Printf("used: id=%s commitment=%s qty=%d\n", u.ID, u.CommitmentID, u.Quantity)

	// 承诺已用 2 件、未用 2 件，状态仍为 active。
	rv, err := s.RequestView("REQ-1", warrantyEnd)
	must(err)
	d := rv.Commitments[0]
	fmt.Printf("commitment after use: qty=%d used=%d unused=%d status=%s\n",
		d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status)
	// 实物剩余 8、有效占用 2、可承诺仍为 6（8−2）。
	ps, err = s.PartStatus("PART-A", warrantyEnd)
	must(err)
	fmt.Printf("part after use: physical=%d occupied=%d committable=%d\n",
		ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)

	// 到达承诺自身的到期时刻：用尚未成功过的使用编号再领 1 件。
	_, err = s.Use("USE-2", "COMMIT-1", 1, commitmentExpiry)
	fmt.Println("use at commitment expiry:", err)
	fmt.Println("use matches ErrCommitmentClosed:", errors.Is(err, warranty.ErrCommitmentClosed))

	// 不新增使用记录：明细仍是原数量 4、已用 2、未用 2，状态变为 expired。
	rv, err = s.RequestView("REQ-1", commitmentExpiry)
	must(err)
	d = rv.Commitments[0]
	fmt.Printf("commitment after expiry: qty=%d used=%d unused=%d status=%s\n",
		d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status)
	// 未用的 2 件释放占用：实物仍剩 8 件且全部可承诺；已领走的 2 件不回库存。
	ps, err = s.PartStatus("PART-A", commitmentExpiry)
	must(err)
	fmt.Printf("part after expiry: physical=%d occupied=%d committable=%d\n",
		ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
```

输出：

```text
reserved: id=COMMIT-1 qty=4 unused=4
eligible at warranty end: false reasons=[warranty_expired]
part at warranty end: physical=10 occupied=4 committable=6
reserve after warranty end: warranty: request is not eligible: [warranty_expired]
reserve matches ErrIneligible: true
part after rejected reserve: physical=10 occupied=4 committable=6
used: id=USE-1 commitment=COMMIT-1 qty=2
commitment after use: qty=4 used=2 unused=2 status=active
part after use: physical=8 occupied=2 committable=6
use at commitment expiry: warranty: commitment is no longer active: commitment "COMMIT-1" expired at 2026-02-10 00:00:00 +0000 UTC
use matches ErrCommitmentClosed: true
commitment after expiry: qty=4 used=2 unused=2 status=expired
part after expiry: physical=8 occupied=0 committable=8
```

## 规则要点

- 产品、备件、请求编号重复登记一律报错（`ErrDuplicateID`）且保留原记录。
- 购买时刻不得晚于当前时刻；保修天数为正整数；保修期自购买时刻起按每天
  二十四小时计算，自保修截止时刻起算过保；命中除外代码必须拒绝；过保与除外
  同时成立时拒绝原因会列出两项。
- 可承诺数量 = 剩余实物库存 − 所有有效承诺的未用数量；库存不足、未知请求或
  备件、不合格请求都不占用数量。
- 到期释放是不可撤销的结果：在针对已知备件的库存查询、针对已知请求的承诺
  明细查询、新预留的库存核算或针对已知承诺的新使用判断中，凡本次当前时刻
  达到承诺到期时刻，就确认该承诺（仅本次操作涉及者）已到期。一经确认，未用
  占用永久释放（不增加实物库存、不抹去已用数量），之后的库存查询、请求查询
  和新预留始终不再计算其未用数量，即使传入更早时刻也不能再次占用已释放给
  其他请求的数量；新使用一律返回 ErrCommitmentClosed。到期时刻前尚未确认
  失效的承诺仍按已有规则使用，不要求调用时刻递增；资格仍按本次时刻判断，不
  会用较大的历史时刻替换本次时刻。幂等重试只取回旧结果，空编号、非正数量等
  参数无效或引用对象不存在而提前失败的调用，都不确认承诺到期。
- 两类查询仍保留过期承诺的原数量、已用数量、未用数量和到期时刻；未取消的
  到期记录持续显示 expired，已取消的记录仍显示 canceled；从仓库取回当前
  承诺后按较早时刻查看，状态也保持到期。重复取消不再释放数量。
- 预留与使用均支持幂等重试：同编号同内容返回首次结果，换内容报
  `ErrConflict`；成功的使用在承诺取消或到期后重试仍返回原结果。
- 已成功预留的编号原样重试（承诺编号、请求编号、备件编号、数量、到期时刻一致；
  本次当前时刻不属于提交内容，到期时刻按实际时刻比较，换时区表示仍算一致）始终
  返回首次预留成功时的完整承诺（已用数量为零、未取消）：即使请求后来过保、库存
  不足，或承诺已分批使用、全部使用、取消、到期，甚至本次当前时刻等于或晚于原
  到期时刻，也不报参数错误。重试只是取回旧结果，不恢复已释放占用、不补回已扣减
  的实物库存、不重新开放承诺；当前状态仍由查询反映。
- 已成功预留的编号只要改了请求、备件、数量或到期时刻任一项即返回 `ErrConflict`，
  即使新参数本身非法（空请求、未知备件、零数量、已过去的到期时刻）也按编号冲突
  处理；冲突失败记录挂到本次指定的已知请求下，请求为空或不存在时不创建请求和
  历史。尚未成功占用的编号继续按本次参数、资格和库存判断，失败后可再次提交。
- 使用数量超过承诺未用数量时整次失败；已全部使用的承诺不再接受新使用。
- 同一承诺编号在首次成功预留前被不同内容（不同请求、备件、数量或到期时刻）
  并发抢占时，只能有一份内容成为首次承诺：与其完全相同的提交全部成功并返回
  同一份完整首次承诺（已用数量为零、未取消），另一组全部返回 `ErrConflict`，
  不能两笔都成功，也不会把后来的内容写进已成功的承诺；首次成功与编号绑定在
  同一临界区内原子完成。库存充足到能同时容纳两笔数量时，有效占用也只对应获胜
  内容（预留不扣减实物库存），承诺只归获胜请求，落败请求每次提交各留一条挂在
  本请求下的冲突失败记录（资格与库存依据均为空），获胜请求只留一条首次成功
  记录。库存恰好被首次成功预留用完时，同内容的其余并发提交仍取回首次结果，
  不报库存不足，另一笔仍报编号冲突；赛后按胜出内容重试取回首次承诺、按落败
  内容重试仍冲突，库存与归属保持原结果。
- 并发预留同一备件时，有效承诺未用总量不会超过剩余实物库存。
- 同一使用编号的并发提交只产生一次真实扣减：相同承诺编号与数量的并发调用全部
  成功并返回同一份完整使用记录，但承诺累计已用数量与备件实物库存只变化一次，
  有效占用只减少实际使用数量，可承诺数量保持原值；即使本次恰好耗尽承诺余量，
  其余相同内容的并发提交也取回首次结果，不当作新的超量使用。同一编号在首次成功
  前被两组不同内容（不同承诺，或同承诺不同数量）并发抢占时，只能有一份内容成为
  成功记录：与其相同的调用全部成功并返回同一结果，另一组全部返回 `ErrConflict`，
  不能两组各自成功；最终扣减量只对应胜出的那一份提交，落败承诺保持未使用，赛后
  按胜出内容重试取回原记录、按落败内容重试仍冲突，冲突不覆盖记录也不额外扣减。
- 历史只保存在当前仓库实例中：每个请求的记录按处理次序排列、序号严格递增，
  与提交时刻无关；成功与失败提交都留痕，同编号同内容重试不追加历史；
  冲突失败记录挂到本次提交指定的已知请求；资格或备件缺失时依据明确为空，
  不用合格或零库存替代缺失；查询返回副本，修改不影响已保存历史。
