package parser

import (
	"testing"
	"time"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
)

func utc(t *testing.T, s string) *time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	u := got.UTC()
	return &u
}

func eqTime(t *testing.T, got, want *time.Time) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil || want == nil:
		t.Fatalf("timestamp mismatch: got %v want %v", fmtTime(got), fmtTime(want))
	case !got.Equal(*want):
		t.Fatalf("timestamp mismatch: got %s want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func fmtTime(p *time.Time) string {
	if p == nil {
		return "nil"
	}
	return p.Format(time.RFC3339Nano)
}

func TestParseConnectionRecord(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	rec := p.Parse("2026/05/04 12:00:01.123456 198.51.100.24:51234")

	if rec.Kind != event.KindConnection {
		t.Fatalf("kind = %q, want connection", rec.Kind)
	}
	if rec.ParseStatus != event.ParseComplete {
		t.Fatalf("parseStatus = %q, want complete", rec.ParseStatus)
	}
	if rec.Addr == nil || rec.Addr.String() != "198.51.100.24" {
		t.Fatalf("addr = %v, want 198.51.100.24", rec.Addr)
	}
	if !rec.HasPort || rec.Port != 51234 {
		t.Fatalf("port = %d/%v, want 51234/true", rec.Port, rec.HasPort)
	}
	if rec.ClientVersion != "" {
		t.Fatalf("clientVersion = %q, want empty", rec.ClientVersion)
	}
	if rec.AmbiguousAuthSuffix {
		t.Fatal("connection record must not be marked as ambiguous auth suffix")
	}
	eqTime(t, rec.Timestamp, utc(t, "2026-05-04T12:00:01.123456Z"))
}

func TestParseAuthAttemptKeepsFieldsUninterpreted(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	// 含空格与形似"最后一段就是密码"的行：不得推测字段。
	rec := p.Parse("2026/05/04 12:00:05.250000 203.0.113.9:40404 SSH-2.0-libssh2_1.11.0 admin hunter2 with spaces")

	if rec.Kind != event.KindAuthAttempt {
		t.Fatalf("kind = %q, want auth_attempt", rec.Kind)
	}
	if rec.ParseStatus != event.ParsePartial {
		t.Fatalf("parseStatus = %q, want partial（凭据后缀不可靠拆分）", rec.ParseStatus)
	}
	if !rec.AmbiguousAuthSuffix {
		t.Fatal("ambiguous suffix must be flagged")
	}
	if rec.ClientVersion != "SSH-2.0-libssh2_1.11.0" {
		t.Fatalf("clientVersion = %q, want SSH-2.0-libssh2_1.11.0", rec.ClientVersion)
	}
	if rec.Addr == nil || rec.Addr.String() != "203.0.113.9" || rec.Port != 40404 {
		t.Fatalf("addr/port = %v/%d, want 203.0.113.9/40404", rec.Addr, rec.Port)
	}
	// 可发布的结构化字段绝不能含凭据；原文只允许留在 Record 内部，
	// 由 privacy 投影丢弃（Record 本身永不进入事件缓冲）。
	for _, s := range []string{rec.ClientVersion} {
		if contains(s, "hunter2") || contains(s, "admin") {
			t.Fatalf("credential leaked into structured field: %q", s)
		}
	}
}

func TestParseIPv6WithPort(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	rec := p.Parse("2026/05/04 12:00:07.000000 [2001:db8::1]:22 SSH-2.0-Go")

	if rec.Addr == nil || rec.Addr.String() != "2001:db8::1" {
		t.Fatalf("addr = %v, want 2001:db8::1", rec.Addr)
	}
	if rec.Port != 22 || !rec.HasPort {
		t.Fatalf("port = %d, want 22", rec.Port)
	}
	if rec.Kind != event.KindAuthAttempt {
		t.Fatalf("kind = %q, want auth_attempt", rec.Kind)
	}
}

func TestParseBareIPv6WithoutPort(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	rec := p.Parse("2026/05/04 12:00:07.000000 2001:db8::1")

	if rec.Addr == nil || rec.Addr.String() != "2001:db8::1" {
		t.Fatalf("addr = %v, want 2001:db8::1", rec.Addr)
	}
	if rec.HasPort {
		t.Fatalf("hasPort = true with port %d; bare IPv6 has no port", rec.Port)
	}
	// 已知地址但缺端口：归为连接记录，状态只能是 partial（不补造端口）。
	if rec.Kind != event.KindConnection || rec.ParseStatus != event.ParsePartial {
		t.Fatalf("kind/status = %q/%q, want connection/partial", rec.Kind, rec.ParseStatus)
	}
}

func TestParseSecondPrecisionTimestamp(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	rec := p.Parse("2026/05/04 12:00:01 198.51.100.24:51234")
	eqTime(t, rec.Timestamp, utc(t, "2026-05-04T12:00:01Z"))
}

func TestParseTimeZoneInterpretation(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	p := New(config.FormatFakessh, loc)
	rec := p.Parse("2026/05/04 20:00:01.000000 198.51.100.24:51234")
	// 生产者时间按配置时区解释，再统一成 UTC。
	eqTime(t, rec.Timestamp, utc(t, "2026-05-04T12:00:01Z"))
}

func TestParseBadTimestamp(t *testing.T) {
	p := New(config.FormatAuto, time.UTC)
	cases := []string{
		"2026-05-04 12:00:01 198.51.100.24:51234",
		"not-a-time 198.51.100.24:51234",
		"",
		"   ",
	}
	for _, in := range cases {
		rec := p.Parse(in)
		if rec.Timestamp != nil {
			t.Fatalf("input %q: timestamp = %v, want nil", in, rec.Timestamp)
		}
		if rec.ParseStatus != event.ParseUnrecognized {
			t.Fatalf("input %q: parseStatus = %q, want unrecognized", in, rec.ParseStatus)
		}
		if rec.Addr != nil {
			t.Fatalf("input %q: addr = %v, want nil（不得伪造字段）", in, rec.Addr)
		}
	}
}

func TestParseAutoFallsBackToMessage(t *testing.T) {
	p := New(config.FormatAuto, time.UTC)
	rec := p.Parse("kernel: something ordinary happened")
	if rec.Kind != event.KindMessage {
		t.Fatalf("kind = %q, want message", rec.Kind)
	}
	if rec.Body != "kernel: something ordinary happened" {
		t.Fatalf("body = %q", rec.Body)
	}
	if rec.ParseStatus != event.ParseUnrecognized {
		t.Fatalf("parseStatus = %q, want unrecognized", rec.ParseStatus)
	}
	if rec.Timestamp != nil {
		t.Fatalf("timestamp = %v, want nil", rec.Timestamp)
	}
}

func TestParsePlainNeverInfersFields(t *testing.T) {
	p := New(config.FormatPlain, time.UTC)
	rec := p.Parse("2026/05/04 12:00:01.123456 198.51.100.24:51234")
	if rec.Kind != event.KindMessage {
		t.Fatalf("kind = %q, want message (plain 模式不解析)", rec.Kind)
	}
	if rec.Addr != nil || rec.HasPort {
		t.Fatalf("plain 模式不得产出地址字段: addr=%v port=%d", rec.Addr, rec.Port)
	}
	if rec.Timestamp != nil {
		t.Fatalf("plain 模式不得产出时间: %v", rec.Timestamp)
	}
}

func TestParseFakesshToleratesGarbage(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	rec := p.Parse("garbage without structure")
	if rec.Kind != event.KindUnknown {
		t.Fatalf("kind = %q, want unknown", rec.Kind)
	}
	if rec.ParseStatus != event.ParseUnrecognized {
		t.Fatalf("parseStatus = %q, want unrecognized", rec.ParseStatus)
	}
}

func TestParseEmptyAndControlBytes(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	rec := p.Parse("2026/05/04 12:00:01.000000 198.51.100.24:51234 SSH-2.0-x \x01\x02 user\x1b[31mpass")
	if rec.Kind != event.KindAuthAttempt {
		t.Fatalf("kind = %q, want auth_attempt", rec.Kind)
	}
	// 控制字符不影响结构识别，但也不得被当成可靠字段。
	if rec.ParseStatus != event.ParsePartial {
		t.Fatalf("parseStatus = %q, want partial", rec.ParseStatus)
	}
}

func TestParseEmptyClientVersionProducesNoVersion(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	// 空版本会形成连续空格；不得把用户名当版本。
	rec := p.Parse("2026/05/04 12:00:01.000000 198.51.100.24:51234  root pw")
	if rec.ClientVersion != "" {
		t.Fatalf("clientVersion = %q, want empty", rec.ClientVersion)
	}
	if rec.Kind != event.KindAuthAttempt {
		t.Fatalf("kind = %q, want auth_attempt", rec.Kind)
	}
}

func TestParsePortBoundaries(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	bad := []string{
		"2026/05/04 12:00:01.000000 198.51.100.24:0",
		"2026/05/04 12:00:01.000000 198.51.100.24:65536",
		"2026/05/04 12:00:01.000000 198.51.100.24:abc",
		"2026/05/04 12:00:01.000000 198.51.100.24:",
	}
	for _, in := range bad {
		rec := p.Parse(in)
		if rec.HasPort {
			t.Fatalf("input %q: hasPort = true, want false", in)
		}
		if rec.ParseStatus == event.ParseComplete {
			t.Fatalf("input %q: status complete on invalid port", in)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
