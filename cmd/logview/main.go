// Command logview 是单进程日志查看器。
//
// 它只做四件事：跟随一个固定路径的日志文件、把每条记录投影成受隐私策略
// 约束的可见事件、维护有界缓冲、通过白名单 HTTP 端点把结果推给浏览器。
// 运行镜像里没有 Node、Python、tmux、ttyd、ccze 或任何 PTY。
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/parser"
	"github.com/Seameee/Logview/internal/privacy"
	"github.com/Seameee/Logview/internal/server"
	"github.com/Seameee/Logview/internal/source"
	"github.com/Seameee/Logview/internal/stream"
)

// shutdownBudget 是收到信号后的有界关闭预算。
const shutdownBudget = 5 * time.Second

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)

	cfg, warnings, err := config.Load(os.Getenv)
	if err != nil {
		logger.Printf("配置错误：%v", err)
		os.Exit(1)
	}
	for _, w := range warnings {
		logger.Printf("提示：%s", w)
	}

	proj, err := privacy.New(cfg.AnonymizeMode, cfg.FilterWords, cfg.ExposeLogDetails)
	if err != nil {
		logger.Printf("隐私模块初始化失败：%v", err)
		os.Exit(1)
	}

	p := parser.New(cfg.LogFormat, cfg.TimeZone)
	hub := stream.NewHub()

	var dropped atomic.Int64
	hub.SetDropHook(func() { dropped.Add(1) })

	info := event.PrivacyInfo{
		AnonymizeMode:    string(cfg.AnonymizeMode),
		DetailsExposed:   cfg.ExposeLogDetails,
		TimeZone:         cfg.TimeZoneName,
		TimeZoneExplicit: cfg.TimeZoneExplicit,
		FilterWordCount:  len(cfg.FilterWords),
	}

	follower := source.New(cfg.LogFile, p, 0, 0, 0)
	follower.OnRecord = func(rec parser.Record) {
		hub.Append(proj.Project(rec, stream.NowUTC()))
	}
	follower.OnStatus = func(st event.SourceStatus) {
		hub.SetStatus(st)
	}
	follower.OnHistoryIncomplete = func() {
		// 再追加一次当前状态即可让客户端知道窗口不完整由快照字段表达，
		// 这里不需要额外事件；预留钩子以便诊断接入。
		_ = struct{}{}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go follower.Run(ctx)

	static := staticHandler()
	srv := server.New(static, server.Options{
		Hub:        hub,
		Follower:   follower,
		Privacy:    info,
		MaxClients: cfg.MaxSSEClients,
		Logger:     logger,
	})

	httpSrv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv,
		// 读超时覆盖请求头与请求体；SSE 不写超时，靠心跳与写期限保活。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          logger,
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Printf("监听失败：%v", err)
		os.Exit(1)
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	logger.Printf("logview 已启动，监听 %s（anon=%s, format=%s, tz=%s, details=%v, clients=%d）",
		cfg.ListenAddr, cfg.AnonymizeMode, cfg.LogFormat, cfg.TimeZoneName, cfg.ExposeLogDetails, cfg.MaxSSEClients)

	select {
	case <-ctx.Done():
		logger.Printf("收到退出信号，开始有界关闭")
	case err := <-serveErr:
		if err != nil {
			logger.Printf("HTTP 服务异常：%v", err)
		}
		stop()
	}

	// 取消源与 SSE，再有界关闭 HTTP，最后关闭缓冲。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	hub.Close()

	if n := dropped.Load(); n > 0 {
		logger.Printf("运行期间因容量上限淘汰的事件数：%d", n)
	}
	logger.Printf("已退出")
}

// staticHandler 选择内嵌产物或占位提示页。
func staticHandler() http.Handler {
	if server.HasEmbeddedIndex() {
		return server.NewStaticHandler(server.EmbeddedDist())
	}
	return http.HandlerFunc(placeholderPage)
}

// placeholderPage 在未构建前端时给出明确提示，而不是空白页。
func placeholderPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><meta charset="utf-8">` +
		`<title>Logview</title><body style="font-family:system-ui;padding:2rem">` +
		`<h1>前端尚未构建</h1><p>请先运行构建脚本（<code>./scripts/build.sh</code>），` +
		`它会把 <code>web/dist/</code> 复制到 <code>internal/server/dist/</code> 后重新编译。</p>` +
		`<p>后端与 <code>/api/stream</code> 端点已可用。</p></body></html>`))
}
