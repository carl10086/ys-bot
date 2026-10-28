# ys-sandbox 开发命令。红线：只收"稳定 + 重复"的操作。

# 清掉所有数据（业务 PG + Redis + Temporal 历史）并重建空环境。
# 测试重来用。注意：ys-server / ys-worker 需重启（ApplySchema 重建表、worker 重连）。
reset:
	cd deploy && docker compose down -v && docker compose up -d

# 重启 ys-server：重 build SPA（go:embed 进二进制，web 改动必须重 build）+ 重 build + 换起。
# 只管 server；ys-worker 持有 LLM_API_KEY，由你手动跑。
# pkill 的 [x] 写法是防匹配到 make recipe 自身的 shell（cmdline 里含同样字符串）。
restart:
	cd web && npm run build
	-pkill -x ys-server
	-pkill -f '[g]o run ./cmd/ys-server'
	go build -o /tmp/ys-server ./cmd/ys-server
	nohup /tmp/ys-server > /tmp/ys-server.log 2>&1 &
	@echo "ys-server :8080 已重启（log: /tmp/ys-server.log）"
