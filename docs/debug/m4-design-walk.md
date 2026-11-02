---
title: M4 设计导读
subtitle: 快照休眠/恢复——五个设计问题，读透就算懂了
template: doc
outline: left
---
读法：先扫 §1 的文件地图建立骨架，然后按 §2–§6 五个设计问题逐个读。
每个问题给：答案 → 代码落点（可点击行号）→ 原文。读完能向外人讲清三板斧就算达标。

## 1. 文件地图：M4 的代码都住在哪

```tree
internal/
  hitsend/hitsend.go | 续期写入点①：发消息 = 用户在场
  workflow/
    sleep.go | 两个 workflow：sweeper 巡检 + 单 sandbox 休眠
    activities.go | 续期写入点② + CAS/SleepVM/起 workflow 的壳
    history.go | PG→LLM 历史投影（降级 boot 不失忆的关键）
  vmdriver/vmdriver.go | worker 与宿主的唯一通道（含 Exec 自愈）
  vmhostd/
    vm.go | VM 生命周期全部：boot/sleepVM/resumeVM/reconcile
    fc.go | firecracker API 最小客户端（pause/snapshot/load）
  store/pg/queries.sql | CAS 与扫描的 SQL 事实源
```

阅读顺序建议：`queries.sql`（机制最短）→ `sleep.go`（流程主干）→ `vm.go`（宿主侧重活）→ 其余按问题跳转。

## 2. 设计问题一：为什么"续期"能替代"锁"？

**答案**：任何活动（发消息、工具执行）都把 `last_activity_at` 刷成 now()。
休眠器不查"有没有 turn 在跑"（这个状态没法原子地查），而是抢一行原子 SQL——
抢不到，说明 N 分钟内必有活动，放弃就是。

```sql src=internal/store/pg/queries.sql lines=108-113
```

续期写入点只有两处，语义各管一半：

```go src=internal/hitsend/hitsend.go lines=43-47
```

```go src=internal/workflow/activities.go lines=185-191
```

Submit 管"用户还在说话"，ExecTool 管"VM 还在干活"。
硬约束写进了 spec：N（10min）必须大于单工具 timeout（60s），否则长命令期间续期会断。
验证断点（可选）：`activities.go:63` TryStartSleep——turn 进行中看 acquired=false。

## 3. 设计问题二：为什么 resume 失败也不要紧？

**答案**：Grok Bot 原则"一切状态在 server"——会话记忆在 PG，VM 只是执行现场。
所以 resume 失败的最坏结果是全新 boot：agent 从 PG 投影历史，接着聊。

```go src=internal/vmhostd/vm.go lines=42-52 hl=42
```

降级后 VM 是空的，但 agent 不失忆——历史投影在这：

```go src=internal/workflow/history.go lines=11-30
```

这一条决定了整套系统的容错姿态：快照是优化，坏了就扔（连目录一起删，防二次踩坑）。
对比 e2b 的多版本快照 + diff 上传——那是"快照必须可靠"才会需要的重工程。

## 4. 设计问题三：为什么快照存宿主本地盘，不进 PG？

**答案**：三个理由——512MB 进 PG 拖垮 backup/vacuum；resume 的读取方就是本机，
走网络延迟翻倍；按问题二的逻辑它本来就是可丢的。完整权衡在 `docs/adr/0001`。

```go src=internal/vmhostd/vm.go lines=159-180
```

代价也真实存在：用户被绑死在 `host_id` 那台机器上。宿主磁盘坏 = 快照和 VM 内文件全丢，
走问题二的降级路径。演进路线（云盘挂载 → files sync → 对象存储）写在 ADR 的 Consequences 里。

## 5. 设计问题四：为什么 resume 必须复用同一个 slot？

**答案**：guest 的 IP 是 boot 时以内核参数烧进去的，冻在内存快照里。
resume 时 guest 带着旧 IP 醒来，宿主侧必须给它同名的 tap、同一个子网。

```go src=internal/vmhostd/vm.go lines=102-102
```

```go src=internal/vmhostd/vm.go lines=185-196 hl=188
```

推论：sleeping 的 VM 不能释放 slot（`sleepVM` 不调 `ReleaseSandbox`），
reconcile 看到 sleeping 也不能按"没进程=死了"清场——`vm.go` 的 `classifySandbox`
就是这条规则的判官，单元测试 `vm_test.go` 覆盖了全部七种状态组合。

## 6. 设计问题五：为什么起 SleepWorkflow 走 client 不走 child workflow？

**答案**：e2e 实测踩出来的——父 workflow 在同一 task 里"发起 child + 立即完成"时，
child 从未被 server 创建（Initiated 事件有，Started/Failed 无，Describe 查无此行）。
改走 activity 里 client.ExecuteWorkflow，与 hitsend 起 turn 同一模式。

```go src=internal/workflow/activities.go lines=95-114
```

`sleep.go` 文件头注释记了这个坑的完整描述。
教训的通用形态：Temporal 的"魔法"（child 生命周期、重试、ReusePolicy）每个都有锋利边缘，
workflow 代码里不直观的机制，优先退到 activity + client 这种"笨"模式。

## 7. 自测：合上书能回答吗

| 问题 | 答不上就回读 |
|---|---|
| 两个续期写入点分别防什么场景？ | §2 |
| 快照丢了用户会看到什么？为什么？ | §3 |
| 为什么 sleeping 的 VM 占着 slot 不放？ | §5 |
| 想把 N 从 10min 改成 5min，动哪几处？ | §2（提示：纯配置，在 sweeper 参数里） |
| 想把快照改存对象存储，动哪几层？ | §4 + ADR-0001 Consequences |
