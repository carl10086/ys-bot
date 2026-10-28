# CONTEXT.md — ys-sandbox 领域语言

> 设计对话（2026-10-27 ~ 10-28）沉淀的统一语言。代码、文档、讨论中用这些词时，含义以此为准。
> 出处：CS146S 讲座全档 `docs/reference/cs146s-grokbot-lecture.html`（讲座序 01–37）+ 本项目架构文档。

## 核心概念

| 术语 | 含义 | 备注 |
|---|---|---|
| **turn** | agent 的一次处理单元：加载状态 → 跑 agent loop → 每步落库 → 停。输入是**一批消息**，不是一条 | 讲座 15/17；实现 = 一个 Temporal workflow（TurnWorkflow） |
| **session**（会话） | 一次对话的完整记录与组织单位；消息、队列、锁都按会话组织 | PG `sessions` 表；id = uuidv7 |
| **sandbox**（沙箱） | agent 的"电脑"：一台 Firecracker microVM，**一用户一台**；会睡眠/快照/恢复 | PG `sandboxes` 表是它的档案卡；归属方向 sandbox → user |
| **agentd** | VM 内的薄工具进程（shell/文件），经 Connect-RPC 被 server 调用 | M2 引入；VM 里**没有** agent loop 和凭证 |
| **唤醒流水线** | 7 种唤醒源 → Router 去重 → 会话队列 → "值得说吗？"判断 | 讲座 10；v1 只有 web 消息一个源，管道形状按全量实现 |

## 数据纪律

| 术语 | 含义 |
|---|---|
| **事实源** | 丢了 = 事故的数据：业务 PG（sessions/messages/sandboxes）+ 文件存储（memory/skills/快照） |
| **协调态** | 丢了可重建的数据，只放 Redis：会话队列 / turn 锁 / 变更信号。往 Redis 写永久数据是红线 |
| **执行账本** | Temporal PG 里的 workflow 事件历史；应用不查，Temporal 自管 |

## 标识符

| 字段 | 谁生成 | 规则 |
|---|---|---|
| `msg_id` | **客户端**（SPA） | uuidv4；幂等靠 PG `UNIQUE` + `ON CONFLICT`，无 Redis 幂等窗 |
| `session_id` | server | uuidv7 |
| `turn_id` | server | = Temporal workflowID：`turn:{sessionId}:{uuid}`；**没有** turn 级自增序号 |
| `seq` | server | **消息级**序号，会话内递增；SPA 回显按它排序、断档重拉 |
| `user_id` | 配置 | v1 = 常量 `local`；text 类型；不建 users 表、不加 FK |

## 行为规则

| 规则 | 内容 |
|---|---|
| 打断 | 1:1 消息不排队，CancelWorkflow 打断当前 turn 后合并队列开新 turn（讲座 17 用户特权） |
| 批量 | 快速连续消息合并进一个 turn（"3 quick messages → 1 turn"） |
| 懒恢复 | turn 启动 ≠ 唤醒 VM；首次 tool_call 前 EnsureVMAwake 才 Resume（M4） |
| never mid-turn | 休眠只在 turn 间隙发生：turn 开始抑制计时、结束重新计时（M4 设计，已推迟） |
| 每步落库 | turn 内 model/tool 每一步都写 `messages`（讲座 15 黑条） |

## 阶段语汇

- **M1 主干**：SPA → ys-server → Temporal → ys-worker → 真 LLM → 回显；无 VM 无工具。核心 = 验证 ys-server ↔ ys-worker 链路
- **"我的选择"**：讲座只说能力没说实现、由本项目定的设计（Redis 选型、sandboxes 表等）——可质疑可推翻
- **"暂不实现"**：讲座明确有但 v1 用不上的（messages.source、routines、多用户）——不是版本计划，是停车场
