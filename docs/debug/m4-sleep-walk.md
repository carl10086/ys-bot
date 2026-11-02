---
title: M4 休眠/恢复 debug 地图
subtitle: 一条完整闭环：发消息 → 续期 → 空闲休眠 → 再发消息 → 原地恢复
template: sheet
cols: 3
---
本图配 GoLand 使用：Debug 配置 `code-walk-m4-sleep`（worker）+ `ys-server`（入口）。
所有预期值来自 2026-10-30 152 真机 e2e 实测，sandbox id 以你运行为准。

## A 关键路径 {span=3}

```flow
[SPA/curl] -> [ys-server]: POST messages
[ys-server] -> hitsend.Submit: BP1 续期
hitsend.Submit -> [Temporal]: 起 TurnWorkflow
[Temporal] -> [ys-worker]: turn 执行
[ys-worker] -> ExecTool: BP6 每次工具调用续期
Schedule -> SleepSweepWorkflow: 每分钟点火
SleepSweepWorkflow -> ListSleepCandidates: BP2 选谁睡
SleepSweepWorkflow -> StartSleepWorkflows: client 起 SleepWorkflow
SleepWorkflow -> TryStartSleep: BP3/BP4 CAS 占坑
SleepWorkflow -> vmdriver.SleepVM: BP5 找宿主
vmdriver.SleepVM -> [vmhostd@152]: pause+快照+停机
[再发消息] -> EnsureAwake: 懒唤醒
EnsureAwake -> [vmhostd@152]: resume 原地恢复
```

远程 vmhostd 不在 GoLand 断点范围（跑在 152）——用日志观察：`ssh root@152.136.212.59 'tail -f /data/firecracker/vmhostd.log'`。

## B 驱动脚本（按顺序执行） {span=3}

```bash title="驱动脚本 · 每步对应哪些断点命中"
# 0. GoLand 同时 Debug 两个配置：ys-server + code-walk-m4-sleep
# 1. 建会话 + 发消息（命中 BP1；turn 用工具命中 BP6）
SID=$(curl -s -X POST localhost:8080/api/sessions \
  -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -s -X POST localhost:8080/api/sessions/$SID/messages \
  -H 'Content-Type: application/json' \
  -d "{\"msg_id\":\"$(uuidgen)\",\"text\":\"在 /tmp 建个文件 walk.txt 写上 hello\"}"
# 2. 静置 ~2min（配置里 SLEEP_IDLE_MINUTES=1）→ 依次命中 BP2 BP3 BP4 BP5
# 3. 再发消息 → 懒唤醒 resume（观察 vmhostd 日志"原地恢复"）
curl -s -X POST localhost:8080/api/sessions/$SID/messages \
  -H 'Content-Type: application/json' \
  -d "{\"msg_id\":\"$(uuidgen)\",\"text\":\"读一下 /tmp/walk.txt\"}"
```

## C 断点清单 {span=3}

| 断点 | 位置 | 预期观察值（实测） | 理解问题 |
|---|---|---|---|
| BP1 续期入口 | hitsend.go:45 | err 被忽略；PG 里 last_activity_at=now() | 发消息时谁告诉系统"用户还活着" |
| BP2 选谁睡 | activities.go:50 | 活跃时 []；静置后 ["019f0000-…"] | 什么样的 sandbox 会进休眠候选 |
| BP3 CAS 占坑 | activities.go:63 | 空闲 acquired=true；turn 中 false | never-mid-turn 的原子互斥在哪 |
| BP4 抢不到就走 | sleep.go:53 | 抢不到直接 return nil，无 SleepSandbox | CAS 失败时 workflow 怎么收工 |
| BP5 找宿主 | vmdriver.go:126 | HGET vmhost:alive:host-152 → 152.136.212.59:15051 | worker 怎么知道 VM 在哪台宿主 |
| BP6 turn 中续期 | activities.go:190 | 每次工具调用都刷 now() | 长 turn 凭什么不被 sweeper 睡掉 |
| BP7 自愈分支 | vmdriver.go:113 | 正常路径不命中；撞上休眠竞态才进 | 休眠和 Exec 撞车时谁兜底 |

BP1 在 ys-server 进程里（Debug `ys-server` 配置才命中）；BP2–BP7 在 ys-worker 里。

## D IDE 操作提示

- 配置：`code-walk-m4-sleep`（= ys-worker + SLEEP_IDLE_MINUTES=1，静置 1 分钟即睡）
- BP1：在 `Submit` 里 step over，然后 DataGrip 查 `SELECT last_activity_at FROM sandboxes`
- BP3：在 `TryStartSleep` step into 看 SQL 参数；turn 进行中发消息再静置，观察 acquired 变 false
- BP4：`if !acquired` 分支，step over 看函数安静返回
- BP5：step into `vmhostClient`，watch Redis HGET 的返回值（vmhostd 地址来源）
- BP7：想命中需构造竞态（VM 入睡瞬间发工具调用），首次 walk 可只读代码
- 观察点：休眠后 `ls /data/firecracker/vms/<id>/` 应见 snap.mem(512MB)+snap.vmstate(约 14KB)
