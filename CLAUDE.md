# CLAUDE.md

给 AI 代理的工作指引。**修改前先读一遍——它比默认行为优先。**

## 项目定位

个人项目：按 Grok Bot 架构实现的 always-on agent 沙箱系统。
先读 `docs/ys-powers/intent/ys-sandbox.md`（已确认的意图）和 `README.md`（架构决策）。

## 技术栈（已定，不要偏离）

Go（单 module）· Temporal · PostgreSQL（业务事实源）· Redis（仅协调态）· Firecracker · charm.land/fantasy（agent 薄层）· Connect-RPC（server↔agentd）· React SPA（调试界面）

## 结构规则

- 二进制在 `cmd/<name>/`，业务代码全在 `internal/`
- **不建 `pkg/`**——除非出现真实的外部 import 需求
- **不拆多 module（go.work）**——除非某个组件需要独立发版
- `internal/vmdriver/`：接口（`driver.go`）与实现（`firecracker/`、`mock/`）分目录；新增驱动走同一接口
- `internal/agent/`：fantasy 必须关在适配接口后面，业务代码不得直接 import fantasy 类型
- agentd 协议改 `proto/agentd/*.proto` 后必须重跑 `buf generate`（产物在 `internal/gen/`，不手改）

## 边界（不要做）

- ❌ 浏览器/桌面进 VM（v1 明确排除）
- ❌ 多节点调度逻辑（Redis 目录路由、client-proxy——单机起步，接口预留即可）
- ❌ 在 VM 内放 agent loop 或模型凭证——loop 和凭证只在 server 侧
- ❌ 为"将来多节点"提前抽象——单机跑通再说

## 开发环境

- 主力 = **Linux 服务器**（用户提供，开发/部署同机）；Mac 场景暂不考虑（2026-10-28 更新）
- M1 主干无 VM，任意系统可跑（`go run` + docker-compose 依赖）；M2 起需要 VM，一律在 Linux 上
- mock 驱动只是测试替身，不是日常开发回路
- Temporal server、PostgreSQL、Redis 用 `deploy/` 的 docker-compose 起，不自建
- **数据归属纪律**：业务事实源只写 PG / 文件存储；Redis 只放可丢的协调态（队列、幂等窗、锁、信号）——往 Redis 写"永久数据"是红线

## 参考源码

`../ys-learn/refer/e2b-runtime/`（只读）——调度/快照/envd 的一手参考。
改设计前先查它是怎么做的，但**不照抄部署拓扑**（它多节点多租户，我们单机）。
