package server

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// staticHandler 从内嵌文件系统提供前端产物。
//
// 只返回已存在的普通文件；".."、目录与反斜杠路径一律 404，因此不存在
// 目录遍历或文件列举入口。不做 SPA 回退：未知路径返回 404。
type staticHandler struct {
	root fs.FS
}

// NewStaticHandler 把一个只读文件系统包装成受限静态资源处理器。
func NewStaticHandler(root fs.FS) http.Handler {
	return &staticHandler{root: root}
}

func (s *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := cleanName(r.URL.Path)
	if !ok {
		notFound(w)
		return
	}

	f, err := s.root.Open(name)
	if err != nil {
		notFound(w)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		notFound(w)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		notFound(w)
		return
	}

	h := w.Header()
	h.Set("Content-Type", contentType(name))
	if strings.HasPrefix(name, "assets/") {
		// 构建产物文件名带内容哈希，可长期缓存。
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, path.Base(name), info.ModTime(), rs)
}

// cleanName 把 URL 路径映射到内嵌文件系统内的相对路径。
//
// 路径里出现 "." 或 ".." 段时直接拒绝，而不是先 Clean 再放行：Clean 会把
// "/../secret.txt" 悄悄改写成 "/secret.txt"，等于接受了一次越权访问请求。
func cleanName(p string) (string, bool) {
	if p == "" || p == "/" {
		return "index.html", true
	}
	if strings.ContainsRune(p, '\\') || strings.Contains(p, "\x00") {
		return "", false
	}
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	clean := path.Clean(p)
	name := strings.TrimPrefix(clean, "/")
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

var fixedTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".ico":   "image/x-icon",
	".png":   "image/png",
	".webp":  "image/webp",
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".txt":   "text/plain; charset=utf-8",
	".map":   "application/json; charset=utf-8",
}

// contentType 按扩展名给出明确的 MIME，未知类型退回二进制流。
func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := fixedTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}
