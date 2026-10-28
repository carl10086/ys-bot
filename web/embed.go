// Package web 把构建产物嵌进 ys-server 二进制。
// dist 由 `cd web && npm run build` 生成；仓库里只留占位 index.html 保证 go build 可过。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist 返回 SPA 静态文件根（剥掉 dist/ 前缀）。
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
