# ys-bot

基于 Grok Bot 架构的 always-on agent 系统。agent loop 运行在 server 侧，工具执行在独立的
Firecracker microVM 中；沙箱空闲时快照休眠，工具需要时从快照恢复。

架构参考 Grok Bot 负责人 Lee Robinson 在 Stanford CS146S 的讲座
[How always-on agents work](https://www.youtube.com/watch?v=L1OBRtfPS34)，
逐条对照见 [docs/architecture/server-vm-boundary.md](docs/architecture/server-vm-boundary.md)。

## Features

**Durable execution**

每个 turn 是一个 Temporal workflow，模型调用与工具执行逐步持久化。worker 进程被 kill 后，
新 worker 接管并从断点继续执行；server 重启不丢失会话历史。
工具参数错误以模型可见的输出返回（模型可自行纠错），不作为 activity error 无限重试；
memory pass 等异步工作的 LLM 调用重试封顶 3 次，失败不影响 turn 回复。

**记忆**

四层结构，存储于 PostgreSQL，跨会话生效：

| 层 | 内容 | 使用方式 |
|---|---|---|
| Profile | 用户核心事实 | 每个 prompt 全量注入，上限 100 条 / 4000 字符 |
| Log | 带日期的既往事件 | 最近 30 条注入 prompt |
| Notes | 短期工作细节 | 带 TTL，过期后不再注入 |
| Everything else | 其余全部记忆 | agent 通过 `memory_search` 工具按需检索 |

每个 turn 结束后异步运行 memory pass：提取新事实、替换旧事实（只能删除本轮见过的条目），
所有变更写入 `memory_ops` 审计表。写入按用户持 advisory lock 串行；agent 也可调用
`memory_save` 主动写入。注入的记忆按信息处理，不作为指令执行。

**沙箱生命周期**

- 空闲数分钟后自动快照（内存 + 磁盘）并停机；turn 进行中不休眠。
- 仅当工具调用需要 VM 时从快照恢复（同 slot、同 IP，VM 内文件保留）；纯对话不启动 VM。
- 快照损坏或丢失时自动降级为全新启动，turn 照常完成。
- 宿主重启后 vmhostd reconcile：无本地进程的沙箱标记 stopped 并释放资源，休眠快照保留。

**寻址与探活**

sandbox → 宿主映射存于 Redis catalog，调用方不记录宿主地址，先查 catalog 再连接。
vmhostd 心跳 key TTL 30 秒，过期后映射自动失效；另提供 `/health` 进程级检查。
catalog 为可丢协调态，丢失后由 vmhostd 重新注册重建。

**消息协议**

发送路径：幂等落库 → 入队 → 打断当前 turn → 启动新 turn。
`msg_id` 由客户端生成，`ON CONFLICT DO NOTHING` 去重；turn 进行中收到新消息时协作式取消并合并输入；
一个 turn 处理 drain 队列得到的一批消息。

**安全边界**

模型凭证仅存在于 server 侧，VM 内无 agent loop 与凭证。VM 位于宿主私有 tap 网段，
不分配公网地址；外部访问经 vmhostd 反向代理，宿主仅暴露一个受控入口。

## Architecture

```
React SPA ──HTTP/SSE──▶ ys-server（API + 内嵌 SPA）
                            │  起 turn / 打断 / 幂等
                            ▼
                       Temporal（每个 turn 一个 durable workflow）
                            │
                            ▼
                       ys-worker（agent loop + 沙箱编排）
                            │  查 Redis catalog → Connect-RPC 经 vmhostd 入 VM
                            ▼
                Firecracker microVM ── agentd（薄工具进程：shell / 文件）

        PostgreSQL = 事实源（会话 / 消息 / 记忆 / 沙箱）    Redis = 协调态（队列 / 锁 / catalog）
```

对应讲座的四层模型：触发（消息入口）→ agent process（Temporal workflow，跑一个 turn 后结束）
→ agent's computer（Firecracker microVM，空闲休眠）→ saved state（PG 与文件存储，仅在 server）。

职责划分：server 负责 agent loop、会话与记忆存储、prompt 组装、工具调度、凭证；
VM 只负责 shell / 文件执行。VM 可重建，丢失不影响会话与记忆。

## 技术选型

| 项 | 选择 | 理由 |
|---|---|---|
| 沙箱 | Firecracker microVM | 硬件级隔离，支持快照/恢复 |
| 持久执行 | Temporal | workflow 级持久化，取消/定时/重试内建 |
| server ↔ VM | Connect-RPC（proto + buf） | typed 流式 API；unary 兼容 HTTP/1.1，可用 curl 调试 |
| agent 框架 | charm.land/fantasy，隔离在适配层后 | 避免框架的 session/workflow 与 Temporal 职责重叠 |
| 进程 | ys-server / ys-worker 分离，单 module | worker 重启不影响 API，API 发版不打断 workflow |
| 调试界面 | Vite + React，go:embed 进 ys-server | 单二进制分发 |

## Quickstart

```bash
cd deploy && docker compose up -d          # Temporal + PG + Redis
cd web && npm install && npm run build     # 构建 SPA（go:embed 需要 dist/）
cd .. && go run ./cmd/ys-server            # :8080，浏览器直接打开
LLM_API_KEY=xxx go run ./cmd/ys-worker     # OpenAI 兼容端点；LLM_BASE_URL/LLM_MODEL 可选
```

## 目录结构

```
cmd/ys-server/      HTTP API + 内嵌 SPA
cmd/ys-worker/      Temporal worker + agent loop + 沙箱编排
cmd/agentd/         VM 内工具进程（shell / 文件）
cmd/vmhostd/        宿主机代理：VM 生命周期、反向代理、reconcile
internal/workflow/  Temporal workflows + activities（turn、memory pass、休眠清扫）
internal/agent/     agent loop（fantasy 适配层）
internal/store/     PG + Redis
internal/vmdriver/  VM 驱动接口 + Firecracker 实现
proto/              Connect-RPC 协议定义（buf generate → internal/gen/）
web/                Vite + React 调试界面
```

## Documentation

- [server / VM 职责边界](docs/architecture/server-vm-boundary.md)：四层模型、唤醒流水线、排队规则
- [数据模型](docs/architecture/data-model.md)：存储角色划分、建表约定
- [设计 spec](docs/ys-powers/) 与 [决策记录](docs/adr/)

## References

- [How always-on agents work — Lee Robinson, Stanford CS146S](https://www.youtube.com/watch?v=L1OBRtfPS34)
- 沙箱调度 / 快照 / catalog：[e2b-dev/runtime](https://github.com/e2b-dev/runtime)
- Temporal agent loop：[temporal-community/temporal-ai-agent](https://github.com/temporal-community/temporal-ai-agent)
- 产品形态 / 沙箱工具 API：[kortix-ai/suna](https://github.com/kortix-ai/suna)
- Go agent 薄层：[charmbracelet/fantasy](https://github.com/charmbracelet/fantasy)
