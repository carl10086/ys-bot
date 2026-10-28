# 分阶段计划（M1–M5）

> 状态：2026-10-28 确认。原则：**先把 ys-server → ys-worker 主干跑通，再逐层加真实能力**。
> 每个阶段必须跑通才算完成（跑不通就修，不留"逻辑对但没跑"的东西）。

## 环境前提

- 用户将提供**真实 Linux 服务器**；开发/部署都以它为主力，**Mac 场景暂不考虑**（2026-10-28 更新）。
- M1 不涉及 VM，天然与平台无关（依赖用 `deploy/` 的 docker-compose 起）。
- mock 驱动降级为**测试替身**（vmdriver 接口的假实现），不再是日常开发回路的载体。

## 阶段划分

| 阶段 | 内容 | 验证标准 | 明确不做 |
|---|---|---|---|
| **M1 主干** | SPA → ys-server（去重/入队/触发）→ Temporal → ys-worker（drain → 真 LLM → 回复）→ 回显 | 发一条消息看到回复；重发同 msg_id 不产生第二条；turn 进行中发消息能打断 | VM、工具 |
| **M2 工具链** | agentd（Connect-RPC）+ 工具执行进 turn loop | agent 能调 shell/文件工具并拿到结果 | 快照 |
| **M3 真 Firecracker** | firecracker 驱动替换 mock，跑在 Linux 服务器 | 工具在真 VM 里执行 | 休眠 |
| **M4 快照休眠/恢复** | 空闲休眠 → 快照 → 停机 → 懒恢复 | 空闲 N 分钟后 VM 停机；下次 tool_call 原地恢复、环境还在 | — |
| **M5 记忆文件** | memory/skills 文件树 + prompt 注入 + turn 后 memory pass | 告诉 agent 一个偏好，新会话里它还记得 | routines 调度 |

## 推迟记录

- **第 4 节设计（快照休眠/恢复时序）= M4 的第一个议题**（2026-10-28 用户决策）。
  理由：快照依赖真 Firecracker 行为，M3 之前设计是空对空；届时带着实测约束再做。
  待答问题清单：谁计时触发休眠、never-mid-turn 的实现、快照内容、首次创建镜像来源。
- M2 用 mock 还是直接上真 Firecracker：取决于 Linux 服务器到位时间，M2 开工时再定。
