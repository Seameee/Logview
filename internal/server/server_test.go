package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/parser"
	"github.com/Seameee/Logview/internal/privacy"
	"github.com/Seameee/Logview/internal/source"
	"github.com/Seameee/Logview/internal/stream"
)

var testPrivacy = event.PrivacyInfo{AnonymizeMode: "partial", TimeZone: "UTC", FilterWordCount: 0}

// newTestServer 构造只有 hub 的服务器（静态资源用占位处理器）。
func newTestServer(t *testing.T, max int) (*Server, *stream.Hub) {
	t.Helper()
	hub := stream.NewHub()
	t.Cleanup(hub.Close)
	srv := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>t</title>"))
	}), Options{Hub: hub, Privacy: testPrivacy, MaxClients: max, Heartbeat: 50 * time.Millisecond})
	return srv, hub
}

func TestRouteWhitelist(t *testing.T) {
	srv, hub := newTestServer(t, 4)
	hub.Append(event.Event{Type: event.TypeLog, Kind: event.KindMessage, Text: "hi", ObservedAt: time.Now().UTC()})

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodHead, "/", http.StatusOK},
		{http.MethodPost, "/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodHead, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/stream", http.StatusMethodNotAllowed},
		// 未知路径一律 404，不泄露文件系统结构。
		{http.MethodGet, "/etc/passwd", http.StatusNotFound},
		{http.MethodGet, "/var/log/0.log", http.StatusNotFound},
		{http.MethodGet, "/api/unknown", http.StatusNotFound},
		{http.MethodGet, "/download", http.StatusNotFound},
		{http.MethodPost, "/upload", http.StatusNotFound},
		{http.MethodGet, "/shell", http.StatusNotFound},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _ := newTestServer(t, 4)
	for _, p := range []string{"/", "/healthz", "/nope"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got != ContentSecurityPolicy {
			t.Fatalf("%s CSP = %q", p, got)
		}
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s nosniff = %q", p, got)
		}
		if got := h.Get("Referrer-Policy"); got != "no-referrer" {
			t.Fatalf("%s referrer = %q", p, got)
		}
	}
	// CSP 必须保持严格：无 unsafe-inline/unsafe-eval，且默认拒绝。
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*"} {
		if strings.Contains(ContentSecurityPolicy, bad) {
			t.Fatalf("CSP must not contain %q: %s", bad, ContentSecurityPolicy)
		}
	}
	if !strings.Contains(ContentSecurityPolicy, "default-src 'none'") {
		t.Fatalf("CSP must start from default-src 'none': %s", ContentSecurityPolicy)
	}
}

func TestHealthzShape(t *testing.T) {
	srv, hub := newTestServer(t, 4)
	hub.SetStatus(event.SourceStatus{State: event.StateLive})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var body struct {
		Status string `json:"status"`
		Source struct {
			State string `json:"state"`
		} `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || body.Source.State != "live" {
		t.Fatalf("body = %+v", body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("healthz cache = %q", got)
	}
}

// sseReader 从 SSE 响应里逐条读取事件（只用于测试）。
type sseFrame struct {
	Event string
	ID    string
	Data  string
}

func readFrames(t *testing.T, r io.Reader, limit int) []sseFrame {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var frames []sseFrame
	var cur sseFrame
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Data != "" || cur.Event != "" {
				frames = append(frames, cur)
				if limit > 0 && len(frames) >= limit {
					return frames
				}
			}
			cur = sseFrame{}
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, ":"):
			// 心跳注释：忽略。
		}
	}
	return frames
}

func TestStreamSnapshotThenLog(t *testing.T) {
	srv, hub := newTestServer(t, 4)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}

	// 连接建立后追加一条，客户端必须收到。
	go func() {
		time.Sleep(30 * time.Millisecond)
		hub.Append(event.Event{Type: event.TypeLog, Kind: event.KindMessage, Text: "hello", ObservedAt: time.Now().UTC()})
	}()

	frames := readFrames(t, resp.Body, 2)
	if len(frames) < 2 {
		t.Fatalf("frames = %d, want snapshot + log", len(frames))
	}
	if frames[0].Event != "snapshot" || frames[0].ID == "" {
		t.Fatalf("first frame = %+v, want snapshot with id", frames[0])
	}
	var snap event.Snapshot
	if err := json.Unmarshal([]byte(frames[0].Data), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Type != "snapshot" || snap.Watermark != frames[0].ID {
		t.Fatalf("snapshot watermark %q != id %q", snap.Watermark, frames[0].ID)
	}
	if snap.Privacy.AnonymizeMode != "partial" {
		t.Fatalf("privacy not carried in snapshot: %+v", snap.Privacy)
	}

	if frames[1].Event != "log" || frames[1].ID == "" {
		t.Fatalf("second frame = %+v", frames[1])
	}
	if !strings.Contains(frames[1].Data, `"text":"hello"`) {
		t.Fatalf("log payload = %q", frames[1].Data)
	}
	if !strings.Contains(frames[1].Data, `"type":"log"`) {
		t.Fatalf("log payload must carry type: %q", frames[1].Data)
	}
}

func TestStreamResumeWithLastEventID(t *testing.T) {
	srv, hub := newTestServer(t, 4)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	first := hub.Append(event.Event{Type: event.TypeLog, Kind: event.KindMessage, Text: "a", ObservedAt: time.Now().UTC()})
	hub.Append(event.Event{Type: event.TypeLog, Kind: event.KindMessage, Text: "b", ObservedAt: time.Now().UTC()})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/stream", nil)
	req.Header.Set("Last-Event-ID", first)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	frames := readFrames(t, resp.Body, 1)
	if len(frames) < 1 {
		t.Fatal("no frames")
	}
	var snap event.Snapshot
	if err := json.Unmarshal([]byte(frames[0].Data), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Reason != nil {
		t.Fatalf("valid cursor must not reset: %q", *snap.Reason)
	}
	if len(snap.Events) != 1 || snap.Events[0].Text != "b" {
		t.Fatalf("resume events = %+v, want only 'b'", snap.Events)
	}
}

func TestStreamInvalidCursorGetsResetSnapshot(t *testing.T) {
	srv, hub := newTestServer(t, 4)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	hub.Append(event.Event{Type: event.TypeLog, Kind: event.KindMessage, Text: "a", ObservedAt: time.Now().UTC()})

	// 超长且非法的游标：不得引发解析开销或错误，只触发重置快照。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/stream", nil)
	req.Header.Set("Last-Event-ID", strings.Repeat("9", 5000))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	frames := readFrames(t, resp.Body, 1)
	if len(frames) < 1 {
		t.Fatal("no frames")
	}
	var snap event.Snapshot
	if err := json.Unmarshal([]byte(frames[0].Data), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Reason == nil {
		t.Fatal("invalid cursor must produce a reset reason")
	}
	if len(snap.Events) != 1 {
		t.Fatalf("reset snapshot must still carry the window: %d", len(snap.Events))
	}
}

func TestStreamHeartbeat(t *testing.T) {
	srv, _ := newTestServer(t, 4)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 心跳是注释行，读端应能在没有日志事件时也收到字节。
	buf := make([]byte, 512)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = resp.Body.(io.Reader)
		n, err := resp.Body.Read(buf)
		if n > 0 && strings.Contains(string(buf[:n]), "heartbeat") {
			return
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	t.Fatal("no heartbeat comment observed")
}

func TestStreamClientLimit(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/stream")
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 确保第一条连接已占用名额。
	readFrames(t, resp.Body, 1)

	resp2, err := http.Get(ts.URL + "/api/stream")
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second stream = %d, want 503 (有界并发)", resp2.StatusCode)
	}
}

func TestStreamFreesSlotOnDisconnect(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		readFrames(t, resp.Body, 1)
		cancel()
		_ = resp.Body.Close()
		// 等待服务端感知断开并释放名额。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if hubSubs(srv) == 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if n := hubSubs(srv); n != 0 {
			t.Fatalf("round %d: subscribers = %d after disconnect", i, n)
		}
	}
}

func hubSubs(s *Server) int { return s.hub.Subscribers() }

// TestEndToEndFileToSSE 覆盖 文件 → 源 → 投影 → SSE 的完整链路。
func TestEndToEndFileToSSE(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0.log")
	content := "2026/05/04 12:00:01.123456 198.51.100.24:51234\n" +
		"2026/05/04 12:00:02.000000 198.51.100.24:51235 SSH-2.0-OpenSSH_9.6 root secretpass\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	proj, err := privacy.New(config.ModePartial, []string{"secretpass"}, false)
	if err != nil {
		t.Fatalf("privacy.New: %v", err)
	}
	hub := stream.NewHub()
	defer hub.Close()

	f := source.New(path, parser.New(config.FormatFakessh, time.UTC), 0, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.OnRecord = func(rec parser.Record) { hub.Append(proj.Project(rec, time.Now().UTC())) }
	f.OnStatus = hub.SetStatus
	go f.Run(ctx)

	// 等启动尾读把两行都送进 hub，再建立 SSE 连接，避免重连轮询。
	deadline := time.Now().Add(5 * time.Second)
	for hub.Len() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hub.Len() < 2 {
		t.Fatalf("follower published %d events, want 2", hub.Len())
	}

	srv := New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), Options{Hub: hub, Privacy: testPrivacy, MaxClients: 4, Heartbeat: time.Second})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	frames := readFrames(t, resp.Body, 1)
	if len(frames) == 0 {
		t.Fatal("no snapshot frame")
	}
	var snap event.Snapshot
	if err := json.Unmarshal([]byte(frames[0].Data), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Events) < 2 {
		t.Fatalf("snapshot events = %d, want at least 2", len(snap.Events))
	}

	body, err := json.Marshal(snap.Events)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(body)
	for _, forbidden := range []string{"198.51.100.24", "secretpass", "root"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("SSE payload leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "198.51.100.xxx") {
		t.Fatalf("expected partial-masked address in %s", text)
	}
	if !strings.Contains(text, `"kind":"auth_attempt"`) {
		t.Fatalf("expected an auth_attempt event in %s", text)
	}
}

func TestStaticHandlerRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>ok</h1>"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	h := NewStaticHandler(os.DirFS(dir))

	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/", nil))
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), "ok") {
		t.Fatalf("index = %d %q", ok.Code, ok.Body.String())
	}

	for _, p := range []string{"/../secret.txt", "/..%2fsecret.txt", "/./secret.txt", "/assets/../secret.txt"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("%s served a file outside the whitelist", p)
		}
	}
	// 目录不得被列举。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("directory listing leaked: %d", rec.Code)
	}
}

func TestStaticHandlerContentTypesAndCaching(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app-abc123.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>ok</h1>"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	h := NewStaticHandler(os.DirFS(dir))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-abc123.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed asset cache = %q", cc)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index cache = %q, want no-cache（避免长久缓存旧入口）", cc)
	}
}
