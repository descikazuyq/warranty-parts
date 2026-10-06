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

// 7. 使用明细：按承诺编号列出组成已用数量的成功使用记录（无需当前时刻），
//    按使用编号升序，数量之和即该承诺的已用数量；取消或到期后仍可查询。
us, _ := s.CommitmentUsages("COMMIT-1")
```

## 先提交保修请求、稍后补登产品资料

上面的典型流程先登记产品、再提交请求，但这只是习惯顺序，并非硬性前提：
`SubmitRequest` 提交时**不校验产品是否已登记**，产品编号可以指向一个尚未登记
的产品，只要故障代码非空，请求就会保存成功。这里要区分两件事：

- **请求已保存**：`SubmitRequest` 成功只代表请求编号、产品编号与故障代码已落库，
  之后用 `Request` 随时能取回原产品编号和故障代码；它不代表资格已经成立。
- **已经符合保修资格**：资格必须在产品资料存在时，按当次当前时刻、购买时刻、
  保修期限与除外清单判断。产品缺失期间，直接查资格（`Evaluate`）、按请求查看
  资格与承诺（`RequestView`），以及参数合法的预留（`Reserve`）一律返回
  `ErrNotFound`——既不算合格，也不算不合格或零库存，且不产生承诺、不占用库存。

产品资料可以稍后补登：`RegisterProduct` 直接作用于**原请求**，无需删除、替换
或再次提交请求，此前因产品缺失而失败的预留也可以**沿用原来的承诺编号和同样的
预留内容**（请求、备件、数量、到期时刻不变，且到期时刻仍晚于当次当前时刻）重新
提交并成功。该编号之所以能继续使用，是因为它此前从未预留成功、失败不占用编号；
一旦编号已经预留成功，重试仍遵守原来的幂等规则：同内容取回首次承诺快照，改内容
报 `ErrConflict`。

注意：**补登产品并不保证原请求一定合格**。资格仍按登记的购买时刻（不得晚于
当次当前时刻）、保修期限（当前时刻未达到保修截止时刻）和除外清单（故障代码未
命中）逐项判断；补登的条款不满足时，预留照样返回 `ErrIneligible`。

下面是可直接采用的完整示例，全程使用同一个仓库实例与固定时刻：先登记十件备件
库存，再提交一张关联未登记产品、故障代码为 `NOISE` 的请求；产品缺失期间完成
各项查询与一次必然失败的三件预留，随后补登合格的产品资料，沿用原请求与原承诺
编号完成预留，并通过历史看到失败与成功两条记录各自的依据。

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/warranty-parts/warranty"
)

func main() {
	s := warranty.NewStore()
	must := func(err error) {
		if err != nil {
			panic(err) // 正常分支遇到错误立即停止
		}
	}

	// 固定时刻：2026-01-01 购买，保修期三十天；两次操作的当前时刻都在保修期内。
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	firstAt := purchase.AddDate(0, 0, 10)  // 2026-01-11，产品缺失期间的当前时刻
	secondAt := purchase.AddDate(0, 0, 11) // 2026-01-12，补登资料后的当前时刻
	expiry := purchase.AddDate(0, 0, 40)   // 2026-02-10，晚于两次当前时刻

	// 先登记十件备件库存；产品 P1 暂不登记。
	must(s.RegisterPart("PART-A", 10))

	// 先提交保修请求：产品尚未登记也能保存，只要给出产品编号、故障代码非空。
	must(s.SubmitRequest("REQ-1", "P1", "NOISE"))

	// 产品缺失期间，查询原请求本身仍可取回原产品编号和故障代码（请求已保存）。
	req, err := s.Request("REQ-1")
	must(err)
	fmt.Println(req.ProductID, req.FaultCode) // P1 NOISE

	// 直接查资格、按请求查看资格与承诺：均返回 ErrNotFound，
	// 即“资格依据缺失”，而不是合格、不合格或零库存。
	_, err = s.Evaluate("REQ-1", firstAt)
	fmt.Println(errors.Is(err, warranty.ErrNotFound)) // true
	_, err = s.RequestView("REQ-1", firstAt)
	fmt.Println(errors.Is(err, warranty.ErrNotFound)) // true

	// 参数合法的三件预留（数量为正、到期时刻晚于当次当前时刻，
	// 因此失败原因确实只是缺少产品资料）：返回 ErrNotFound，不产生承诺。
	_, err = s.Reserve("COMMIT-1", "REQ-1", "PART-A", 3, expiry, firstAt)
	fmt.Println(errors.Is(err, warranty.ErrNotFound)) // true
	_, err = s.Commitment("COMMIT-1")
	fmt.Println(errors.Is(err, warranty.ErrNotFound)) // true（失败不占用编号）

	// 失败后没有承诺：实物剩余十件、有效占用零件、可承诺十件。
	ps, err := s.PartStatus("PART-A", firstAt)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable) // 10 0 10

	// 历史留下一条失败记录：类别 product_not_found；即使备件已登记，
	// 资格依据与库存依据也均为 nil——缺失不能被解释成零库存。
	h, err := s.RequestHistory("REQ-1")
	must(err)
	fmt.Println(len(h), h[0].Success, h[0].Error)                // 1 false product_not_found
	fmt.Println(h[0].Eligibility == nil, h[0].StockBasis == nil) // true true

	// 补登产品资料：购买时刻早于当次时刻，三十天保修期尚未结束，除外清单不含 NOISE。
	must(s.RegisterProduct("P1", purchase, 30, []string{"BROKEN_SEAL"}))

	// 原请求继续使用：再次查看应显示合格，且尚无承诺——
	// 无需删除、替换或再次提交请求。
	rv, err := s.RequestView("REQ-1", secondAt)
	must(err)
	fmt.Println(rv.Eligibility.Eligible, len(rv.Commitments)) // true 0

	// 沿用前次失败的承诺编号和三件预留内容（请求、备件、数量、到期时刻不变，
	// 到期时刻仍晚于当次时刻）：提交成功。编号能沿用，是因为此前从未预留成功；
	// 已有成功记录的编号重试仍遵守原来的幂等规则。
	c, err := s.Reserve("COMMIT-1", "REQ-1", "PART-A", 3, expiry, secondAt)
	must(err)
	fmt.Println(c.ID, c.Quantity, c.Used) // COMMIT-1 3 0

	// 预留不扣实物：实物剩余仍是十件，有效占用变为三件，可承诺变为七件。
	ps, err = s.PartStatus("PART-A", secondAt)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable) // 10 3 7

	// 补登和成功预留之后，旧失败记录仍保留且不会被补填依据，后面追加成功记录；
	// 成功记录的库存依据是新增占用之前的十、零、十。
	h, err = s.RequestHistory("REQ-1")
	must(err)
	fmt.Println(len(h))                                            // 2
	fmt.Println(h[0].Seq, h[0].Success, h[0].Error)                // 1 false product_not_found
	fmt.Println(h[0].Eligibility == nil, h[0].StockBasis == nil)   // true true（旧记录不补填）
	fmt.Println(h[1].Seq, h[1].Success, h[1].Eligibility.Eligible) // 2 true true
	b := h[1].StockBasis
	fmt.Println(b.PhysicalRemaining, b.ActiveOccupied, b.Committable) // 10 0 10
}
```

关键预期结果：产品缺失期间请求可正常取回（`P1 NOISE`），资格查询、请求视图和
三件预留都返回 `ErrNotFound`，账目保持 `10 / 0 / 10`，历史第一条为
`product_not_found` 且两类依据均为 `nil`；补登合格条款后原请求显示合格且无承诺，
沿用 `COMMIT-1` 与原预留内容成功，账目变为 `10 / 3 / 7`；历史增至两条，旧失败
记录原样保留、成功记录的库存依据为新增占用前的 `10 / 0 / 10`。

## 过保后继续使用既有承诺

保修资格的期限与承诺预留的期限是两个互相独立的时刻，分别约束不同环节，
资格变化不会追溯已经预留成功的备件：

- **保修截止时刻只决定“新的首次预留”能否成功。** 每次首次预留都按当次调用
  传入的当前时刻重新判断资格：在保修截止时刻之前且未命中除外代码才合格。
  产品过保之后用**新的承诺编号**预留，即使备件仍有余量，也返回
  `ErrIneligible`，不创建承诺、不增加任何占用。
- **承诺自身的到期时刻约束“已经预留成功的备件”的后续使用。** 成功预留之后的
  分批使用（`Use`）只检查这笔承诺自己的到期时刻、取消状态和未用数量是否足够，
  不再回看产品保修资格。因此产品后来过保，既不会自动取消承诺，也不会提前释放
  未用占用：在承诺到期或被取消之前，未用数量一直计入有效占用，可承诺数量相应
  减少；拿着**旧承诺编号**领取备件是另一种操作，仍正常扣减未用数量与实物库存。
- **承诺到期只释放未用占用，不收回已领实物。** 当前时刻达到承诺到期时刻后，
  新的使用一律返回 `ErrCommitmentClosed`，不新增使用记录、不再扣减实物；未用
  数量永久移出有效占用、重新计入可承诺数量。已经成功领走的实物早已扣减，不会
  因到期回到库存。承诺明细（原数量、已用、未用、到期时刻）继续保留，状态显示
  expired（若曾取消则显示 canceled）。

下面是可直接采用的完整示例：固定购买时刻、三十天保修期、十件初始库存，故障
代码不在除外清单中；保修截止前为同一请求成功预留四件，承诺到期时刻安排在保修
截止时刻之后，依次跨过保修截止与承诺到期两个时刻。

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/warranty-parts/warranty"
)

func main() {
	s := warranty.NewStore()
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	// 固定时刻：2026-01-01 购买，保修期三十天，保修截止为 2026-01-31 00:00 UTC。
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	warrantyEnd := purchase.AddDate(0, 0, 30)  // 2026-01-31 00:00 UTC
	reserveAt := purchase.AddDate(0, 0, 20)   // 2026-01-21 00:00 UTC，保修截止之前
	commitExpiry := purchase.AddDate(0, 0, 40) // 2026-02-10 00:00 UTC，保修截止之后

	// 登记：故障代码 NOISE 不在除外清单中；备件初始库存十件。
	must(s.RegisterProduct("P1", purchase, 30, []string{"BROKEN_SEAL"}))
	must(s.RegisterPart("PART-A", 10))
	must(s.SubmitRequest("REQ-1", "P1", "NOISE"))

	// 保修截止前成功预留四件，承诺到期晚于保修截止。
	_, err := s.Reserve("COMMIT-1", "REQ-1", "PART-A", 4, commitExpiry, reserveAt)
	must(err)

	// 恰好到达保修截止时刻：资格不合格，拒绝原因只有过保一项。
	rv, err := s.RequestView("REQ-1", warrantyEnd)
	must(err)
	fmt.Println(rv.Eligibility.Eligible)       // false
	fmt.Printf("%v\n", rv.Eligibility.Reasons) // [warranty_expired]

	// 原承诺不受影响：实物十件、有效占用四件、可承诺六件。
	ps, err := s.PartStatus("PART-A", warrantyEnd)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable) // 10 4 6

	// 用新承诺编号再预留：按当次时刻判资格，返回 ErrIneligible，不增加占用。
	_, err = s.Reserve("COMMIT-2", "REQ-1", "PART-A", 2, commitExpiry, warrantyEnd)
	fmt.Println(errors.Is(err, warranty.ErrIneligible)) // true
	ps, err = s.PartStatus("PART-A", warrantyEnd)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable) // 10 4 6

	// 使用旧承诺是另一种操作：用新的使用编号领取两件，成功（不回看保修资格）。
	_, err = s.Use("USE-1", "COMMIT-1", 2, warrantyEnd)
	must(err)

	// 承诺已用两件、未用两件；实物剩八件、有效占用两件、可承诺仍为六件。
	rv, err = s.RequestView("REQ-1", warrantyEnd)
	must(err)
	d := rv.Commitments[0]
	fmt.Println(d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status) // 4 2 2 active
	ps, err = s.PartStatus("PART-A", warrantyEnd)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable) // 8 2 6

	// 到达承诺自身的到期时刻：用尚未成功过的使用编号领取一件，被关闭拒绝。
	_, err = s.Use("USE-2", "COMMIT-1", 1, commitExpiry)
	fmt.Println(errors.Is(err, warranty.ErrCommitmentClosed)) // true

	// 不新增使用记录、不再扣实物；未用两件释放占用，备件仍剩八件且都可承诺。
	ps, err = s.PartStatus("PART-A", commitExpiry)
	must(err)
	d = ps.Details[0]
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)            // 8 0 8
	fmt.Println(d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status) // 4 2 2 expired
}
```

三个时刻的账目（实物剩余 / 有效占用 / 可承诺）依次是：保修截止时刻
`10 / 4 / 6`，新承诺 `COMMIT-2` 因过保被拒后保持不变；用 `USE-1` 领取两件后
变为 `8 / 2 / 6`，承诺明细为 `4 / 2 / 2 active`；到达承诺到期时刻，`USE-2`
返回 `ErrCommitmentClosed`，账目变为 `8 / 0 / 8`，明细保留 `4 / 2 / 2 expired`。
到期释放的只是未用的两件占用，已经领走的两件实物不会因此回到库存。

## 取消已部分使用的承诺：释放预留占用，不收回实物

`Cancel` 关闭一笔承诺时，处理的是这笔承诺**尚未使用的预留占用**，而不是把整笔
原定数量还回库存。要看懂取消前后的账目，先把四个数量分开：

- **实物剩余（`PhysicalRemaining`）**：仓库里实际剩下的备件，只被成功的
  `Use` 扣减；预留（`Reserve`）不扣实物，取消也不增加实物。
- **单笔承诺的未用数量（明细中的 `RemainingQuantity`）**：这笔承诺的原定数量
  减去它自己的已用数量，是保留在该承诺明细上的数量事实，取消、到期都不会抹掉。
- **有效占用（`ActiveOccupied`）**：全部**有效（active）**承诺的未用数量之和；
  已取消（canceled）或已到期（expired）承诺的未用数量不再计入。
- **可承诺数量（`Committable`）**：实物剩余减去有效占用，即还能被新的合格请求
  预留的数量。

对一笔已经部分使用的承诺调用 `Cancel`：

- 该承诺**尚未使用的部分**立即移出有效占用、转入可承诺数量，随后可以被其他请求
  重新预留；这才是“取消释放”的数量。取消只释放四件时，不能把原定六件全部算作
  释放数量——另外两件此前已经被成功领取。
- 已经成功领取的实物早已离开仓库，**不会因为取消而退回**：实物剩余保持不变。
- 承诺记录并不删除：取消后的请求查询（`RequestView`）与备件查询（`PartStatus`）
  仍展示它的原定数量、已用数量、未用数量和到期时刻，状态为 `canceled`。明细里的
  未用数量只是一笔**历史数量事实**，既不代表这笔承诺还能继续领取，也不能再与其他
  有效承诺的未用数量相加当作当前占用——当前占用只数 `active` 的记录。
- 取消后用**新的使用编号**向这笔承诺领取，一律返回 `ErrCommitmentClosed`，
  账目与已用数量保持不变。已经成功过的使用编号原样重试仍按幂等规则取回首次使用
  记录，不会再次扣减，更不是取消后又领走了备件。
- 取消不影响其他承诺：别的请求（或同一请求的其他承诺）的数量、归属和到期时刻
  保持原值；重复取消不再释放数量。

下面是可直接采用的完整示例：同一个仓库实例、同一个保修期内的操作时刻，全部承诺
到期时刻相同且晚于这些操作，故障代码均未命中除外。登记十件同种备件，为两张不同
请求分别预留六件和三件，再从六件承诺中领取两件；随后取消六件承诺，用第三个合格
请求把释放后全部的可承诺数量预留完；最后先演示对已取消承诺的新使用被
`ErrCommitmentClosed` 拒绝，再演示已有成功使用的原样重试只取回旧记录。

```go
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

	// 固定时刻：2026-01-01 购买、保修三十天；所有操作都取保修期内的同一时刻，
	// 全部承诺到期时刻相同且晚于这些操作，故障代码 NOISE 不命中除外清单。
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := purchase.AddDate(0, 0, 10)    // 2026-01-11，保修期内
	expiry := purchase.AddDate(0, 0, 40) // 2026-02-10，晚于全部操作时刻

	// account 输出备件账目的三项数量：实物剩余 / 有效占用 / 可承诺。
	account := func(label string) {
		ps, err := s.PartStatus("PART-A", now)
		must(err)
		fmt.Printf("%-8s physical=%d occupied=%d committable=%d\n",
			label, ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)
	}

	// 登记：同种备件初始库存十件；两张不同的合格请求。
	must(s.RegisterProduct("P1", purchase, 30, []string{"BROKEN_SEAL"}))
	must(s.RegisterPart("PART-A", 10))
	must(s.SubmitRequest("REQ-1", "P1", "NOISE"))
	must(s.SubmitRequest("REQ-2", "P1", "NOISE"))

	// 两张请求分别预留六件和三件。
	c6, err := s.Reserve("COMMIT-6", "REQ-1", "PART-A", 6, expiry, now)
	must(err)
	c3, err := s.Reserve("COMMIT-3", "REQ-2", "PART-A", 3, expiry, now)
	must(err)
	fmt.Printf("reserve : %s qty=%d used=0 ; %s qty=%d used=0\n",
		c6.ID, c6.Quantity, c3.ID, c3.Quantity)

	// 从六件承诺中领取两件：扣减实物库存与该承诺的未用数量。
	u, err := s.Use("USE-1", "COMMIT-6", 2, now)
	must(err)
	fmt.Printf("use     : %s commitment=%s quantity=%d\n", u.ID, u.CommitmentID, u.Quantity)

	// 取消前：实物剩余八件（十减已领两件），有效占用七件（未用四件加三件），
	// 可承诺一件。
	account("before:")

	// 取消六件承诺：释放的是它尚未使用的四件预留占用，不是原定六件；
	// 已经领取的两件实物不会退回库存。
	cc, err := s.Cancel("COMMIT-6", now)
	must(err)
	fmt.Printf("cancel  : %s qty=%d used=%d unused=%d canceled=%v\n",
		cc.ID, cc.Quantity, cc.Used, cc.Unused(), cc.Canceled)

	// 取消后：实物仍是八件，有效占用降为三件（只剩 COMMIT-3），
	// 可承诺增为五件（原有一件余量 + 取消释放的四件）。
	account("after:")

	// 请求查询继续展示原承诺：原数量六、已用两件、未用四件，状态 canceled。
	rv1, err := s.RequestView("REQ-1", now)
	must(err)
	d := rv1.Commitments[0]
	fmt.Printf("REQ-1   : %s %d/%d/%d %s\n",
		d.CommitmentID, d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status)

	// 备件查询的明细同样保留这笔取消记录；另一张请求的三件承诺仍为 active。
	// 明细按承诺编号排序，COMMIT-3 在前。
	ps, err := s.PartStatus("PART-A", now)
	must(err)
	for _, d := range ps.Details {
		fmt.Printf("part    : %s %s %d/%d/%d %s\n",
			d.CommitmentID, d.RequestID,
			d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status)
	}

	// 另一张请求的三件承诺继续有效：数量、归属与到期时刻保持原值。
	rv2, err := s.RequestView("REQ-2", now)
	must(err)
	d = rv2.Commitments[0]
	fmt.Printf("REQ-2   : %s %d/%d/%d %s owner=%s expiry-kept=%v\n",
		d.CommitmentID, d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity,
		d.Status, d.RequestID, d.Expiry.Equal(expiry))

	// 用一个新的合格请求把五件可承诺数量全部预留：来自原有一件余量与
	// 取消释放的四件，释放的数量可以供其他请求使用。
	must(s.SubmitRequest("REQ-3", "P1", "NOISE"))
	c5, err := s.Reserve("COMMIT-5", "REQ-3", "PART-A", 5, expiry, now)
	must(err)
	fmt.Printf("reserve : %s request=%s qty=%d used=%d\n",
		c5.ID, c5.RequestID, c5.Quantity, c5.Used)
	account("full:")

	// 用尚未成功过的使用编号向已取消承诺领取一件：返回 ErrCommitmentClosed。
	_, err = s.Use("USE-2", "COMMIT-6", 1, now)
	closed := errors.Is(err, warranty.ErrCommitmentClosed)
	expectErr(err, warranty.ErrCommitmentClosed)
	fmt.Println("closed  :", closed)

	// 失败后账目与已用数量保持不变：仍是八件实物、八件有效占用、可承诺为零。
	account("still:")
	rv1, err = s.RequestView("REQ-1", now)
	must(err)
	d = rv1.Commitments[0]
	fmt.Printf("REQ-1   : %s %d/%d/%d %s\n",
		d.CommitmentID, d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status)

	// 已有成功使用原样重试：取回 USE-1 的旧记录，不再扣减，
	// 不是取消后又领走了备件。
	u, err = s.Use("USE-1", "COMMIT-6", 2, now)
	must(err)
	fmt.Printf("retry   : %s commitment=%s quantity=%d\n", u.ID, u.CommitmentID, u.Quantity)
	account("final:")
}
```

程序输出：

```text
reserve : COMMIT-6 qty=6 used=0 ; COMMIT-3 qty=3 used=0
use     : USE-1 commitment=COMMIT-6 quantity=2
before:  physical=8 occupied=7 committable=1
cancel  : COMMIT-6 qty=6 used=2 unused=4 canceled=true
after:   physical=8 occupied=3 committable=5
REQ-1   : COMMIT-6 6/2/4 canceled
part    : COMMIT-3 REQ-2 3/0/3 active
part    : COMMIT-6 REQ-1 6/2/4 canceled
REQ-2   : COMMIT-3 3/0/3 active owner=REQ-2 expiry-kept=true
reserve : COMMIT-5 request=REQ-3 qty=5 used=0
full:    physical=8 occupied=8 committable=0
closed  : true
still:   physical=8 occupied=8 committable=0
REQ-1   : COMMIT-6 6/2/4 canceled
retry   : USE-1 commitment=COMMIT-6 quantity=2
final:   physical=8 occupied=8 committable=0
```

结果对应说明（账目中的三项依次为实物剩余 / 有效占用 / 可承诺）：

- 两笔预留完成、再从 `COMMIT-6` 领取两件后，`before:` 为 `8 / 7 / 1`：实物从
  十件降到八件是因为确实领走了两件；有效占用七件等于六件承诺未用的四件加三件
  承诺的三件；可承诺只剩一件。
- 取消 `COMMIT-6` 的返回是 `6 / 2 / 4` 且 `canceled=true`：释放的是它尚未使用
  的**四件**预留占用，不是原定六件——另外两件此前已经领走、实物也已扣减，取消
  不会把它们还回库存。因此 `after:` 中实物仍是八件，有效占用从七件降为三件
  （只剩 `COMMIT-3` 的三件），可承诺从一件增为五件（原有一件余量加释放的四件）。
- 两类查询都保留原承诺：`REQ-1` 视图与备件明细都显示 `COMMIT-6 6/2/4
  canceled`。这四件未用数量只是保留的数量事实：不能再领取，也不计入有效占用，
  更不能与仍有效的三件相加；`REQ-2` 的 `COMMIT-3 3/0/3 active` 在数量、归属与
  到期时刻上保持原值。
- 新的合格请求 `REQ-3` 把五件可承诺数量全部预留为 `COMMIT-5` 后，`full:` 为
  `8 / 8 / 0`：这五件由原有一件余量和取消释放的四件组成，说明释放出的数量确实
  可以供其他请求使用。
- 尚未成功过的使用编号 `USE-2` 向已取消承诺领取一件：`closed: true`
  （`ErrCommitmentClosed`），`still:` 保持 `8 / 8 / 0`，`COMMIT-6` 仍是
  `6 / 2 / 4 canceled`，账目与已用数量都没有变化。
- 成功过的 `USE-1` 原样重试只取回首次的使用记录（`retry: USE-1 COMMIT-6 2`），
  `final:` 仍为 `8 / 8 / 0`：幂等重试不再扣减实物，不能把它描述成取消后又领走
  了备件。

## 故障代码除外按完整字符串精确匹配

除外清单在 `RegisterProduct` 时随产品资料登记；判断资格时，把请求提交的故障
代码与清单条目做**完整字符串比较**，只有与其中某一条**逐字符完全相同**才算
命中。匹配规则没有任何模糊空间：

- **区分大小写**：`BROKEN_SEAL` 与 `broken_seal` 是两个不同的代码，小写写法
  不命中大写条目。
- **首尾空格属于代码内容**：`" BROKEN_SEAL "`（首尾各一个空格）与
  `"BROKEN_SEAL"` 不相等、不命中；字符串中的每个字符都原样参与比较。
- **不做任何规范化或部分匹配**：提交请求（`SubmitRequest`）与判断资格
  （`Evaluate`、`RequestView`，以及 `Reserve` 内部的资格判断）都不会替调用方
  转成大写、删掉首尾空格，也不做前缀、子串等部分匹配。调用方若需要统一的规范
  形式，必须在提交前自行处理。
- **空除外清单与清单里的空字符串是两回事**：传入 `nil` 或长度为零的切片表示
  清单为空，不按任何故障代码拒保；清单中一旦包含空字符串条目，
  `RegisterProduct` 立即返回 `ErrInvalidParam` 且不保存产品。请求侧同理：
  `SubmitRequest` 的故障代码为空字符串时返回 `ErrInvalidParam`、不保存请求。
- 另外要分清“**请求保存成功**”与“**符合保修资格**”：三张故障代码互不相同的
  请求都可以保存成功，是否命中除外只在按当次时刻判断资格时才见分晓。

下面是可直接采用的完整示例：同一个仓库实例中登记购买时刻已到、保修三十天、
除外清单只有 `BROKEN_SEAL` 的产品，另登记十件备件；先演示空清单与空字符串
条目的区别、空故障代码请求被拒，再提交故障代码分别为 `BROKEN_SEAL`、
`broken_seal` 和首尾各带一个空格的 `BROKEN_SEAL` 的三张请求，于保修期内
固定时刻查询资格，并用三个不同的承诺编号各预留一件。预期失败（命中除外导致的
`ErrIneligible` 等）作为显式分支校验；其他操作一旦遇到意外错误就立即停止，
不会继续输出后面的“成功结果”。

```go
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
	// expectErr 用于预期失败分支：错误必须恰好是目标原因，
	// 否则同样立即停止，不把真正的意外错误当成“预期拒绝”略过。
	expectErr := func(err error, target error) {
		if !errors.Is(err, target) {
			panic(fmt.Sprintf("want %v, got %v", target, err))
		}
	}

	// 固定时刻：2026-01-01 购买，保修三十天；查询与预留都取保修期内的同一时刻。
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := purchase.AddDate(0, 0, 10)    // 2026-01-11 00:00 UTC，保修期内
	expiry := purchase.AddDate(0, 0, 40) // 2026-02-10 00:00 UTC，晚于当前时刻

	// 空除外清单（nil 与长度为零的切片等价）：没有任何除外条目，不按故障代码拒保。
	must(s.RegisterProduct("P0", purchase, 30, nil))

	// 清单里出现空字符串：参数不合法，首次登记返回 ErrInvalidParam，产品不保存。
	err := s.RegisterProduct("PX", purchase, 30, []string{"BROKEN_SEAL", ""})
	expectErr(err, warranty.ErrInvalidParam)
	_, err = s.Product("PX")
	expectErr(err, warranty.ErrNotFound) // 产品确实没有落库

	// 正式登记：购买时刻已到、保修三十天，除外清单只有精确的 BROKEN_SEAL；备件十件。
	must(s.RegisterProduct("P1", purchase, 30, []string{"BROKEN_SEAL"}))
	must(s.RegisterPart("PART-A", 10))

	// 故障代码为空字符串：提交即返回 ErrInvalidParam，请求不保存。
	err = s.SubmitRequest("REQ-BAD", "P1", "")
	expectErr(err, warranty.ErrInvalidParam)
	_, err = s.Request("REQ-BAD")
	expectErr(err, warranty.ErrNotFound)

	// 三张请求的故障代码是三个不同的字符串，只有第一个与登记条目逐字符完全相等。
	codes := []string{"BROKEN_SEAL", "broken_seal", " BROKEN_SEAL "}
	reqIDs := []string{"REQ-1", "REQ-2", "REQ-3"}
	for i, code := range codes {
		must(s.SubmitRequest(reqIDs[i], "P1", code))
	}

	// 三张请求全部保存成功：保存只校验编号与非空故障代码，不判断资格。
	// %q 给字符串加上引号，首尾空格在取回结果中清晰可见，三个输入不会看起来一样。
	for _, id := range reqIDs {
		r, err := s.Request(id)
		must(err)
		fmt.Printf("%s saved fault=%q\n", id, r.FaultCode)
	}

	// 保修期内固定时刻查资格：REQ-1 不合格、命中除外，拒绝原因只有
	// fault_code_excluded；另外两张合格、未命中除外且没有拒绝原因。
	for _, id := range reqIDs {
		rv, err := s.RequestView(id, now)
		must(err)
		e := rv.Eligibility
		fmt.Printf("%s fault=%q eligible=%v excluded=%v reasons=%v\n",
			id, e.FaultCode, e.Eligible, e.Excluded, e.Reasons)
	}

	// 用三个不同的承诺编号为三张请求各预留一件同种备件，到期时刻都晚于当前时刻。
	commitIDs := []string{"COMMIT-1", "COMMIT-2", "COMMIT-3"}
	for i, id := range reqIDs {
		_, err := s.Reserve(commitIDs[i], id, "PART-A", 1, expiry, now)
		if i == 0 {
			// 预期失败分支：精确命中除外代码，返回 ErrIneligible。
			expectErr(err, warranty.ErrIneligible)
			continue
		}
		must(err) // 另外两张保修期内且未命中除外，预留必须成功
	}

	// 被拒的编号没有产生承诺：失败不占用编号，也不占用库存。
	_, err = s.Commitment("COMMIT-1")
	expectErr(err, warranty.ErrNotFound)

	// 预留不扣实物：实物仍为十件，两张成功承诺各占一件，可承诺为八件。
	ps, err := s.PartStatus("PART-A", now)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)
}
```

程序输出（注意 `%q` 下第三个故障代码首尾的空格）：

```text
REQ-1 saved fault="BROKEN_SEAL"
REQ-2 saved fault="broken_seal"
REQ-3 saved fault=" BROKEN_SEAL "
REQ-1 fault="BROKEN_SEAL" eligible=false excluded=true reasons=[fault_code_excluded]
REQ-2 fault="broken_seal" eligible=true excluded=false reasons=[]
REQ-3 fault=" BROKEN_SEAL " eligible=true excluded=false reasons=[]
10 2 8
```

结果说明：

- 三张请求都**保存成功**，取回的故障代码原样保留大小写与首尾空格；保存成功
  本身不代表保修合格。
- `REQ-1` 的原代码 `BROKEN_SEAL` 与除外条目逐字符相等：不合格、命中除外，
  拒绝原因只有 `fault_code_excluded`；`COMMIT-1` 预留返回 `ErrIneligible`，
  查承诺得到 `ErrNotFound`，没有承诺、没有库存占用。
- `REQ-2`（小写）与 `REQ-3`（首尾带空格）都不命中：合格、`excluded=false`、
  没有拒绝原因，`COMMIT-2`、`COMMIT-3` 各成功预留一件。
- 最终备件账目为实物十件、有效占用两件、可承诺八件（`10 / 2 / 8`）：预留不扣
  实物，库存只被两笔成功承诺各占用一件。
- 空清单产品 `P0` 正常登记；含空字符串条目的 `PX` 返回 `ErrInvalidParam` 且
  不落库；空故障代码请求 `REQ-BAD` 同样返回 `ErrInvalidParam`、不落库。

## 首次预留被拒绝后，可承诺数量为何可能增加

一次首次预留被拒绝，不代表库存账目原地不动：被拒绝的只是**本次新承诺**，
而同种备件上的**旧承诺**可能恰好被这次提交确认到期。合法提交（参数合法、
引用资料齐全）的 `Reserve` 在核算库存之前，会先按本次当前时刻确认该备件下
已到期的承诺：未取消、尚未确认且到期时刻已达到的旧承诺被永久标记到期，
**未用占用立即释放、重新计入可承诺数量**。随后才判断资格——即使资格判断
最终拒绝了本次预留（如故障代码命中除外清单，返回 `ErrIneligible`），到期
确认已经完成且不可撤销。于是就出现看似矛盾的结果：本次没有创建任何新承诺、
没有新增占用，可承诺数量却比提交前**增加**了。增加的不是本次申请的数量，
而是旧承诺释放的未用余量；已领走的实物不会返还，实物剩余保持不变。

要把失败历史与数量变化对应起来，看两点：

- **失败记录的库存依据是释放后的账目。** 资格或库存判断阶段的失败记录
  （如 `ineligible`），其库存依据快照取自到期确认之后、新增占用之前，
  因此体现的是旧承诺释放后的实物剩余、有效占用与可承诺数量，而不是空依据，
  也不是释放前的旧账。
- **提前失败是另一回事。** 空编号、非正数量、到期时刻未晚于当前时刻等参数
  非法的提交，在校验阶段就返回 `ErrInvalidParam`，**不确认任何承诺到期**；
  对应历史记录为 `invalid_param`，资格依据与库存依据均为空（nil）。两类
  失败都留下记录、按处理顺序排列，且失败均不占用承诺编号。

下面的完整示例全程使用同一个仓库实例与固定时刻：登记十件备件，以及购买时间
已到、保修期未结束的产品；先为未命中除外代码的请求成功预留四件，并在到期前
领取两件，得到实物剩余八件、有效占用两件、可承诺六件的账目。另一个请求关联
同一产品，但故障代码精确命中登记的除外清单。在旧承诺恰好到期的时刻，先提交
一次零数量预留（提前失败、不确认到期），再用尚未成功过的新承诺编号提交一次
完全合法的一件预留（到期时刻晚于本次时刻、引用资料齐全）：返回
`ErrIneligible`，新编号查询不到承诺，但旧承诺被这次提交确认到期、释放未用
两件，可承诺数量增为八件。示例特意**不在到期时刻先查询库存**——库存查询同样
会确认到期，那样就说不清释放到底来自哪次调用了；改用不确认到期的
`Commitment` 取回承诺记录，直接观察到期标记在两次提交之间的变化。

```go
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
	// expectErr 用于预期拒绝：错误必须恰好是目标原因，否则同样立即停止，
	// 不把真正的意外错误当成“预期拒绝”略过。
	expectErr := func(err error, target error) {
		if !errors.Is(err, target) {
			panic(fmt.Sprintf("want %v, got %v", target, err))
		}
	}

	// 固定时刻：2026-01-01 购买，保修三百六十五天，示例全程保修期未结束。
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reserveAt := purchase.AddDate(0, 0, 10)    // 2026-01-11，预留与领取的时刻
	commitExpiry := purchase.AddDate(0, 0, 40) // 2026-02-10，旧承诺的到期时刻
	lateExpiry := purchase.AddDate(0, 0, 50)   // 2026-02-20，新申请给出的到期时刻

	// 登记：十件备件；产品购买时刻已到、保修期未结束，除外清单只有 BROKEN_SEAL。
	// 两张请求关联同一产品：REQ-1 未命中除外，REQ-2 精确命中。
	must(s.RegisterPart("PART-A", 10))
	must(s.RegisterProduct("P1", purchase, 365, []string{"BROKEN_SEAL"}))
	must(s.SubmitRequest("REQ-1", "P1", "NOISE"))
	must(s.SubmitRequest("REQ-2", "P1", "BROKEN_SEAL"))

	// 为 REQ-1 成功预留四件，并在到期前领取两件。
	_, err := s.Reserve("COMMIT-1", "REQ-1", "PART-A", 4, commitExpiry, reserveAt)
	must(err)
	_, err = s.Use("USE-1", "COMMIT-1", 2, reserveAt)
	must(err)

	// 到期前的账目：实物剩余八件、有效占用两件、可承诺六件。
	ps, err := s.PartStatus("PART-A", reserveAt)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable) // 8 2 6

	// 来到旧承诺恰好到期的时刻。先为除外请求提交零数量预留：
	// 参数非法提前失败，不确认任何承诺到期。
	_, err = s.Reserve("COMMIT-ZERO", "REQ-2", "PART-A", 0, lateExpiry, commitExpiry)
	expectErr(err, warranty.ErrInvalidParam)

	// 仓库中的旧承诺记录仍未标记到期：提前失败没有确认到期。
	old, err := s.Commitment("COMMIT-1")
	must(err)
	fmt.Println(old.Expired) // false

	// 再用尚未成功过的新编号提交合法预留：数量为正、到期时刻晚于本次时刻、
	// 引用资料齐全，但故障代码精确命中除外清单，返回 ErrIneligible。
	_, err = s.Reserve("COMMIT-2", "REQ-2", "PART-A", 1, lateExpiry, commitExpiry)
	expectErr(err, warranty.ErrIneligible)

	// 失败不占用编号：新编号查询不到承诺，没有新增占用。
	_, err = s.Commitment("COMMIT-2")
	expectErr(err, warranty.ErrNotFound)

	// 这次合法提交在核算库存前确认了旧承诺到期：未用两件被释放。
	old, err = s.Commitment("COMMIT-1")
	must(err)
	fmt.Println(old.Expired) // true

	// 本次没有创建新承诺，可承诺数量却增加了：实物仍为八件，
	// 有效占用降为零，可承诺增为八件。
	ps, err = s.PartStatus("PART-A", commitExpiry)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable) // 8 0 8

	// 旧承诺明细保留原数量四件、已用两件、未用两件，状态 expired；
	// 已领走的两件实物不会返还。
	d := ps.Details[0]
	fmt.Println(d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status) // 4 2 2 expired

	// REQ-2 的两条失败历史按处理顺序保留，失败均不占用承诺编号。
	h, err := s.RequestHistory("REQ-2")
	must(err)
	fmt.Println(len(h))                                          // 2
	fmt.Println(h[0].Seq, h[0].Success, h[0].Error)              // 1 false invalid_param
	fmt.Println(h[0].Eligibility == nil, h[0].StockBasis == nil) // true true
	fmt.Println(h[1].Seq, h[1].Success, h[1].Error)              // 2 false ineligible
	e := h[1].Eligibility
	fmt.Println(e.Eligible, e.Excluded, e.Reasons) // false true [fault_code_excluded]
	b := h[1].StockBasis
	fmt.Println(b.PhysicalRemaining, b.ActiveOccupied, b.Committable) // 8 0 8
}
```

程序输出：

```text
8 2 6
false
true
8 0 8
4 2 2 expired
2
1 false invalid_param
true true
2 false ineligible
false true [fault_code_excluded]
8 0 8
```

结果对应说明（账目中的三项依次为实物剩余 / 有效占用 / 可承诺）：

- 预留四件、领取两件后，到期前账目为 `8 / 2 / 6`：实物只被成功领取的两件
  扣减，旧承诺未用的两件仍计入有效占用。
- 零数量预留在参数校验阶段返回 `ErrInvalidParam`，属于提前失败：不确认
  旧承诺到期，取回 `COMMIT-1` 记录可见 `Expired` 仍为 `false`；历史留下
  第一条记录 `invalid_param`，资格依据与库存依据均为空。
- 合法的一件预留返回 `ErrIneligible`：本次没有创建新承诺（`COMMIT-2` 查询
  不到），没有新增占用；但这次合法提交在核算库存前确认了旧承诺到期，
  `COMMIT-1` 的 `Expired` 变为 `true`，未用两件被释放。账目变为
  `8 / 0 / 8`——可承诺数量从六件增为八件，增加的正是旧承诺释放的余量；
  旧承诺明细保留 `4 / 2 / 2 expired`，已领走的两件实物不会返还。
- `REQ-2` 的历史按处理顺序保留两条失败记录：第一条 `invalid_param`、两类
  依据均为空；第二条 `ineligible`，资格依据显示命中除外
  （`excluded=true`、原因只有 `fault_code_excluded`），库存依据为
  `8 / 0 / 8`——体现旧承诺释放**之后**的数量，而不是空依据或释放前的
  `8 / 2 / 6`。这也说明到期确认来自这次合法预留本身，而非之前的查询或
  提前失败的提交。

## 取回承诺记录后按时刻查看状态：只是显示，不等于仓库已释放占用

`Commitment` 按承诺编号取回的是一条**记录副本**，对它调用 `Status(now)` 只是
把这条记录按调用方给定的时刻换算成显示状态，全程不触碰仓库。因此要区分两件
条件不同、后果也不同的事：

- **显示已到期（`Status` 返回 expired）**：只要手中记录的取消标记、到期确认
  标记或传入时刻满足条件就算出来——传入时刻达到到期时刻即显示 expired，已
  取消的记录始终显示 canceled。这是纯本地的换算结果：不确认仓库中的承诺到期，
  不释放未用占用，不改变任何账目，也不会写回手中的副本。
- **仓库确认到期（释放占用）**：只在针对已知备件的库存查询（`PartStatus`）、
  针对已知请求的承诺明细查询（`RequestView`）、新预留的库存核算（`Reserve`）
  或针对已知承诺的新使用判断（`Use`）中，本次当前时刻达到承诺到期时刻时才
  发生。一经确认不可逆：未用占用永久释放、重新计入可承诺数量，已领走的实物
  不会回到库存，之后即使传入更早时刻查询也持续显示 expired。

调用方按需选择：只想按某个时刻**查看手中记录**的显示状态，用 `Commitment`
取回副本后调 `Status`，这不会影响仓库；要**核对当前库存与占用**（这本身会
确认到期），用 `PartStatus` 或 `RequestView`。尤其注意：`Commitment` 返回的
副本是取回时刻的快照，仓库后来确认了到期也不会回填到旧副本上——旧副本仍
保留未确认的标记，按早于到期的时刻查看仍可显示 active，因此旧副本不能替代
重新查询来判断仓库现状。释放只涉及**未用占用**：已经成功领取的实物早已扣减，
不会因到期确认回到实物库存。另外，已取消记录的状态始终为 canceled（取消
优先于到期，与传入时刻无关）；取回未知承诺编号时返回 `ErrNotFound`。

下面是可直接采用的完整示例，全程使用同一个仓库实例与固定时刻，产品始终在保、
故障代码未命中除外：登记十件备件，预留六件并在到期前领取两件；通过
`Commitment` 取得承诺副本后，按恰好到期的时刻调用 `Status` 观察显示结果，
再分别用到期前、到期时刻的库存查询对照账目变化，最后演示旧副本不随仓库
更新、已取消记录的状态与未知编号的错误。

```go
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
	// expectErr 用于预期失败分支：错误必须恰好是目标原因，否则同样立即停止，
	// 不把真正的意外错误当成“预期拒绝”略过。
	expectErr := func(err error, target error) {
		if !errors.Is(err, target) {
			panic(fmt.Sprintf("want %v, got %v", target, err))
		}
	}

	// 固定时刻：2026-01-01 购买，保修三百六十五天，示例全程产品在保、
	// 故障代码 NOISE 不命中除外清单。
	purchase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := purchase.AddDate(0, 0, 10)    // 2026-01-11，到期前的操作时刻
	expiry := purchase.AddDate(0, 0, 40) // 2026-02-10，承诺到期时刻

	// 登记十件备件，为合格请求预留六件，并在到期前领取两件。
	must(s.RegisterProduct("P1", purchase, 365, []string{"BROKEN_SEAL"}))
	must(s.RegisterPart("PART-A", 10))
	must(s.SubmitRequest("REQ-1", "P1", "NOISE"))
	_, err := s.Reserve("COMMIT-1", "REQ-1", "PART-A", 6, expiry, now)
	must(err)
	_, err = s.Use("USE-1", "COMMIT-1", 2, now)
	must(err)

	// 通过 Commitment 取回承诺副本：原数量六件、已用两件、未用四件，
	// 到期尚未被任何操作确认。
	copy1, err := s.Commitment("COMMIT-1")
	must(err)
	fmt.Println(copy1.Quantity, copy1.Used, copy1.Unused(), copy1.Expired)

	// 按恰好到期的时刻调用 Status：这只是对手中副本按给定时刻算出的显示
	// 结果，显示 expired；它不确认仓库中的承诺到期，也不释放未用占用。
	fmt.Println(copy1.Status(expiry))

	// 副本与重新取回的记录中 Expired 仍为 false，数量保持原值。
	rec, err := s.Commitment("COMMIT-1")
	must(err)
	fmt.Println(copy1.Expired, rec.Expired)
	fmt.Println(rec.Quantity, rec.Used, rec.Unused())

	// 按到期前的时刻查询库存：仍是实物八件、有效占用四件、可承诺四件，
	// 先前的状态显示没有改变这些数量。
	ps, err := s.PartStatus("PART-A", now)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)

	// 在到期时刻用 PartStatus 核对库存：这次查询确认了承诺到期，未用四件
	// 的占用被释放；实物仍为八件（已领走的两件不会回来），八件都可承诺，
	// 明细保留六件原数量、两件已用、四件未用的 expired 记录。
	ps, err = s.PartStatus("PART-A", expiry)
	must(err)
	fmt.Println(ps.PhysicalRemaining, ps.ActiveOccupied, ps.Committable)
	d := ps.Details[0]
	fmt.Println(d.OriginalQuantity, d.UsedQuantity, d.RemainingQuantity, d.Status)

	// 重新取回同一承诺：Expired 已为 true；到期确认不可逆，
	// 按到期前的时刻查看也持续显示 expired。
	rec, err = s.Commitment("COMMIT-1")
	must(err)
	fmt.Println(rec.Expired, rec.Status(now))

	// 此前保存的副本不随这次查询更新：它仍保留未确认的标记，按早于到期的
	// 时刻查看仍可显示 active——旧副本不能替代重新查询来判断仓库现状。
	fmt.Println(copy1.Expired, copy1.Status(now))

	// 未知承诺编号取回时返回 ErrNotFound。
	_, err = s.Commitment("COMMIT-UNKNOWN")
	expectErr(err, warranty.ErrNotFound)

	// 已取消记录的状态始终为 canceled，与传入时刻无关（取消优先于到期）。
	must(s.RegisterPart("PART-B", 1))
	_, err = s.Reserve("COMMIT-2", "REQ-1", "PART-B", 1, expiry, now)
	must(err)
	_, err = s.Cancel("COMMIT-2", now)
	must(err)
	canceled, err := s.Commitment("COMMIT-2")
	must(err)
	fmt.Println(canceled.Status(now), canceled.Status(expiry))
}
```

程序输出：

```text
6 2 4 false
expired
false false
6 2 4
8 4 4
8 0 8
6 2 4 expired
true expired
false active
canceled canceled
```

结果对应说明（账目中的三项依次为实物剩余 / 有效占用 / 可承诺）：

- 取回副本时承诺为 `6 / 2 / 4` 且 `Expired=false`：领取两件只扣实物，未用
  四件仍是有效占用。对副本按恰好到期的时刻调 `Status` 显示 `expired`，但这
  只是显示结果：副本与重新取回记录的 `Expired` 仍为 `false`，数量保持
  `6 / 2 / 4`，按到期前时刻查库存仍是 `8 / 4 / 4`——状态显示没有确认到期，
  也没有释放那四件占用。
- 在到期时刻用 `PartStatus` 核对库存才确认到期：账目变为 `8 / 0 / 8`，明细
  保留 `6 / 2 / 4 expired`。释放的只是未用的四件占用，已经领取的两件实物不
  会回到库存，实物仍为八件。此后重新取回记录 `Expired=true`，按到期前的时刻
  查看也持续显示 `expired`（确认不可逆）。
- 先前保存的副本不随这次查询更新：`Expired` 仍为 `false`，按早于到期的时刻
  查看仍显示 `active`。它是取回时刻的快照，不能替代重新查询来判断仓库现状。
- 未知承诺编号 `COMMIT-UNKNOWN` 取回时返回 `ErrNotFound`；已取消的
  `COMMIT-2` 无论按到期前还是到期时刻查看，状态始终为 `canceled`。

## 规则要点

- 产品、备件、请求编号重复登记一律报错（`ErrDuplicateID`）且保留原记录。
- 购买时刻不得晚于当前时刻；保修天数为正整数；保修期自购买时刻起按每天
  二十四小时计算，自保修截止时刻起算过保；命中除外代码必须拒绝；过保与除外
  同时成立时拒绝原因会列出两项。除外匹配按登记时给出的完整字符串逐字符比较：
  区分大小写，首尾空格也是代码内容，不转大写、不删空格、不做部分匹配；空除外
  清单表示不按故障代码拒保，清单中包含空字符串则产品登记返回
  `ErrInvalidParam`、不保存产品，故障代码为空字符串的请求同样返回
  `ErrInvalidParam`、不保存请求。
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
- 取消只释放该承诺**尚未使用的预留占用**：部分使用后取消，释放数量是原数量减
  已用数量（例如原定六件、已用两件只释放四件，不能把六件全部算作释放），未用
  数量移出有效占用、重新计入可承诺数量并可供其他合格请求预留；取消不增加实物
  库存，已经成功领取的实物不会退回。取消记录在请求查询与备件查询中继续保留原
  数量、已用、未用与到期时刻，状态为 canceled；这笔未用数量只是保留的数量
  事实，不能再领取，也不与仍有效承诺的未用数量相加计入当前占用。取消后的新使用
  （新使用编号）一律返回 `ErrCommitmentClosed`，账目与已用数量不变；已成功使用
  原样重试仍取回首次记录、不再次扣减；其他承诺的数量、归属与到期时刻不受影响。
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
