// Package server 提供受白名单约束的 HTTP 入口。
//
// 只暴露三类端点：内嵌静态资源、GET /api/stream、GET /healthz。
// 不提供文件浏览、下载、上传、shell 或任何出站请求。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/source"
	"github.com/Seameee/Logview/internal/stream"
)

// ContentSecurityPolicy 是统一的 CSP 基线。
//
// 所有响应都带同一份策略；页面没有内联脚本/内联样式/eval 的需求。
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"connect-src 'self'; img-src 'self'; font-src 'self'; object-src 'none'; " +
	"base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

// Options 是服务器的构造参数。
type Options struct {
	Hub        *stream.Hub
	Follower   *source.Follower
	Privacy    event.PrivacyInfo
	MaxClients int
	// Heartbeat 是 SSE 心跳间隔，默认 15 秒。
	Heartbeat time.Duration
	Logger    *log.Logger
}

// Server 是 HTTP 处理器。
type Server struct {
	hub      *stream.Hub
	follower *source.Follower
	privacy  event.PrivacyInfo
	slots    chan struct{}
	beat     time.Duration
	log      *log.Logger
	static   http.Handler
}

// New 构造服务器。static 为内嵌静态资源处理器。
func New(static http.Handler, opt Options) *Server {
	beat := opt.Heartbeat
	if beat <= 0 {
		beat = 15 * time.Second
	}
	max := opt.MaxClients
	if max <= 0 {
		max = 64
	}
	logger := opt.Logger
	if logger == nil {
		logger = log.New(logDiscard{}, "", 0)
	}
	return &Server{
		hub:      opt.Hub,
		follower: opt.Follower,
		privacy:  opt.Privacy,
		slots:    make(chan struct{}, max),
		beat:     beat,
		log:      logger,
		static:   static,
	}
}

type logDiscard struct{}

func (logDiscard) Write(p []byte) (int, error) { return len(p), nil }

// ServeHTTP 实现白名单路由。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	switch p := r.URL.Path; p {
	case "/api/stream":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleStream(w, r)
	case "/healthz":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleHealthz(w, r)
	default:
		if !isStaticPath(p) {
			notFound(w)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.static.ServeHTTP(w, r)
	}
}

// isStaticPath 限定静态资源只在这几个前缀下。
func isStaticPath(p string) bool {
	if p == "/" || p == "/index.html" {
		return true
	}
	return strings.HasPrefix(p, "/assets/") || p == "/favicon.svg" || p == "/favicon.ico"
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", ContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// healthzBody 是健康检查响应。
type healthzBody struct {
	Status      string             `json:"status"`
	Source      event.SourceStatus `json:"source"`
	Subscribers int                `json:"subscribers"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	st, ok := s.hub.Status()
	if !ok {
		st = event.SourceStatus{State: event.StateConnecting}
	}
	writeJSON(w, http.StatusOK, healthzBody{
		Status:      "ok",
		Source:      st,
		Subscribers: s.hub.Subscribers(),
	})
}
