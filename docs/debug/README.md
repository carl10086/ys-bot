# docs/debug — M2 调试学习文档

断点调试导读 + 链路图解，配合 GoLand 断点使用。自包含 HTML，浏览器直接打开。

| 页面 | 内容 | 对应代码 |
|---|---|---|
| [m2-debug-guide.html](m2-debug-guide.html) | 按场景组织的断点清单（纯对话红线 / 懒唤醒 / ExecTool 本地×远端对照 / 多轮上限 / reconcile），每个场景 = 操作 + 断点位置 + 预期观察 | `internal/workflow/turn.go`、`internal/workflow/activities.go`、`internal/vmdriver/` |
| [m2-lazy-wake-flow.html](m2-lazy-wake-flow.html) | 懒唤醒全链路图：时序图（断点 1-7 位置）、EnsureAwake 三分支、vmhostd 启动流、PG/Redis 数据落点 | `internal/vmdriver/vmdriver.go:68-91`、`internal/vmhostd/` |

## 远端日志对照（2026-10-30 起可用）

vmhostd 四个 RPC 有请求级日志，格式 `[vmhostd] <rpc> sb=<短id8位> … dur=<ms>`：

```bash
ssh root@152.136.212.59 'tail -f /data/firecracker/vmhostd.log'
```

断点停在 `vmdriver.go` 放行后，远端日志出现的时刻 = 请求跨公网到达 vmhostd 的时刻。

## 注意

- 页面内断点行号以 2026-10-29 的代码为准；代码演进后先核对行号再用
- workflow 代码断点需配 `DEBUG_DEADLOCK_TIMEOUT_SEC=3600`（见 `cmd/ys-worker/main.go` 注释），否则触发 TMPRL1101
