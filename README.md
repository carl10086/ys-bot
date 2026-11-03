# ys-bot

基于 **Grok Bot 架构**实现的 always-on agent 沙箱系统：agent loop 在 server 侧，
每个沙箱是一个 Firecracker microVM（agent 的"电脑"），不活跃时快照休眠、调用时原地恢复。

灵感与架构依据来自 Grok Bot 负责人 Lee Robinson 在 Stanford CS146S 的分享：
**[How always-on agents work](https://www.youtube.com/watch?v=L1OBRtfPS34)**。
本项目按讲座中的四层模型亲手实现，架构逐条对照见
[`docs/architecture/server-vm-boundary.md`](docs/architecture/server-vm-boundary.md)。

## 核心架构

```
React SPA ──HTTP/SSE──▶ ys-server（API + 内嵌 SPA）
                            │  起 turn / 打断 / 幂等
                            ▼
                       Temporal（持久执行，每步 durable）
                            │
                            ▼
                       ys-worker（agent loop + 沙箱编排）
                            │  Connect-RPC（typed 流式）
                            ▼
                Firecracker microVM ── agentd（薄工具进程：shell / 文件）

        PostgreSQL = 业务事实源（会话 / 消息 / 记忆 / 沙箱）
        Redis      = 仅协调态（队列、锁、变更信号），可丢
```

### 四层模型（讲座原图的实现）

| 层 | 职责 | 本项目的实现 |
|---|---|---|
| Triggers | 把 agent 叫醒 | 用户消息（HTTP 入口），事件驱动而非进程常驻 |
| Agent process | 跑一个 turn，然后停 | Temporal workflow：加载状态 → 工作 → 每步落库 |
| Agent's computer | 工具执行环境，空闲即睡 | Firecracker microVM，一用户一台；快照到磁盘，工具需要时才恢复 |
| Saved state | 永远在 server | PG（会话/消息/记忆）+ 文件存储（快照） |

"Always-on" 不是进程一直跑，而是**任何东西都能把它叫醒，且叫醒后上下文完好**。

### 职责边界：什么活在 server，什么活在 VM

- **Server = 智能与状态（丢了 = 事故）**：agent loop、会话与消息、memory/skills 的权威存储、
  会话队列（每会话同时只有一个 turn）、prompt 组装、工具定义与调度、模型凭证。
- **VM = agent 的电脑（丢了 = 环境重置，不是失忆）**：shell/文件执行、跨会话保留的工作文件与已装工具。
  VM 内没有 agent loop，也没有模型凭证——它是可重建的一次性环境。

## 关键机制

### hit-send 协议：幂等、打断、合并

一次发送的完整路径：**解析 → 幂等落库 → 入队 → 打断旧 turn → 起新 turn**，顺序不可换。

- **幂等**：`msg_id` 由客户端生成，`INSERT ... ON CONFLICT(msg_id) DO NOTHING` 兜底，
  网络重试/重复提交不产生重复消息，不需要额外的幂等窗组件。
- **打断**：turn 进行中发新消息 → Temporal 协作式取消当前 turn → 合并队列 → 启动新 turn，
  回复始终响应最新指令（对应讲座的用户特权通道："Actually, make it Thursday"）。
- **合并**：turn 的输入是 drain 队列得到的**一批消息**，连续快速消息合进一个 turn，不是每条各跑一个。

### Durable execution：崩溃从断点恢复

每个 turn 是一个 Temporal workflow，每一步（模型调用、工具执行）都是落库的活动记录。
`kill -9` worker 后重启，进行中的 turn 由新 worker 接管、从断点继续跑完；
server 重启也不丢任何会话历史——事实源在 PG，进程只是无状态的执行者。

### 快照休眠 / 懒唤醒

沙箱空闲后：内存 + 磁盘快照到文件存储 → 停机。**只在 turn 中首个 tool_call 真正需要 VM 时**
才按 `snapshot_ref` 原地恢复；纯对话的 turn 全程不醒。休眠绝不发生在 turn 中途（never mid-turn）。
宿主重启后由 vmhostd 启动对账：本地无进程的沙箱标记 stopped、回收资源，PG 档案卡与真实状态保持一致。

### 长期记忆

每个 turn 结束后跑独立的 memory pass workflow：加载本轮消息 → 提取值得保留的事实 → 写入记忆库。
写入阶段用 per-user advisory lock 串行，防止并发 turn 互相覆盖；后续 turn 按需注入相关记忆，
注入失败降级为空而不是卡死对话。被打断的 turn 也参与提取——用户说过的偏好不因打断而丢失。

## 存储设计

| 存储 | 角色 | 放什么 | 丢了会怎样 |
|---|---|---|---|
| 业务 PG | 事实源 | sessions / messages / sandboxes / 记忆 | 事故 |
| 文件存储 | 事实源 | 快照、知识文件 | 事故 |
| Redis | 协调态 | 会话队列 / turn 锁 / 变更信号 | 可从 PG 重建，无害 |
| Temporal PG | 执行账本 | workflow 事件历史 | 应用不查，Temporal 自管 |

业务表统一约定，直接回应"高并发下编号/多租户"的经典坑：

- **uuidv7 单列代理主键**，应用生成，不用复合自然键。
- **每组编号用父行计数器**：`sessions.last_seq` 经 `UPDATE … RETURNING` 同事务发放会话内序号——
  行锁天然串行化同组插入，回滚不留空号（PG sequence 给不了每组编号且回滚留洞，`MAX+1` 并发报废）。
- **`user_id` 全表冗余、无 FK**：按用户直查免 JOIN；子表 user_id 从父行派生（SQL 内 SELECT），不靠应用传参。
- **PG 记"应该存在什么"（档案卡），Redis 记"此刻谁活着"（值班表）**——低频事实与高频易逝状态分家。

## 关键技术选型

| 决策 | 结论 | 理由 |
|---|---|---|
| 仓库形态 | 单 module monorepo（Go） | 没有组件需要独立发版；temporal、coder 同体量项目均如此 |
| 进程拓扑 | `ys-server` / `ys-worker` 分离 | worker 重启不掉 API，API 发版不打断 workflow |
| server ↔ VM 协议 | **Connect-RPC**（proto + buf codegen） | e2b envd 同款：process/filesystem 全是有类型的流；unary 调用兼容 HTTP/1.1，curl 可调试 |
| agent 框架 | `charm.land/fantasy` 薄层，关在适配接口后 | 重框架的 session/workflow 与 Temporal 职责冲突；薄层可整体替换 |
| 调试界面 | Vite + React SPA，`go:embed` 进 ys-server | 单二进制分发，无需单独部署前端 |

## 跑起来

```bash
cd deploy && docker compose up -d          # Temporal + PG + Redis
cd web && npm install && npm run build     # 构建 SPA（go:embed 需要 dist/）
cd .. && go run ./cmd/ys-server            # :8080，浏览器直接打开
LLM_API_KEY=xxx go run ./cmd/ys-worker     # OpenAI 兼容端点；LLM_BASE_URL/LLM_MODEL 可选
```

## 目录结构

```
cmd/ys-server/      HTTP API + 内嵌 SPA（对外入口）
cmd/ys-worker/      Temporal worker + agent loop + 沙箱编排
cmd/agentd/         VM 内薄工具进程（shell / 文件）
cmd/vmhostd/        宿主机代理：VM 生命周期 + 启动对账
internal/workflow/  Temporal workflows + activities（turn、memory pass、休眠清扫）
internal/agent/     agent loop（fantasy 适配层，业务代码不直接碰 fantasy 类型）
internal/store/     PG（事实源）+ Redis（协调态）
internal/vmdriver/  VM 驱动接口 + Firecracker 实现
proto/              Connect-RPC 协议定义（buf generate → internal/gen/）
web/                Vite + React 调试界面
```

## 文档

- **架构基石**：[`docs/architecture/server-vm-boundary.md`](docs/architecture/server-vm-boundary.md)——server/VM 职责边界、唤醒流水线、排队规则，含例子走查
- **数据模型**：[`docs/architecture/data-model.md`](docs/architecture/data-model.md)——四类存储角色、统一建表约定
- **设计 spec 与意图**：[`docs/ys-powers/`](docs/ys-powers/)

## 参考实现（源码精读）

- 沙箱调度 / 快照 / envd 通信 → [e2b-dev/runtime](https://github.com/e2b-dev/runtime)
- Temporal agent loop 写法 → [temporal-community/temporal-ai-agent](https://github.com/temporal-community/temporal-ai-agent)
- 产品形态 / 沙箱工具 API → [kortix-ai/suna](https://github.com/kortix-ai/suna)
- Go 薄 agent 层 → [charmbracelet/fantasy](https://github.com/charmbracelet/fantasy)
