// Package stream 维护有界事件缓冲、游标与订阅。
//
// 全服务只有一份缓冲；浏览器只订阅游标，不各自持有读取链路。
// 锁内只做短暂的缓冲与订阅操作，序列化和写网络都在锁外完成。
package stream

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/Seameee/Logview/internal/event"
)

const (
	// MaxEvents 是服务端缓冲的条数上限。
	MaxEvents = 1000
	// MaxBytes 是服务端缓冲的字节上限（估算值）。
	MaxBytes = 4 << 20 // 4 MiB
	// SnapshotLimit 是首连快照中携带的历史条数上限。
	SnapshotLimit = 100
	// subscriberQueue 是每个订阅者的有界排队长度。
	subscriberQueue = 256
	// eventByteOverhead 是单条事件的结构性开销估算，用于字节配额。
	eventByteOverhead = 128
)

// ErrCursorExpired 表示请求的游标已不在缓冲内。
var ErrCursorExpired = errors.New("cursor expired")

// ErrClosed 表示缓冲区已关闭。
var ErrClosed = errors.New("stream closed")

// Hub 是有界事件缓冲与订阅中心。
type Hub struct {
	mu       sync.Mutex
	epoch    string
	seq      uint64
	events   []event.Event
	bytes    int
	subs     map[*Subscriber]struct{}
	closed   bool
	onDrop   func() // 诊断回调：因容量淘汰记录
	status   event.SourceStatus
	hasState bool
}

// NewHub 创建一个带随机 epoch 的缓冲区。epoch 用于让浏览器识别重启。
func NewHub() *Hub {
	return &Hub{
		epoch:  newEpoch(),
		events: make([]event.Event, 0, MaxEvents),
		subs:   make(map[*Subscriber]struct{}),
	}
}

func newEpoch() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}

// Epoch 返回本进程的随机 epoch。
func (h *Hub) Epoch() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.epoch
}

// SetDropHook 注册淘汰计数回调。必须在开始消费前设置。
func (h *Hub) SetDropHook(fn func()) {
	h.mu.Lock()
	h.onDrop = fn
	h.mu.Unlock()
}

// SetStatus 记录采集源状态并广播给订阅者。
//
// 状态事件不带日志序号：它不占用日志的去重/排序空间，只是随连接推送的
// 实时状况；首连的权威状态在快照里。
func (h *Hub) SetStatus(st event.SourceStatus) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.status = st
	h.hasState = true
	subs := make([]*Subscriber, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()

	ev := event.StatusEvent{Type: event.TypeStatus, Source: st}
	for _, sub := range subs {
		sub.pushStatus(ev)
	}
}

// Status 返回最近已知的源状态。
func (h *Hub) Status() (event.SourceStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status, h.hasState
}

// Cursor 是 epoch 与序号的组合。
type Cursor struct {
	Epoch string
	Seq   uint64
}

// ParseCursor 解析 "epoch-seq" 形式的游标。
//
// 非法或过长的输入一律返回 ErrCursorExpired，不触发无界分配或磁盘访问。
func ParseCursor(s string) (Cursor, error) {
	if s == "" || len(s) > 64 {
		return Cursor{}, ErrCursorExpired
	}
	i := len(s) - 1
	for i >= 0 && s[i] != '-' {
		i--
	}
	if i <= 0 || i == len(s)-1 {
		return Cursor{}, ErrCursorExpired
	}
	epoch := s[:i]
	num := s[i+1:]
	if len(epoch) > 32 || len(num) > 20 {
		return Cursor{}, ErrCursorExpired
	}
	for k := 0; k < len(epoch); k++ {
		c := epoch[k]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return Cursor{}, ErrCursorExpired
		}
	}
	var seq uint64
	for k := 0; k < len(num); k++ {
		c := num[k]
		if c < '0' || c > '9' {
			return Cursor{}, ErrCursorExpired
		}
		seq = seq*10 + uint64(c-'0')
	}
	return Cursor{Epoch: epoch, Seq: seq}, nil
}

// String 输出用于 SSE id: 的游标。
func (c Cursor) String() string { return c.Epoch + "-" + utoa(c.Seq) }

func utoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Append 追加一条事件并通知订阅者。事件 ID 在此统一分配。
func (h *Hub) Append(ev event.Event) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ""
	}
	h.seq++
	ev.ID = h.epoch + "-" + utoa(h.seq)
	h.events = append(h.events, ev)
	h.bytes += estimateSize(ev)

	dropped := false
	for len(h.events) > MaxEvents || h.bytes > MaxBytes {
		if len(h.events) <= 1 {
			break
		}
		h.bytes -= estimateSize(h.events[0])
		h.events[0] = event.Event{} // 及时释放引用
		h.events = h.events[1:]
		dropped = true
	}
	if dropped && h.onDrop != nil {
		h.onDrop()
	}

	for sub := range h.subs {
		sub.push(ev)
	}
	return ev.ID
}

// Snapshot 返回首连/重置快照，并把订阅注册放在同一个同步边界里。
//
// cursor 为 nil 表示新客户端：返回最近窗口。cursor 可用时只补发其后的
// 事件，并把高水位作为快照的 watermark，从而不漏行也不重复。
func (h *Hub) Snapshot(cursor *Cursor, resetReason *string) (*event.Snapshot, *Subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, ErrClosed
	}

	var pending []event.Event
	reason := resetReason
	watermark := Cursor{Epoch: h.epoch, Seq: h.seq}

	if cursor != nil && cursor.Epoch == h.epoch && cursor.Seq <= h.seq && h.cursorUsable(*cursor) {
		for _, ev := range h.events {
			if seqOf(ev.ID) > cursor.Seq {
				pending = append(pending, ev)
			}
		}
	} else {
		start := len(h.events) - SnapshotLimit
		if start < 0 {
			start = 0
		}
		pending = append(pending, h.events[start:]...)
		if cursor != nil && reason == nil {
			r := "旧 epoch、过期游标或订阅溢出，已发送新的快照"
			reason = &r
		}
	}

	sub := newSubscriber(h)
	h.subs[sub] = struct{}{}

	// 历史不完整的判定：本进程已产生的事件多于本次窗口携带的数量。
	incomplete := h.seq > uint64(len(pending))

	snap := &event.Snapshot{
		Type:              event.TypeSnapshot,
		Events:            pending,
		Watermark:         watermark.String(),
		Reason:            reason,
		HistoryIncomplete: incomplete,
	}
	return snap, sub, nil
}

// cursorUsable 判断游标之后的事件是否仍完整保留在缓冲内。
func (h *Hub) cursorUsable(c Cursor) bool {
	if len(h.events) == 0 {
		return c.Seq == h.seq
	}
	oldest := seqOf(h.events[0].ID)
	return c.Seq+1 >= oldest || oldest == 0
}

// Recent 返回不超过 limit 条的最近事件副本（供测试与诊断）。
func (h *Hub) Recent(limit int) []event.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	if limit > len(h.events) {
		limit = len(h.events)
	}
	if limit <= 0 {
		return nil
	}
	out := make([]event.Event, limit)
	copy(out, h.events[len(h.events)-limit:])
	return out
}

// Len 返回当前缓冲条数。
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events)
}

// HighWater 返回当前高水位游标。
func (h *Hub) HighWater() Cursor {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Cursor{Epoch: h.epoch, Seq: h.seq}
}

// Subscribers 返回当前订阅者数量。
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Close 关闭缓冲并结束所有订阅。之后不再接受追加。
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*Subscriber, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.subs = make(map[*Subscriber]struct{})
	h.mu.Unlock()

	for _, sub := range subs {
		sub.Close()
	}
}

// unsubscribe 由 Subscriber.Close 调用。
func (h *Hub) unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

// Subscriber 是一个有界排队的订阅者。
//
// 队列满时置溢出标记并丢弃新事件，由读端断开并让客户端重同步；
// 绝不积累无限队列，慢客户端也不会阻塞源和其他客户端。
type Subscriber struct {
	ch       chan event.Event
	statusCh chan event.StatusEvent
	hub      *Hub
	done     chan struct{}
	overflow chan struct{}
	once     sync.Once
	ofOnce   sync.Once
}

func newSubscriber(h *Hub) *Subscriber {
	return &Subscriber{
		// 预留一格：push 在持锁状态下非阻塞写入，读端可能略慢。
		ch:       make(chan event.Event, subscriberQueue),
		statusCh: make(chan event.StatusEvent, 4),
		hub:      h,
		done:     make(chan struct{}),
		overflow: make(chan struct{}),
	}
}

// Events 返回事件通道。
func (s *Subscriber) Events() <-chan event.Event { return s.ch }

// Statuses 返回源状态通道。状态只保留最新几条，丢弃旧的不会丢信息。
func (s *Subscriber) Statuses() <-chan event.StatusEvent { return s.statusCh }

// Overflow 返回溢出信号通道。
func (s *Subscriber) Overflow() <-chan struct{} { return s.overflow }

// push 由 Hub 在持锁状态下调用；非阻塞。
func (s *Subscriber) push(ev event.Event) {
	select {
	case s.ch <- ev:
	default:
		s.ofOnce.Do(func() { close(s.overflow) })
	}
}

// pushStatus 非阻塞投递状态；队列满则丢掉最旧的一条。
func (s *Subscriber) pushStatus(ev event.StatusEvent) {
	for {
		select {
		case s.statusCh <- ev:
			return
		default:
			select {
			case <-s.statusCh:
			default:
			}
		}
	}
}

// Close 取消订阅。可重复调用。
func (s *Subscriber) Close() {
	s.once.Do(func() {
		s.hub.unsubscribe(s)
		close(s.done)
	})
}

// Done 返回关闭信号。
func (s *Subscriber) Done() <-chan struct{} { return s.done }

// estimateSize 粗略估算单条事件的序列化尺寸，用于字节配额。
func estimateSize(ev event.Event) int {
	n := eventByteOverhead + len(ev.Text) + len(ev.ID)
	if ev.Source != nil {
		n += len(ev.Source.Addr)
	}
	if ev.Details != nil {
		if ev.Details.Version != nil {
			n += len(*ev.Details.Version)
		}
		if ev.Details.Note != nil {
			n += len(*ev.Details.Note)
		}
	}
	return n
}

// seqOf 从 "epoch-seq" 中取出序号。
func seqOf(id string) uint64 {
	start := 0
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '-' {
			start = i + 1
			break
		}
	}
	var seq uint64
	for k := start; k < len(id); k++ {
		c := id[k]
		if c < '0' || c > '9' {
			return 0
		}
		seq = seq*10 + uint64(c-'0')
	}
	return seq
}

// NowUTC 是服务端接收时间的统一来源，便于测试替换。
var NowUTC = func() time.Time { return time.Now().UTC() }
