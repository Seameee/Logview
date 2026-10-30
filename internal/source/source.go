// Package source 负责跟随一个固定路径的日志文件。
//
// 全服务只有这一条读取链路：不按浏览器数量启动 shell、tail、PTY 或独立
// 跟随器。一个客户端暂停、断开或变慢都不会阻塞这里。
package source

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/parser"
)

const (
	// DefaultMaxLineBytes 是单行上限。超长行丢弃到换行并计数，随后继续读；
	// 采集不会因为某一行过长而永久退出。
	DefaultMaxLineBytes = 64 << 10

	// DefaultTailLines 是启动尾读的最大完整记录数。
	DefaultTailLines = 100

	// DefaultTailBytes 是启动尾读的字节上限，避免为取末尾若干行扫描整个
	// 超大文件。
	DefaultTailBytes = int64(512 << 10)

	minRetryDelay = 200 * time.Millisecond
	maxRetryDelay = 3 * time.Second
	// pollInterval 是到达 EOF 后的轮转/截断检查间隔。
	pollInterval = 200 * time.Millisecond
	readChunk    = 32 << 10
)

// DiagKind 是诊断计数种类。
type DiagKind int

const (
	// DiagOversizedLine 表示因超过单行上限被丢弃的行。
	DiagOversizedLine DiagKind = iota
	// DiagRotationPartial 表示轮转时丢弃的残留半行。
	DiagRotationPartial
	// DiagTruncatedRetry 表示检测到 copytruncate 后从头重读的次数。
	DiagTruncatedRetry
	// DiagKindCount 是种类数量。
	DiagKindCount
)

// Diag 是累计诊断计数。
type Diag struct {
	OversizedLines  int64
	RotationPartial int64
	TruncatedRetry  int64
}

// Follower 跟随单个文件。
type Follower struct {
	path      string
	parse     *parser.Parser
	maxLine   int
	tailLines int
	tailBytes int64

	// OnRecord 收到一条完整记录。它在读取协程里同步调用，必须快速返回。
	OnRecord func(rec parser.Record)
	// OnStatus 在源状态变化时调用。
	OnStatus func(st event.SourceStatus)
	// OnHistoryIncomplete 在尾读未能覆盖全部历史时调用一次。
	OnHistoryIncomplete func()

	mu    sync.Mutex
	diag  Diag
	state event.SourceState
	since time.Time
}

// New 创建跟随器。maxLine/tailLines/tailBytes 传 0 表示用默认值。
func New(path string, p *parser.Parser, maxLine, tailLines int, tailBytes int64) *Follower {
	if maxLine <= 0 {
		maxLine = DefaultMaxLineBytes
	}
	if tailLines <= 0 {
		tailLines = DefaultTailLines
	}
	if tailBytes <= 0 {
		tailBytes = DefaultTailBytes
	}
	return &Follower{
		path:      path,
		parse:     p,
		maxLine:   maxLine,
		tailLines: tailLines,
		tailBytes: tailBytes,
		state:     event.StateConnecting,
		since:     time.Now().UTC(),
	}
}

// Diag 返回累计诊断计数的副本。
func (f *Follower) Diag() Diag {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diag
}

// Status 返回当前状态。
func (f *Follower) Status() event.SourceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	since := f.since
	return event.SourceStatus{State: f.state, Message: nil, Since: &since}
}

func (f *Follower) addDiag(kind DiagKind) {
	f.mu.Lock()
	switch kind {
	case DiagOversizedLine:
		f.diag.OversizedLines++
	case DiagRotationPartial:
		f.diag.RotationPartial++
	case DiagTruncatedRetry:
		f.diag.TruncatedRetry++
	}
	f.mu.Unlock()
}

// setStatus 只在状态真正变化时回调，避免重复刷屏。
func (f *Follower) setStatus(state event.SourceState, msg string) {
	f.mu.Lock()
	changed := f.state != state
	f.state = state
	if changed {
		f.since = time.Now().UTC()
	}
	since := f.since
	f.mu.Unlock()

	if !changed || f.OnStatus == nil {
		return
	}
	st := event.SourceStatus{State: state, Since: &since}
	if msg != "" {
		st.Message = &msg
	}
	f.OnStatus(st)
}

// Run 阻塞运行直到 ctx 被取消。
//
// 缺文件、权限不足属可恢复状态：低频重试，不忙循环。SIGTERM/SIGINT 取消
// ctx 后本函数返回，由调用方继续有界关闭 HTTP。
func (f *Follower) Run(ctx context.Context) {
	var (
		pending    []byte
		discarding bool
		firstOpen  = true
		backoff    = minRetryDelay
	)

	for ctx.Err() == nil {
		fh, err := os.Open(f.path)
		var info os.FileInfo
		if err == nil {
			info, err = fh.Stat()
			if err != nil {
				_ = fh.Close()
				fh = nil
			}
		}
		if err != nil {
			f.setStatus(stateForOpenError(err), msgForOpenError(err))
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextDelay(backoff)
			continue
		}
		backoff = minRetryDelay

		startAt := int64(0)
		pending = pending[:0]
		discarding = false
		if firstOpen {
			startAt, pending, discarding = f.readTail(fh, info)
			firstOpen = false
		}

		if _, err := fh.Seek(startAt, io.SeekStart); err != nil {
			_ = fh.Close()
			continue
		}
		f.setStatus(event.StateLive, "")

		reopen := f.stream(ctx, fh, info, &pending, &discarding)
		_ = fh.Close()
		if ctx.Err() != nil {
			return
		}
		if !reopen {
			return
		}
	}
}

// stream 从 fh 当前位置读到 EOF 并跟随追加。返回 true 表示需要重新打开文件。
func (f *Follower) stream(ctx context.Context, fh *os.File, info os.FileInfo, pending *[]byte, discarding *bool) bool {
	buf := make([]byte, readChunk)
	for ctx.Err() == nil {
		n, err := fh.Read(buf)
		if n > 0 {
			f.consume(buf[:n], pending, discarding)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			f.setStatus(event.StateUnreadable, "读取日志文件失败，正在重试")
			return true
		}
		if n > 0 {
			continue // 可能还有数据，继续读直到 EOF
		}
		if !errors.Is(err, io.EOF) {
			continue
		}

		// 已到 EOF：检查轮转与截断。
		switch f.detect(fh, info) {
		case rotReplaced, rotRemoved:
			// 轮转：把旧文件读到确实没有新数据再重新打开。
			f.drain(fh, pending, discarding)
			// 轮转残留的半行不与新文件拼接，丢弃并计入诊断。
			if len(*pending) > 0 {
				f.addDiag(DiagRotationPartial)
				*pending = (*pending)[:0]
				*discarding = false
			}
			return true
		case rotTruncated:
			// copytruncate：长度小于偏移，从头读。
			f.addDiag(DiagTruncatedRetry)
			if _, err := fh.Seek(0, io.SeekStart); err != nil {
				return true
			}
			*pending = (*pending)[:0]
			*discarding = false
			continue
		case rotNone:
			if !sleepCtx(ctx, pollInterval) {
				return false
			}
		}
	}
	return false
}

// drain 反复读取直到连续两次没有新数据，尽量读尽旧文件。
func (f *Follower) drain(fh *os.File, pending *[]byte, discarding *bool) {
	buf := make([]byte, readChunk)
	for i := 0; i < 8; i++ {
		n, err := fh.Read(buf)
		if n > 0 {
			f.consume(buf[:n], pending, discarding)
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return
		}
		// 一次空读之后再确认一次，减少"切换瞬间又写入"的丢失。
		if n == 0 {
			n2, err2 := fh.Read(buf)
			if n2 > 0 {
				f.consume(buf[:n2], pending, discarding)
				continue
			}
			if err2 != nil && !errors.Is(err2, io.EOF) {
				return
			}
			return
		}
	}
}

// rotation 是轮转检测结果。
type rotation int

const (
	rotNone rotation = iota
	rotReplaced
	rotRemoved
	rotTruncated
)

// detect 比较固定路径与已打开文件的身份，并检查是否被截断。
func (f *Follower) detect(fh *os.File, info os.FileInfo) rotation {
	cur, err := os.Stat(f.path)
	if err != nil {
		return rotRemoved
	}
	if !os.SameFile(info, cur) {
		return rotReplaced
	}
	pos, err := fh.Seek(0, io.SeekCurrent)
	if err != nil {
		return rotNone
	}
	st, err := fh.Stat()
	if err != nil {
		return rotNone
	}
	if st.Size() < pos {
		return rotTruncated
	}
	return rotNone
}

// consume 把字节流切成完整行并发布。未以换行结束的尾部留在 pending。
func (f *Follower) consume(chunk []byte, pending *[]byte, discarding *bool) {
	for len(chunk) > 0 {
		if *discarding {
			// 丢弃到下一个换行，随后恢复读。
			i := bytes.IndexByte(chunk, '\n')
			if i < 0 {
				return
			}
			chunk = chunk[i+1:]
			*discarding = false
			continue
		}

		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			if len(*pending)+len(chunk) > f.maxLine {
				f.addDiag(DiagOversizedLine)
				*pending = (*pending)[:0]
				*discarding = true
				return
			}
			*pending = append(*pending, chunk...)
			return
		}

		line := chunk[:i]
		chunk = chunk[i+1:]
		if len(*pending)+len(line) > f.maxLine {
			f.addDiag(DiagOversizedLine)
			*pending = (*pending)[:0]
			continue
		}
		if len(*pending) > 0 {
			*pending = append(*pending, line...)
			line = *pending
			*pending = (*pending)[:0]
		}
		f.emit(line)
	}
}

// emit 发布一条完整记录。
func (f *Follower) emit(line []byte) {
	// 兼容 CRLF：只去掉一个行尾 \r。
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	rec := f.parse.Parse(string(line))
	if f.OnRecord != nil {
		f.OnRecord(rec)
	}
}

// readTail 读取文件末尾的完整记录窗口，返回起始偏移、待续半行与丢弃标记。
//
// 只读取尾部 tailBytes 字节，不为取末尾若干行而扫描整个文件。
func (f *Follower) readTail(fh *os.File, info os.FileInfo) (int64, []byte, bool) {
	size := info.Size()
	if size <= 0 {
		return 0, nil, false
	}

	start := size - f.tailBytes
	if start < 0 {
		start = 0
	}
	window := make([]byte, size-start)
	if _, err := fh.ReadAt(window, start); err != nil && !errors.Is(err, io.EOF) {
		// 读失败时交给 stream 从当前位置重试。
		return 0, nil, false
	}

	begin := 0
	if start > 0 {
		// 窗口起点落在某行中间，丢掉这段不完整的前缀。
		i := bytes.IndexByte(window, '\n')
		if i < 0 {
			return size, nil, false
		}
		begin = i + 1
	}

	body := window[begin:]
	// 最后一段没有以换行结束，属于"半行"，保留等待补齐。
	lastNL := bytes.LastIndexByte(body, '\n')
	var complete, tail []byte
	if lastNL < 0 {
		complete = nil
		tail = body
	} else {
		complete = body[:lastNL+1]
		tail = body[lastNL+1:]
	}

	lines := splitLines(complete)
	totalLines := len(lines)
	from := 0
	if totalLines > f.tailLines {
		from = totalLines - f.tailLines
	}
	for _, ln := range lines[from:] {
		f.emit(ln)
	}

	// 历史不完整：尾读窗口被截断，或还有更早的完整记录未包含在窗口里。
	if start > 0 || totalLines > f.tailLines {
		if f.OnHistoryIncomplete != nil {
			f.OnHistoryIncomplete()
		}
	}

	if len(tail) > f.maxLine {
		f.addDiag(DiagOversizedLine)
		return size, nil, true
	}
	return size, append([]byte(nil), tail...), false
}

// splitLines 按 \n 切分，返回不含换行符的各行（末尾多余空段丢弃）。
func splitLines(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	if b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return nil
	}
	return bytes.Split(b, []byte{'\n'})
}

func stateForOpenError(err error) event.SourceState {
	if errors.Is(err, fs.ErrNotExist) {
		return event.StateReconnecting
	}
	if errors.Is(err, fs.ErrPermission) {
		return event.StateUnreadable
	}
	return event.StateReconnecting
}

// msgForOpenError 生成给用户看的说明，不含宿主绝对路径。
func msgForOpenError(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "等待日志文件出现"
	case errors.Is(err, fs.ErrPermission):
		return "无法读取日志文件：请检查文件属组或 ACL"
	default:
		return "日志文件暂时不可读，稍后重试"
	}
}

func nextDelay(d time.Duration) time.Duration {
	d *= 2
	if d > maxRetryDelay {
		d = maxRetryDelay
	}
	return d
}

// sleepCtx 休眠并响应取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
