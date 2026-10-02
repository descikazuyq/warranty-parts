# 本地保修资格与备件承诺

这是一个在本机运行、可由本机程序直接调用的保修资格与备件承诺库（Go 包 `warranty`）。

## 能力

- `NewService()` 创建独立现场，所有方法可并发调用；时刻全部由调用方注入，不读系统时钟。
- 登记：`RegisterProduct`（购买时刻、保修天数、除外故障代码）、`RegisterPart`（非负初始库存）、`RegisterRequest`（引用产品与故障代码）。编号重复报错并保留原记录。
- 资格：保修期自购买时刻起按每天 24 小时计，`now >= 截止时刻` 即过保；命中除外代码拒绝；两者同时成立时列出两项原因。
- 预留 `Reserve`：按当次时刻重新判断资格；提交编号幂等，相同内容重试返回同一承诺，任一内容变化报 `ErrConflict`；可承诺数量 = 实物剩余 − 有效承诺未用总量。
- 使用 `Use`：分批、使用编号幂等，超量整次失败；`Cancel` 只释放未用量且幂等；到期（`now >= 到期时刻`）自动失效并释放余量，无需清理。
- 查询：`QueryRequest`（资格依据、拒绝原因、关联承诺）与 `QueryPart`（实物剩余、有效占用、可承诺数量、占用明细，含已取消/到期记录）。

## 使用

```bash
go test ./...
go test -race ./...
```

最小示例：

```go
s := warranty.NewService()
now := time.Now()
s.RegisterProduct("P1", now, 365, []string{"WATER"}, now)
s.RegisterPart("A", 10)
s.RegisterRequest("R1", "P1", "ELECTRIC")

cid, err := s.Reserve("submit-1", "R1", "A", 2, now.Add(7*24*time.Hour), now)
if err == nil {
    s.Use("use-1", cid, 1, now)
}
```
