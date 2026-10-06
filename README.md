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
