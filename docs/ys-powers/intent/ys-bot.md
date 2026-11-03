# ys-bot — 意图文档

> 产出：`/clarify-intent`（2026-10-27），经访谈确认，用户明确 yes。
> 项目根：`/Users/yusizhen/soft/projects/ys-bot`（独立于 ys-learn 的新项目）

## 确认的意图

```
Outcome:      ys-bot——亲手实现 Grok Bot 架构的 always-on agent 沙箱系统，
              跑在自己的 Linux 服务器上，真实可用而非纯玩具
User:         你自己（第一个且唯一的用户）
Why now:      读完 e2b runtime / Suna 等源码后，要把架构认知变成自己跑起来的系统
Success:      web 调试页发消息 → server 侧 loop 调 LLM → HTTP 调 VM 内工具执行
              → 回复回显；VM 不活跃时快照休眠、下次调用原地恢复
Constraint:   单机起步（多节点调度延后）；Go server + Temporal + Redis +
              Firecracker + fantasy（薄 agent 层）；调试界面 = Go 内嵌轻 SPA
Out of scope: 浏览器/桌面进 VM、记忆四层栈、auto review、多节点、
              Mac 支持（后期可插拔）
```

## 关键架构决策（访谈中逐项确认）

1. **Agent loop 在 server 侧**（Grok Bot 原版架构）：VM 只是 agent 的"电脑"——薄工具进程
   （envd 类：shell、文件读写）。会话状态全在 server，VM 可随时重建。
   "Save everything on the server. The virtual machine can then always be rebuilt."
2. **中心 server 单体起步**：web → server → 调度本机 Firecracker microVM。
   多节点（Redis 目录路由、client-proxy）延后，但 Redis v1 就要（状态/队列/路由表预留）。
3. **Temporal 必选**：durable execution 承载 agent loop 的每一步（崩溃从断点恢复）。
   用户明确"肯定要"，即使当前不熟。
4. **快照休眠是 v1 核心验证目标**：不活跃 VM 快照 + 停机，下次调用原地恢复。
   这是选这个架构而非"又一个代码沙箱"的根本原因。
5. **浏览器不进 VM**（v1）：沙箱负载 = agent 的 shell/文件操作，无桌面无 Chrome。
6. **Go 全栈 server**：Temporal 亲儿子 SDK、firecracker-go-sdk 官方、
   e2b runtime 同语言可直接对照借鉴。调试 web = Go embed 的轻 SPA（几百行 React）。
7. **agent 框架 = fantasy（charm.land/fantasy）v0.x**：pi 的 Go 生态位——
   多 provider 统一 API + tools + 薄 loop，Crush（28.8k⭐）的 LLM 层抽出的库。
   **框架关在隔离接口后，预期将来替换**（fallback：官方 SDK 手写百行 loop）。
   排除重框架的理由：adk-go / trpc-agent-go / eino 的 session、graph workflow
   与 Temporal + server 职责冲突。

## 开发环境约束（Phase 3 交叉检查发现）

- 开发机是 Mac（Go 1.27.3 就绪），Firecracker 是 Linux/KVM 专属
- → v1 开发循环：本地写代码，推到 Linux 服务器跑；或本地 mock VM 驱动层
- VM 驱动接口要在 spec 阶段就设计成可插拔（firecracker / mock）

## 参考项目（源码可读）

| 参考什么 | 项目 | 位置 |
|---|---|---|
| 沙箱调度/快照/envd 通信 | e2b-dev/runtime（1.7k⭐） | `ys-learn/refer/e2b-runtime/`（已精读，笔记在 `ys-learn/notes/agents/open-source-sandbox-comparison.md`） |
| Temporal agent loop 写法 | temporal-community/temporal-ai-agent（787⭐，Python） | 待 clone |
| 产品形态/沙箱工具 API 设计 | kortix-ai/suna（20.3k⭐） | 待 clone |
| Go 薄 agent 层 | charmbracelet/fantasy（1k⭐）+ charmbracelet/crush（28.8k⭐，使用者） | 待 clone |

## 下一步

走 `/spec`：写结构化设计文档（spec 保存到本项目 `docs/`）。
