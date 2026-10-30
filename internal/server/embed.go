package server

import (
	"embed"
	"io/fs"
)

// 内嵌前端构建产物。构建顺序：web/ 里 npm ci && npm run build，然后把
// web/dist/ 复制到 internal/server/dist/，最后编译 Go。干净检出时该目录
// 只有占位文件，因此运行时会退化为提示页而不是启动失败。
//
//go:embed all:dist
var embeddedDist embed.FS

// EmbeddedDist 返回以 dist 为根的内嵌文件系统。
func EmbeddedDist() fs.FS {
	sub, err := fs.Sub(embeddedDist, "dist")
	if err != nil {
		return embeddedDist
	}
	return sub
}

// HasEmbeddedIndex 报告是否存在真正的构建产物入口。
func HasEmbeddedIndex() bool {
	_, err := fs.Stat(EmbeddedDist(), "index.html")
	return err == nil
}
