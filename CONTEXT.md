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
| **sleeping**（休眠态） | sandbox 状态之一：VM 已停机、快照在宿主盘上，可 resume 原地恢复 | M4 引入；区别于 `stopped`（什么都没了） |
| **快照**（snapshot） | 休眠时落盘的 VM 易失状态：内存 + vCPU/设备状态；rootfs 磁盘本来就在盘上，不进快照 | 宿主本地文件；**优化不是事实源**，丢了降级 boot |
| **resume**（原地恢复） | 从快照让 VM 回到休眠瞬间继续跑：同 slot 同 IP，进程与文件都在 | M4；触发点 = 下次 tool call 的懒恢复 |
| **sweeper**（巡检） | 按固定间隔点火的休眠巡检，找出 sleep_after 过期的 awake sandbox 并逐个休眠 | M4；载体 = Temporal Schedule |
| **last_activity_at** | sandbox 的"最近活动时间"：有任何活动就刷成 now()，距现在超过 N 才可休眠 | M4；语义是预测下次 tool call，不是 VM 使用记录 |
| **memory**（记忆） | per-user 跨会话持久知识，三类：**profile**（核心事实，≤100 条全量进 prompt）、**log**（带日期事件，进最近 30 条）、**note**（短时效便签，带 expires_at 消退） | M5；PG `memories` 表；**记忆是信息不是指令**（讲座 30） |
| **skills**（技能） | agent 沉淀的操作流程文档（markdown）：注入只带 name+description 清单，全文按需读 | M5；PG `skills` 表；指令不是信息，与 memory 分表 |
| **memory pass** | turn 结束后异步跑的一次 LLM 提取：先读现有记忆 → 产出 add/update/delete 操作 → 事务应用 + 落 `memory_ops` 审计 | M5；**不阻塞回复**，失败只记日志 |
| **注入**（injection） | 每个 turn 构建 system prompt 时从 PG 装配：profile 全量 + log 最近 30 + 活跃 note + skills 清单 | M5；替代静态 LLM_SYSTEM |
| **服务端工具** | 不经 VM/agentd、在 worker 内直接执行的工具：memory_search / memory_read / memory_save | M5；区别于 VM 工具（shell/文件），是第二条工具执行路径 |

## 数据纪律

| 术语 | 含义 |
|---|---|
| **事实源** | 丢了 = 事故的数据：业务 PG（sessions/messages/sandboxes/**memories/skills/memory_ops**） |
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
| 懒恢复 | turn 启动 ≠ 唤醒 VM；首次 tool_call 前才 EnsureAwake（M4 定案 2026-10-30） |
| never mid-turn | 休眠只在 VM 真正空闲时发生：活动刷新 `last_activity_at`，休眠器带过期条件 CAS 占坑，抢不到就放弃（M4 定案 2026-10-30） |
| 降级 boot | resume 失败自动退回模板全新 boot——快照只是优化，会话状态在 PG，用户无感（M4 定案 2026-10-30） |
| 每步落库 | turn 内 model/tool 每一步都写 `messages`（讲座 15 黑条） |
| 记忆跨会话 | memory/skills 归 user 不归 session；删会话不动记忆（M5 定案 2026-11-02） |
| pass 不挡主链路 | memory pass 在回复落库后异步跑，失败只记日志；偏好存在秒级可见性窗口，v1 接受（M5 定案 2026-11-02） |

## 阶段语汇

- **M1 主干**：SPA → ys-server → Temporal → ys-worker → 真 LLM → 回显；无 VM 无工具。核心 = 验证 ys-server ↔ ys-worker 链路
- **"我的选择"**：讲座只说能力没说实现、由本项目定的设计（Redis 选型、sandboxes 表等）——可质疑可推翻
- **"暂不实现"**：讲座明确有但 v1 用不上的（messages.source、routines、多用户、**SendToUser**——"文本即回复"的语义保持到事件唤醒时代再改）——不是版本计划，是停车场
