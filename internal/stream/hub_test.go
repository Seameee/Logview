package stream

import (
	"testing"
	"time"

	"github.com/Seameee/Logview/internal/event"
)

func mkEvent(text string) event.Event {
	return event.Event{Type: event.TypeLog, Kind: event.KindMessage, Text: text, ObservedAt: time.Unix(0, 0).UTC()}
}

func TestAppendAssignsMonotonicIDs(t *testing.T) {
	h := NewHub()
	defer h.Close()

	first := h.Append(mkEvent("a"))
	second := h.Append(mkEvent("b"))
	if first == second {
		t.Fatalf("IDs must be unique: %q", first)
	}
	c1, err := ParseCursor(first)
	if err != nil {
		t.Fatalf("ParseCursor(%q): %v", first, err)
	}
	c2, err := ParseCursor(second)
	if err != nil {
		t.Fatalf("ParseCursor(%q): %v", second, err)
	}
	if c2.Seq != c1.Seq+1 {
		t.Fatalf("seq %d then %d, want +1", c1.Seq, c2.Seq)
	}
	if c1.Epoch != c2.Epoch || c1.Epoch == "" {
		t.Fatal("epoch must be stable and non-empty within a process")
	}
}

func TestParseCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "-", "abc", "x-1", "0123-", "a" + repeat("b", 100) + "-1", "0123-1x"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	c, err := ParseCursor("deadbeef-42")
	if err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
	if c.Epoch != "deadbeef" || c.Seq != 42 {
		t.Fatalf("got %+v", c)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func TestSnapshotIncludesRecentWindowAndWatermark(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for i := 0; i < SnapshotLimit+25; i++ {
		h.Append(mkEvent("line"))
	}
	snap, sub, err := h.Snapshot(nil, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer sub.Close()

	if len(snap.Events) != SnapshotLimit {
		t.Fatalf("snapshot events = %d, want %d", len(snap.Events), SnapshotLimit)
	}
	if !snap.HistoryIncomplete {
		t.Fatal("history must be marked incomplete when the window was truncated")
	}
	if snap.Reason != nil {
		t.Fatalf("first-connect snapshot must not carry a reset reason: %q", *snap.Reason)
	}
	hw := h.HighWater()
	if snap.Watermark != hw.String() {
		t.Fatalf("watermark = %q, want high water %q", snap.Watermark, hw.String())
	}
}

func TestSnapshotDuringAppendLosesNothing(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for i := 0; i < 10; i++ {
		h.Append(mkEvent("before"))
	}

	snap, sub, err := h.Snapshot(nil, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer sub.Close()

	// 快照与订阅注册在同一临界区内完成，因此此后的追加必然进入订阅队列。
	h.Append(mkEvent("after-1"))
	h.Append(mkEvent("after-2"))

	got := []string{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-sub.Events():
			got = append(got, ev.Text)
		case <-deadline:
			t.Fatalf("timed out, got %v", got)
		}
	}
	if got[0] != "after-1" || got[1] != "after-2" {
		t.Fatalf("events = %v", got)
	}
	if len(snap.Events) != 10 {
		t.Fatalf("snapshot carried %d events, want 10", len(snap.Events))
	}
}

func TestResumeAfterCursorIsExclusiveAndOrdered(t *testing.T) {
	h := NewHub()
	defer h.Close()
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		ids = append(ids, h.Append(mkEvent("e")))
	}
	c, err := ParseCursor(ids[1])
	if err != nil {
		t.Fatalf("ParseCursor: %v", err)
	}
	snap, sub, err := h.Snapshot(&c, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer sub.Close()

	if snap.Reason != nil {
		t.Fatalf("valid cursor must not reset: %q", *snap.Reason)
	}
	if len(snap.Events) != 3 {
		t.Fatalf("resume events = %d, want 3 (exclusive of the cursor)", len(snap.Events))
	}
	for i, ev := range snap.Events {
		wantID := ids[i+2]
		if ev.ID != wantID {
			t.Fatalf("event %d id = %q, want %q", i, ev.ID, wantID)
		}
	}
}

func TestOldEpochTriggersResetWithReason(t *testing.T) {
	h := NewHub()
	defer h.Close()
	h.Append(mkEvent("x"))

	c := Cursor{Epoch: "0000000000000000", Seq: 1}
	snap, sub, err := h.Snapshot(&c, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer sub.Close()
	if snap.Reason == nil {
		t.Fatal("old epoch must produce a reset reason")
	}
	if len(snap.Events) != 1 {
		t.Fatalf("reset snapshot events = %d, want 1", len(snap.Events))
	}
}

func TestExpiredCursorTriggersReset(t *testing.T) {
	h := NewHub()
	defer h.Close()
	// 制造淘汰：写入超过条数上限的事件，最早的一批被移出缓冲。
	for i := 0; i < MaxEvents+50; i++ {
		h.Append(mkEvent("x"))
	}
	epoch := h.HighWater().Epoch
	// 序号 1 早已被淘汰，属于"过期游标"。
	expired := Cursor{Epoch: epoch, Seq: 1}
	snap, sub, err := h.Snapshot(&expired, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer sub.Close()
	if snap.Reason == nil {
		t.Fatal("expired cursor must produce a reset reason")
	}
	if len(snap.Events) != SnapshotLimit {
		t.Fatalf("reset snapshot events = %d, want %d", len(snap.Events), SnapshotLimit)
	}
}

func TestBufferBoundsByCount(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for i := 0; i < MaxEvents+100; i++ {
		h.Append(mkEvent("x"))
	}
	if n := h.Len(); n > MaxEvents {
		t.Fatalf("buffer length = %d, want <= %d", n, MaxEvents)
	}
}

func TestBufferBoundsByBytes(t *testing.T) {
	h := NewHub()
	defer h.Close()
	big := repeat("y", 64<<10)
	for i := 0; i < 100; i++ {
		h.Append(mkEvent(big))
	}
	if n := h.Len(); n*len(big) > MaxBytes*2 {
		t.Fatalf("buffer %d events × %d bytes exceeds byte budget", n, len(big))
	}
}

func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	h := NewHub()
	defer h.Close()

	slow, slowSub, err := h.Snapshot(nil, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	_ = slow
	defer slowSub.Close()

	fast, fastSub, err := h.Snapshot(nil, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	_ = fast
	defer fastSub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberQueue*4; i++ {
			h.Append(mkEvent("x"))
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Append blocked on a slow subscriber")
	}

	// 慢订阅者应被告知溢出，而不是让它无限堆积。
	select {
	case <-slowSub.Overflow():
	case <-time.After(time.Second):
		t.Fatal("slow subscriber must be flagged as overflowed")
	}

	// 快订阅者仍能取到事件。
	select {
	case <-fastSub.Events():
	case <-time.After(time.Second):
		t.Fatal("fast subscriber received nothing")
	}
}

func TestSubscriberCloseUnsubscribes(t *testing.T) {
	h := NewHub()
	defer h.Close()
	_, sub, err := h.Snapshot(nil, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if h.Subscribers() != 1 {
		t.Fatalf("subscribers = %d, want 1", h.Subscribers())
	}
	sub.Close()
	sub.Close() // 重复关闭必须安全
	if n := h.Subscribers(); n != 0 {
		t.Fatalf("subscribers = %d after close, want 0", n)
	}
}

func TestHubCloseEndsSubscribers(t *testing.T) {
	h := NewHub()
	_, sub, err := h.Snapshot(nil, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	h.Close()
	select {
	case <-sub.Done():
	case <-time.After(time.Second):
		t.Fatal("Close must end subscriber")
	}
	if id := h.Append(mkEvent("x")); id != "" {
		t.Fatalf("Append after Close must be a no-op, got id %q", id)
	}
	if _, _, err := h.Snapshot(nil, nil); err == nil {
		t.Fatal("Snapshot after Close must fail")
	}
}

func TestStatusBroadcast(t *testing.T) {
	h := NewHub()
	defer h.Close()
	_, sub, err := h.Snapshot(nil, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer sub.Close()

	msg := "等待日志文件出现"
	h.SetStatus(event.SourceStatus{State: event.StateReconnecting, Message: &msg})

	select {
	case st := <-sub.Statuses():
		if st.Source.State != event.StateReconnecting {
			t.Fatalf("state = %q", st.Source.State)
		}
	case <-time.After(time.Second):
		t.Fatal("status not delivered")
	}

	if st, ok := h.Status(); !ok || st.State != event.StateReconnecting {
		t.Fatalf("Status() = %+v/%v", st, ok)
	}
}
