# 快照存宿主本地盘，不进 PG 也不上对象存储

M4 快照休眠/恢复设计（2026-10-30 访谈定案）。休眠快照（内存约 512MB + vCPU/设备状态）以文件形式存放在 VM 所在宿主的本地磁盘（`{VMDir}/<sandbox_id>/`），PG 只记元数据（host_id、status、slot、snapshot_ref）。

理由：
1. **体积**：每个 sleeping sandbox 512MB+，进 PG 会拖垮 backup/vacuum。
2. **局部性**：resume 的读取方就是要跑 VM 的那台宿主，本地读 512MB ≈ 1~2 秒；走网络拉取会翻倍唤醒延迟，而"醒得快"正是休眠的卖点。
3. **职责**：按"VM 永远可重建"原则，快照是可丢的优化（丢了降级全新 boot），不需要 DB 级持久性。

## Considered Options

- **PG jsonb/bytea**：体积与局部性双输，排除。
- **对象存储（e2b 路线）**：支持任意宿主复活，但需要 diff 格式、上传管线、prefetch 等重工程；单宿主规模下是过度设计。
- **云盘挂载（如腾讯云 CBS）**：零代码的演进路径——VM 目录挂云盘，宿主故障时 detach/reattach 到备用宿主即可保住快照与文件。

## Consequences

- 用户被绑定在 `sandboxes.host_id` 记的那台宿主上；宿主磁盘故障 = sleeping 快照与 VM 内文件丢失，走降级 boot（v1 接受的妥协）。
- 演进路径（按规模逐级解锁）：本地盘 → 云盘挂载 → 对象存储快照 + files sync（对齐 Grok Bot "files sync to cloud storage"）。
