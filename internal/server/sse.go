package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/stream"
)

// writeTimeout 是单次写入的期限。超过即认为客户端过慢，断开让其重连补发
// 快照，而不是无限占用内存和连接。
const writeTimeout = 10 * time.Second

// handleStream 提供 GET /api/stream。
//
// 契约要点：先发 snapshot（带标准 id:），再按追加序补发日志与状态；
// Last-Event-ID 能可靠解析且未过期时只补发其后的日志；不能用时重发快照
// 并在 reason 中说明原因。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming_unsupported"})
		return
	}

	// 有界并发：超限立即拒绝，不排队占用资源。
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too_many_streams"})
		return
	}

	var cursor *stream.Cursor
	var resetHint *string
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		if c, err := stream.ParseCursor(raw); err == nil {
			cursor = &c
		} else {
			// 非法/超长游标：不用它做任何解析，直接发新快照。
			reason := "游标无法识别，已发送新的快照"
			resetHint = &reason
		}
	}

	snap, sub, err := s.hub.Snapshot(cursor, resetHint)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "stream_unavailable"})
		return
	}
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	setSecurityHeaders(w)

	ctl := http.NewResponseController(w)
	if err := ctl.Flush(); err != nil {
		return
	}

	// 快照补齐运行时策略：Hub 只持有事件，隐私与状态由这里的进程状态补充。
	st, hasSt := s.hub.Status()
	if !hasSt {
		st = event.SourceStatus{State: event.StateConnecting}
	}
	snap.Source = st
	snap.Privacy = s.privacy

	if err := writeEvent(w, flusher, ctl, "snapshot", snap.Watermark, snap); err != nil {
		return
	}

	beat := time.NewTicker(s.beat)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-sub.Done():
			return

		case <-sub.Overflow():
			// 订阅者排队溢出：断开，让浏览器重连并收到新快照。
			return

		case ev := <-sub.Events():
			payload := eventStreamEvent{Type: event.TypeLog, Event: ev}
			if err := writeEvent(w, flusher, ctl, "log", ev.ID, payload); err != nil {
				return
			}

		case stEv := <-sub.Statuses():
			if err := writeEvent(w, flusher, ctl, "status", "", stEv); err != nil {
				return
			}

		case <-beat.C:
			// 心跳用注释行，不占用事件 ID 空间。
			if err := writeRaw(w, flusher, ctl, []byte(": heartbeat\n\n")); err != nil {
				return
			}
		}
	}
}

// eventStreamEvent 是 SSE 中 log 事件的包装：type 与事件体同层展开。
type eventStreamEvent struct {
	Type string `json:"type"`
	event.Event
}

// writeEvent 序列化并写出一个 SSE 事件，随后立即 flush。
func writeEvent(w http.ResponseWriter, flusher http.Flusher, ctl *http.ResponseController, eventName, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(b)+64)
	if id != "" {
		buf = append(buf, "id: "...)
		buf = append(buf, id...)
		buf = append(buf, '\n')
	}
	buf = append(buf, "event: "...)
	buf = append(buf, eventName...)
	buf = append(buf, '\n')
	buf = append(buf, "data: "...)
	buf = append(buf, b...)
	buf = append(buf, '\n', '\n')

	if err := rawWrite(w, ctl, buf); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeRaw(w http.ResponseWriter, flusher http.Flusher, ctl *http.ResponseController, b []byte) error {
	if err := rawWrite(w, ctl, b); err != nil {
		return err
	}
	// 心跳若不 flush，会滞留在响应缓冲区里直到写满，等于没有心跳。
	flusher.Flush()
	return nil
}

// rawWrite 设置单次写期限并写入。
func rawWrite(w http.ResponseWriter, ctl *http.ResponseController, b []byte) error {
	_ = ctl.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := w.Write(b)
	return err
}
