# 记忆与技能存 PG 三表，不抄 Grok Bot 的文件树形态

M5 记忆文件设计（2026-11-02 访谈定案）。memory（profile/log/note 三类事实行）+ skills（技能文档）+ memory_ops（审计流水）全部落 PG；不建 markdown 文件树；语义检索以"加 embedding 列 + pgvector/全文索引"预留，不实现。

理由：
1. **VM 独立性**：拼 prompt 的是 ys-worker，而 VM 可能睡着（M4 懒唤醒）、或本轮根本不需要 VM（简单对话）——记忆必须服务端随时可读。Grok Bot 的文件方案能闭环靠的是"休眠时 files sync to cloud storage"的双副本；我们无对象存储、文件字节又不进 PG（ADR-0001 同一理由），文件形态在我们这里闭不了环。
2. **结构化写入**：提取 pass 产出 add/update/delete 操作，落行有事务、可审计；落文件则要自写"ops 应用到 markdown"的整套机制（OpenViking 为此写了 Rust 路径锁 + MemoryUpdater）。
3. **检索演进**：百级体量全量注入够用；搜索需求来了在表上加列加索引，事实源不换。

## Considered Options

- **VM 内文件树（Grok Bot 原教旨）**：宿主死=失忆；VM 睡着时服务端读不到；排除。
- **服务端文件树 + 派生索引（OpenClaw 形态）**：grep/rg 的 shell 体验用户明确不需要（agent 反正只能经工具访问）；双写一致性与文件并发写成本不值。
- **引入 OpenViking / mem0 组件**：Python+Rust+C++ 异构栈进 Go monorepo、AGPL 协议、为多租户大规模设计；抄其设计（类型体系、两段式提取、审计），不引代码。

## Consequences

- agent 访问记忆只能经 `memory_search` / `memory_read` / `memory_save` 三个**服务端工具**（不经 VM/agentd）——这是 M5 新增的第二条工具执行路径。
- "记忆是信息不是指令"（讲座 30）靠 `memories.type` 与 `skills` 表区分，不靠目录树。
- 演进路径：全量注入（v1）→ 加 embedding 列做语义检索 → importance/时间衰减排序 → 后台整理合并（对应 OpenClaw dreaming / OpenViking ov compile）。
