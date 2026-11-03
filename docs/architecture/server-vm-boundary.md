# Server / VM 职责边界（Grok Bot 架构的权威依据）

> 本文档是 ys-bot 架构的第一块基石：**什么活在 server，什么活在 VM，以及 agent 怎么被唤醒**。
> 依据：Lee Robinson（Grok Bot 负责人）Stanford CS146S 讲座 "How always-on agents work"（2026-10-08）
> 的幻灯片原文，重点是 "How does it stay always on?"（四层模型，**s00/s10**）与 "What wakes the agent"（唤醒流水线，**s11**）。
> 原始素材：`ys-learn/notes/agents/cs146s-slides/`、`cs146s-how-always-on-agents-work-transcript.txt`
> **逐张提取的全档（编号已校正，引用前先查它）：`docs/reference/cs146s-grokbot-lecture.html`**

## 系统全景（"Three parts of the system"）

```
Apps and channels              Server: runs the agent               Agent's computer: runs tools
───────────────              ───────────────────────────           ─────────────────────────────
Desktop app        ──send──▶  Entry point: checks and routes       File and connector server
Mobile and web                      (唤醒流水线的 Router)           Command runner: shell,
Slack                           ▼                                  files, screenshots
Email/webhooks/schedules        Queue: one per conversation               │
Phone calls and meetings        ▼                                         ▼
                                Model ⇄ Agent loop and tools ──tool─▶  desktop + Chrome
                                ▲  ▼ load and save                     （每活跃 agent 一屏）
                                Saved state: database and storage
└─────── live updates ──────────┘（各端收信号后回源拉取，s16 Watching）
```

定调原文："**The agent runs on the server, uses tools on its own computer,
and every app shows the same conversation.**"

注意：VM 内的服务写的是 "**File and connector server**"——连接器本体在 VM 内
（要访问 app 登录态），但凭证签发/保管在 server 侧（s27）。衔接机制讲座未细说，见未决问题。

## 四层模型（原图直译）

```
┌─────────────────────────────────────────────────────────────┐
│ Triggers（wake it up）                                       │
│   Your message          Routine at 8:00        …（共 7 种）   │
└──────────────┬───────────────────────────────┬──────────────┘
               ▼                               ▼
┌─────────────────────────────────────────────────────────────┐
│ Agent process（runs a turn, then stops）                     │
│   "Loads state, works, saves each step"                      │
│   · 被触发才启动 · 空闲 2 分钟就停 · 每步落库                  │
└──────────────┬──────────────────────────────────────────────┘
               │ （仅当 tool 需要时）
               ▼
┌─────────────────────────────────────────────────────────────┐
│ Agent's computer（sleeps when idle）                         │
│   Firecracker microVM，一用户一台                             │
│   · 睡着：saved to disk（快照）                               │
│   · 懒恢复："Restored when a tool needs it"                   │
│   · 不需要就全程不醒："Stays asleep: this routine didn't need it"│
└──────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ Saved state（always on the server）                          │
│   "Database and storage: conversations, memory, routines, skills" │
└─────────────────────────────────────────────────────────────┘
```

"When nothing is happening… any message or trigger can bring it back."

## 职责边界总表

### Server —— 智能与状态（丢了 = 事故）

| 职责 | 依据 |
|---|---|
| agent loop（model/tools 循环），跑一个 turn 就停 | 四层图 agent process 层 |
| 会话状态、消息 DB（通知只是"有变化"空信号） | s16；四层图 saved state |
| **memory、routines、skills 的权威存储（DB + storage）** | 四层图 saved state 层（盖棺） |
| 会话队列、每会话同时一个 turn、唤醒路由/优先级 | s16、s11、s18 |
| prompt 组装（Profile ≤100 facts/4000 字符 + Log 30 条注入） | s28、s35 |
| memory 更新 pass（每 turn 后加/换事实，只能删它见过的） | s35 |
| 12 种工具的定义与调度（Shell/Files 经 VM 执行） | s19 |
| auto review（独立模型审危险操作，15s 无响应即拦截） | s27 |
| 凭证（密码/OAuth token 走环境变量，永不进 VM/模型） | s27 |
| durable execution（每 turn/helper 一个 workflow） | s17 |

### VM（agent 的电脑）—— 环境（丢了 = 环境重置，不是失忆）

| 职责 | 依据 |
|---|---|
| File server：文件、app 连接、实时桌面画面 | s12 |
| Command runner：shell、文件、截图、点击 | s12 |
| 虚拟屏幕 + Chrome（每活跃 agent 一屏） | s12 |
| 跨会话保留：工作文件、已装工具、浏览器登录态 | s12 黑条 |
| 知识文件（skill/memory）的**工作副本**——plain files 形态，agent 自己读写 | s08-5、s35 |
| 休眠：文件同步云存储 → 内存+磁盘快照 → 停机（**"never mid-turn"**） | s12 |

### 形态澄清：文件 vs 存储位置

s08 说 "skills and memories are stored as **plain files** that the agent reads and updates itself"，
四层图说 saved state（含 memory/skills）"**always on the server**"。
两者兼容的唯一解读：**形态是 plain files，权威存储在 server（database and storage）**；
VM 运行时持有同步副本供 agent 高速读写，休眠时同步回云存储（s12 休眠第 1 步）。

## 唤醒流水线（s11 "What wakes the agent"）

always-on ≠ 进程一直跑，而是**任何东西都能把它叫醒**。agent 是纯事件驱动的：

```
7 种唤醒源                Router              会话队列            出口判断
─────────────────  →  去重 + 选对会话  →  one turn at a time →  Anything worth saying?
1. 你在任何端发消息                                              ├─ yes → 发到正确的位置
2. routine（schedule/webhook）                                   └─ no  → Stay quiet
3. Slack mention / DM / thread
4. 电话或会议
5. 另一个 agent
6. 后台任务完成
7. 定时 check-in
```

三条设计要点：

1. **Router 去重**：同一事件可能到达多次（webhook 重试、多端同步），靠唯一 ID 幂等丢弃；
   "选对会话"指 Slack 线程的 @ 路由到已有会话而非开新会话——决定 agent 有没有上下文。
2. **醒了 ≠ 要说话**（"Anything worth saying?"）：后台任务完成、check-in 无异常都可以安静待机。
   与 s19 "Quiet is allowed" 互为表里；缓解 open problem "When to speak"（s40）。
3. **每次唤醒记录来源**（图底部）：agent 能分辨"用户亲口要求 / routine 触发 / 外部服务触发"，
   信任级别与回复位置（回线程还是私聊）随之不同（呼应 s27 来源标记）。

### 排队规则（s18 "Rules for handling new messages"）

```
You（1:1 聊天）──────────────── Skips the line ──▶ 打断/停止当前 running turn
                                                    （改主意场景："Actually, make it Thursday"）
Everyone else（群聊/Slack/routine/helper）
  队尾 → Helper finished → Slack reply → [3 quick messages → 1 turn] → Routine → 队头 → turn
```

三条规则：

1. **用户特权通道**：1:1 聊天里你的消息不排队，interrupts or stops the running turn
2. **其他一切 FIFO**：helper 完成、Slack 回复、routine 到点，全部排队等队头
3. **turn 的输入是"一批消息"**："The agent works on one batch of messages at a time"——
   连续快速消息合并进一个 turn，不是每条各跑一个

对 ys-bot：TurnWorkflow 输入 = turn 开始时 drain 会话队列得到的**一批消息**；
"打断" = Temporal `CancelWorkflow`（协作式取消当前 turn）→ 合并队列 → 启动新 turn。
v1 只有用户一个来源，但"打断"和"合并"从第一天就实现——它们是调试体验的核心手感。

对 ys-bot：v1 只有一个触发源（web 调试消息），但 **Router + 会话队列 + Turn 的管道形状
按此实现**——msg ID 幂等即 Router 去重，sessionID 即"选会话"；将来加 routine/webhook
只是往左接新源，主干不动。

## 三个例子走查

### 例子 1：你发消息 "帮我把 downloads 里的 csv 汇总一下"（VM 睡着）

```
1. 消息带唯一 ID 落 server DB（重发安全）→ 进会话队列
2. agent process 启动：从 server 加载 state（会话、memory、skills）
3. turn 开始：模型决定要用 Shell 工具
4. 第一个 tool call 触发 VM 懒恢复：快照原地 restore → 登录态/工具/文件还在
5. agentd 执行 shell，读 VM 里的 csv → 结果回 server
6. 回复经 SendToUser 发出；每步已落库
7. 空闲 2 分钟 → agent process 停；再过几分钟无活动 → 文件同步云存储 → 快照 → VM 停机
```

关键点：**turn 从第 2 步就开始跑了，VM 第 4 步才被需要**——纯靠 server 状态就能完成对话类工作。

### 例子 2：8:00 routine "给我今天的日程摘要"（不需要电脑）

```
1. Routine 触发器到点 → agent process 启动，加载 state
2. turn 全程在 server：读 memory（你的偏好）、调 calendar connector（OAuth token 在 server）
3. SendToUser 发出摘要
4. VM 全程没醒——"Stays asleep: this routine didn't need it"
```

关键点：**routine 可以完全不碰 VM**。VM 唤醒是有成本的（快照恢复几秒），架构特意让它懒加载。

### 例子 3：agent 自我进化（每个用户都不一样）

```
1. 某个 turn 里你说"以后汇总 csv 都用 pandas 而不是 awk"
2. turn 结束后 server 跑 memory pass：新事实写入你的 profile（server storage）
3. 下次 turn 组装 prompt 时，这条事实在 Profile 层永远注入
4. 期间 VM 可能休眠、重建、换了台新机器——记忆无损，因为权威副本从不在 VM
```

关键点：**自我进化的产物（每个用户独有的 prompt 片段/skill/memory）必须比 VM 长寿**。
这是它们权威存储在 server 的根本原因。

## 对 ys-bot 的设计约束

1. **agent process 短命化**：loop 不是一个常驻服务循环，而是"触发 → 跑 turn → 停"。
   对应我们的 TurnWorkflow：每 turn 一个 workflow，跑完即终（与 per-turn 拓扑决策一致）。
2. **VM 懒唤醒**：turn 启动 ≠ 唤醒 VM；第一次 tool call 需要时才 `Resume`。
   对应 `envdclient` 调用前的 `EnsureVMawake` activity。
3. **权威状态零依赖 VM**：四层存储各就各位——业务 PG（sessions/messages/sandboxes，对应
   Grok Bot 的 "database"）、文件存储（skills/prompt 片段/快照，对应 "storage"）、
   Redis（协调态：队列/幂等/锁/信号，全部可丢可重建）、Temporal PG（执行账本，应用不查）。
   VM 快照只保环境。v1 允许偷懒：文件同步云存储可省（快照含磁盘），但边界按本文档画。
4. **两套空闲超时**：agent process 级（Grok Bot 是 2 分钟）与 VM 级（"最后活动几分钟后"）分开配置。
   v1：turn 结束即释放（workflow 完成），VM 空闲 5 分钟休眠（参照 e2b v2 默认）。
   **休眠只在 turn 间隙发生（"never mid-turn"）**：idle timer 在会话有进行中 turn 时不得触发——
   实现上 = turn 开始时抑制 timer、turn 结束重新计时（而非仅靠 tool call 刷新）。
5. **唤醒管道主干不变**：Router（msg ID 幂等 + 选会话）→ Redis 会话队列 → TurnWorkflow。
   v1 只接 web 消息一个源；routine/webhook 等新源只接入口，不改主干。

## 未决问题（标注推断，待验证）

- [ ] server 侧 saved state 的具体形态：讲座说 "database and storage"，推断 = DB（结构化：会话、队列、profile facts）+ storage（文件：skills、日志、快照、同步副本）。未区分哪些进 DB 哪些进文件存储。
- [ ] VM 内知识文件副本与 server 的同步方向/冲突处理：推断 = VM 写、休眠时单向同步上云；未提及冲突。
- [ ] "2 idle minutes" 是 agent process 的超时还是示意值：按原文采纳为参考值。
- [ ] connector 的位置拆分：全景图显示 "File and connector server" 在 VM 内，s27 要求凭证不进 VM。推断 = 连接器逻辑在 VM、凭证签发在 server（短期/受限凭据下发），具体机制未知。v1 无 connector，仅记录。
