package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/parser"
)

// collector 收集跟随器回调，避免测试依赖 sleep。
type collector struct {
	mu      sync.Mutex
	records []parser.Record
	states  []event.SourceStatus
	histInc int
	// waiters 收到每次 emit 的通知。
	notify chan struct{}
}

func newCollector() *collector {
	return &collector{notify: make(chan struct{}, 1024)}
}

func (c *collector) onRecord(rec parser.Record) {
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *collector) onStatus(st event.SourceStatus) {
	c.mu.Lock()
	c.states = append(c.states, st)
	c.mu.Unlock()
}

func (c *collector) onHistoryIncomplete() {
	c.mu.Lock()
	c.histInc++
	c.mu.Unlock()
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
}

func (c *collector) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.records))
	for _, r := range c.records {
		out = append(out, r.RawLine)
	}
	return out
}

// waitFor 等到条件成立或超时；轮询间隔很短，不构成长时间 sleep。
func (c *collector) waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case <-c.notify:
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("condition not met within %s; got %d records: %v", d, c.count(), c.lines())
}

// start 启动跟随器并返回取消函数。
func start(t *testing.T, path string, c *collector, opts ...func(*Follower)) (context.CancelFunc, *Follower) {
	t.Helper()
	f := New(path, parser.New(config.FormatFakessh, time.UTC), 0, 0, 0)
	f.OnRecord = c.onRecord
	f.OnStatus = c.onStatus
	f.OnHistoryIncomplete = c.onHistoryIncomplete
	for _, o := range opts {
		o(f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("follower did not stop within 3s after cancel")
		}
	})
	return cancel, f
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteString(content); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func line(n int) string {
	return "2026/05/04 12:00:01.000000 198.51.100." + itoa(n%250) + ":51234\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestTailWindowIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	var sb strings.Builder
	for i := 0; i < 250; i++ {
		sb.WriteString(line(i))
	}
	writeFile(t, path, sb.String())

	c := newCollector()
	_, f := start(t, path, c)

	c.waitFor(t, 3*time.Second, func() bool { return c.count() >= DefaultTailLines })
	// 给一段时间确认没有继续灌入超出窗口的行。
	time.Sleep(120 * time.Millisecond)
	got := c.count()
	if got != DefaultTailLines {
		t.Fatalf("startup records = %d, want exactly %d", got, DefaultTailLines)
	}
	if c.histInc == 0 {
		t.Fatal("truncated tail must be reported as incomplete history")
	}
	if f.Diag().OversizedLines != 0 {
		t.Fatalf("unexpected diagnostics: %+v", f.Diag())
	}
}

func TestTailByteBudgetAvoidsFullFileScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	var sb strings.Builder
	// 超过尾读字节上限的内容，但仍需取到最后 100 行。
	filler := strings.Repeat("x", 2000) + "\n"
	for i := 0; i < 400; i++ {
		sb.WriteString(filler)
	}
	for i := 0; i < 120; i++ {
		sb.WriteString(line(i))
	}
	writeFile(t, path, sb.String())

	c := newCollector()
	_, f := start(t, path, c)
	c.waitFor(t, 3*time.Second, func() bool { return c.count() >= 100 })

	got := c.lines()
	if len(got) != DefaultTailLines {
		t.Fatalf("records = %d, want %d", len(got), DefaultTailLines)
	}
	// 最后一行必须是最新的那条，且不包含被截断的填充行。
	if !strings.Contains(got[len(got)-1], "198.51.100.119") {
		t.Fatalf("last record = %q, want the newest line", got[len(got)-1])
	}
	if strings.Contains(got[0], "xxxx") {
		t.Fatalf("first record looks like a fragment: %q", got[0])
	}
	_ = f
}

func TestAppendIsFollowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	writeFile(t, path, line(1))
	c := newCollector()
	start(t, path, c)
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 1 })

	appendFile(t, path, line(2))
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 2 })

	appendFile(t, path, line(3)+line(4))
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 4 })

	if got := c.lines()[3]; !strings.Contains(got, "198.51.100.4") {
		t.Fatalf("last line = %q", got)
	}
}

func TestPartialLineHeldUntilComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	writeFile(t, path, "")
	c := newCollector()
	start(t, path, c)

	full := line(7)
	cut := len(full) / 2
	appendFile(t, path, full[:cut])

	// 半行不得发布（否则会发布两次或提前暴露不完整记录）。
	time.Sleep(150 * time.Millisecond)
	if n := c.count(); n != 0 {
		t.Fatalf("partial line published early: %d records", n)
	}

	appendFile(t, path, full[cut:])
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 1 })

	// 补齐之后只发布一次。
	time.Sleep(150 * time.Millisecond)
	if n := c.count(); n != 1 {
		t.Fatalf("line published %d times, want 1", n)
	}
}

func TestOversizedLineDroppedAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	writeFile(t, path, "")
	c := newCollector()
	_, f := start(t, path, c)

	// 超过单行上限的一行，没有任何换行。
	huge := strings.Repeat("A", DefaultMaxLineBytes+1024)
	appendFile(t, path, huge)
	c.waitFor(t, 3*time.Second, func() bool { return f.Diag().OversizedLines >= 1 })

	// 该行结束后必须能继续读后续行，而不是永久退出。
	appendFile(t, path, "\n"+line(9))
	c.waitFor(t, 3*time.Second, func() bool { return c.count() >= 1 })

	if got := c.lines()[0]; !strings.Contains(got, "198.51.100.9") {
		t.Fatalf("recovery line = %q", got)
	}
	if strings.Contains(strings.Join(c.lines(), ""), "AAAA") {
		t.Fatal("oversized line content must never be published")
	}
}

func TestRotationRenameCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0.log")
	writeFile(t, path, line(1))
	c := newCollector()
	start(t, path, c)
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 1 })

	// 旧文件在轮转前又写入一行：翻新后必须先把旧文件读尽。
	appendFile(t, path, line(2))
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	writeFile(t, path, line(3))

	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 3 })
	got := strings.Join(c.lines(), "|")
	if !strings.Contains(got, "198.51.100.2") {
		t.Fatalf("pre-rotation append lost: %v", c.lines())
	}
	if !strings.Contains(got, "198.51.100.3") {
		t.Fatalf("post-rotation line missing: %v", c.lines())
	}
}

func TestRotationDiscardsResidualPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0.log")
	writeFile(t, path, line(1))
	c := newCollector()
	_, f := start(t, path, c)
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 1 })

	// 旧文件留下半行后轮转：半行不得与新文件拼接成假记录。
	appendFile(t, path, "2026/05/04 12:00:09.000000 198.51.100.77:")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	writeFile(t, path, "51234\n"+line(4))

	c.waitFor(t, 3*time.Second, func() bool { return c.count() >= 2 })
	if n := f.Diag().RotationPartial; n != 0 {
		// 也可能旧文件被补全后正常发布；两者都不允许出现拼接记录。
		t.Logf("rotation partial diagnostics = %d", n)
	}
	for _, l := range c.lines() {
		if strings.Contains(l, "198.51.100.77") && strings.Contains(l, "51234") {
			t.Fatalf("partial line was stitched across rotation: %q", l)
		}
	}
}

func TestCopyTruncateRereadsFromStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	writeFile(t, path, line(1)+line(2)+line(3))
	c := newCollector()
	_, f := start(t, path, c)
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 3 })

	// copytruncate：文件被原地清空后立刻写入新内容。
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	appendFile(t, path, line(5))

	c.waitFor(t, 3*time.Second, func() bool { return c.count() >= 4 })
	if n := f.Diag().TruncatedRetry; n == 0 {
		t.Fatal("truncation must be counted as a diagnostic")
	}
	got := strings.Join(c.lines(), "|")
	if !strings.Contains(got, "198.51.100.5") {
		t.Fatalf("post-truncate line missing: %v", c.lines())
	}
}

func TestMissingFileThenCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.log")
	c := newCollector()
	start(t, path, c)

	// 缺文件是可恢复状态，不忙循环、不退出。
	time.Sleep(100 * time.Millisecond)
	if n := c.count(); n != 0 {
		t.Fatalf("records = %d, want 0", n)
	}

	writeFile(t, path, line(1))
	c.waitFor(t, 4*time.Second, func() bool { return c.count() == 1 })

	c.mu.Lock()
	sawState := false
	for _, st := range c.states {
		if st.State == event.StateReconnecting || st.State == event.StateLive {
			sawState = true
		}
	}
	c.mu.Unlock()
	if !sawState {
		t.Fatal("source state must be reported while waiting for the file")
	}
}

func TestUnreadableFileReportsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	path := filepath.Join(t.TempDir(), "0.log")
	writeFile(t, path, line(1))
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	c := newCollector()
	start(t, path, c)

	c.waitFor(t, 3*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, st := range c.states {
			if st.State == event.StateUnreadable {
				return true
			}
		}
		return false
	})

	// 恢复权限后必须自动继续。
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	appendFile(t, path, line(2))
	c.waitFor(t, 4*time.Second, func() bool { return c.count() >= 2 })
}

func TestCancelStopsFollower(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	writeFile(t, path, line(1))
	c := newCollector()
	f := New(path, parser.New(config.FormatFakessh, time.UTC), 0, 0, 0)
	f.OnRecord = c.onRecord
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestCRLFTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	writeFile(t, path, strings.TrimSuffix(line(1), "\n")+"\r\n")
	c := newCollector()
	start(t, path, c)
	c.waitFor(t, 3*time.Second, func() bool { return c.count() == 1 })
	if got := c.lines()[0]; strings.HasSuffix(got, "\r") {
		t.Fatalf("CR must be trimmed: %q", got)
	}
}
