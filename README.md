# ys-sandbox

always-on agent 沙箱系统——按 Grok Bot 架构实现的个人项目：agent loop 在 server 侧，
每个沙箱是一个 Firecracker microVM（agent 的"电脑"），不活跃时快照休眠、调用时原地恢复。

- 意图文档：[`docs/ys-powers/intent/ys-sandbox.md`](docs/ys-powers/intent/ys-sandbox.md)
- **架构基石：[`docs/architecture/server-vm-boundary.md`](docs/architecture/server-vm-boundary.md)（server/VM 职责边界，含例子走查）**
- 数据模型（已定稿）：[`docs/architecture/data-model.md`](docs/architecture/data-model.md)
- 分阶段计划：[`docs/architecture/phasing.md`](docs/architecture/phasing.md)（当前目标 = M1 主干）
- 状态：**M1 主干已跑通**（2026-10-28 五条冒烟验收全过，见文末清单）

## 跑起来（M1）

```bash
cd deploy && docker compose up -d          # Temporal + PG + Redis
cd web && npm install && npm run build     # 构建 SPA（go:embed 需要 dist/）
cd .. && go run ./cmd/ys-server            # :8080，浏览器直接打开
LLM_API_KEY=xxx go run ./cmd/ys-worker     # OpenAI 兼容端点；LLM_BASE_URL/LLM_MODEL 可选
```

## M1 冒烟验收清单（2026-10-28 全过）

| # | 验收 | 结果 |
|---|---|---|
| 1 | 发消息 → LLM 回复出现在消息流 | ok（真 MiniMax-M2.5，回复带 turn_id） |
| 2 | 同 msg_id 重发 → `deduped:true`，消息数不变 | ok |
| 3 | turn 进行中发消息 → 旧 turn 取消、合并开新 turn | ok（"数数：1"被打断，最终回复响应了最新指令） |
| 4 | kill -9 ys-worker → 重启 → turn 从断点恢复完成 | ok（Temporal durable，新 WorkerID 接管） |
| 5 | ys-server 重启 → 历史完好 | ok（事实源在 PG） |

## 关键架构决策

| 决策 | 结论 | 理由（详见 spec） |
|---|---|---|
| 仓库形态 | **单 module monorepo**（一个 go.mod） | 拆多 module（go.work）的唯一理由是组件独立发版/被外部 import（e2b 如此）；temporal、coder 这种体量都是单 module |
| 进程拓扑 | `cmd/` 多二进制：`ys-server` / `ys-worker` | worker 与 API 生命周期不同（worker 重启不掉 API，API 发版不打断 workflow）——flintlock/temporal/coder 均如此 |
| agent loop 位置 | **server 侧**；VM 里只有薄工具进程（shell/文件） | Grok Bot 原版架构：VM 可重建，会话状态全在 server |
| 持久执行 | Temporal | loop 每步 durable，崩溃从断点恢复 |
| 沙箱底座 | Firecracker（Linux/KVM） | 快照/恢复一等能力、硬件级隔离 |
| agent 框架 | `charm.land/fantasy`（薄层，关在适配接口后，预期替换） | 重框架（adk-go/eino）的 session/workflow 与 Temporal 职责冲突 |
| server ↔ VM 协议 | **Connect-RPC**（proto + buf codegen，typed 流式 API） | e2b envd 同款：process/filesystem 全是有类型的流；unary 调用仍兼容 HTTP/1.1 + curl 可调试 |
| 调试界面 | `web/` Vite+React，`go:embed` 进 ys-server | coder `site/site_embed.go` 同款做法 |
| 私有代码 | 全在 `internal/`，**不放 `pkg/`** | `pkg/` 的含义是"可被外部 import"；v1 没有外部消费者 |
| VM 驱动 | 接口与实现分目录：`vmdriver/driver.go` + `firecracker/` + `mock/` | 六边形最小债务：开发/部署都在 Linux 用 firecracker；mock 只做测试替身 |

## 目录结构

```
cmd/ys-server/    HTTP API + 内嵌 SPA（对外入口）
cmd/ys-worker/    Temporal worker + 编排器 + VM 驱动
internal/agent/   agent loop（fantasy 适配层）
internal/workflow/  Temporal workflows + activities
internal/orchestrator/  沙箱生命周期：创建/休眠/恢复/回收
internal/vmdriver/      VM 驱动接口 + firecracker + mock
internal/envdclient/    agentd 的 Connect-RPC 客户端（生成代码在 internal/gen/）
internal/store/         存储层：PG（事实源：sessions/messages/sandboxes）+ Redis（协调态）
proto/agentd/     agentd 的 proto 定义（buf generate → internal/gen/）
web/              Vite + React 调试界面
deploy/           docker-compose（Temporal + Redis + PostgreSQL）
```

## 参考实现（源码）

- 沙箱调度/快照/envd 通信 → [e2b-dev/runtime](https://github.com/e2b-dev/runtime)
  （本地精读副本：`ys-learn/refer/e2b-runtime`，对比笔记：`ys-learn/notes/agents/open-source-sandbox-comparison.md`）
- Temporal agent loop 写法 → [temporal-community/temporal-ai-agent](https://github.com/temporal-community/temporal-ai-agent)
- 产品形态/沙箱工具 API → [kortix-ai/suna](https://github.com/kortix-ai/suna)
- Go 薄 agent 层 → [charmbracelet/fantasy](https://github.com/charmbracelet/fantasy)（Crush 的 LLM 层）
