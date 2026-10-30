package parser

import (
	"testing"
	"time"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
)

// 本文件的样本全部是合成数据：主机名 loghost、RFC 5737 文档地址段、
// 明显虚构的凭据。真实部署抓到的主机名、来源地址与凭据不进入仓库。

// 真实部署的日志先经 rsyslog 收集，落盘时被套上一层带偏移的外壳。外壳时间
// 是唯一无歧义的时间来源，必须优先于内层无时区时间被采用。
func TestParseSyslogWrappedConnection(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	line := "2026-05-04T20:00:01.123456+08:00 loghost fakessh[4242]: " +
		"2026/05/04 12:00:01.123456 198.51.100.31:56222"
	rec := p.Parse(line)

	if rec.Kind != event.KindConnection {
		t.Fatalf("kind = %q, want connection", rec.Kind)
	}
	if rec.ParseStatus != event.ParseComplete {
		t.Fatalf("parseStatus = %q, want complete", rec.ParseStatus)
	}
	if rec.Addr == nil || rec.Addr.String() != "198.51.100.31" {
		t.Fatalf("addr = %v, want 198.51.100.31", rec.Addr)
	}
	if !rec.HasPort || rec.Port != 56222 {
		t.Fatalf("port = %d/%v, want 56222/true", rec.Port, rec.HasPort)
	}
	// 外壳偏移 +08:00 的 20:00:01 等于 UTC 12:00:01。
	eqTime(t, rec.Timestamp, utc(t, "2026-05-04T12:00:01.123456Z"))
}

func TestParseSyslogWrappedAuthAttempt(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	line := "2026-05-04T20:00:02.654321+08:00 loghost fakessh[4242]: " +
		"2026/05/04 12:00:02.654321 203.0.113.24:45802 SSH-2.0-Go user1 secret-not-real"
	rec := p.Parse(line)

	if rec.Kind != event.KindAuthAttempt {
		t.Fatalf("kind = %q, want auth_attempt", rec.Kind)
	}
	if rec.ParseStatus != event.ParsePartial || !rec.AmbiguousAuthSuffix {
		t.Fatalf("status = %q ambiguous = %v, want partial/true", rec.ParseStatus, rec.AmbiguousAuthSuffix)
	}
	if rec.ClientVersion != "SSH-2.0-Go" {
		t.Fatalf("clientVersion = %q, want SSH-2.0-Go", rec.ClientVersion)
	}
	// 分隔壳层后的正文不含外壳内容，也不含时间戳。
	if rec.Body != "203.0.113.24:45802 SSH-2.0-Go user1 secret-not-real" {
		t.Fatalf("body = %q", rec.Body)
	}
}

// 外壳时间必须压过内层时间：两者相差 8 小时，若误用内层就会把 UTC 04:00
// 当成真实时刻。
func TestParseSyslogOuterTimestampWins(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	line := "2026-01-02T01:30:00.000000+08:00 loghost fakessh[9]: 2026/01/01 17:30:00.000000 198.51.100.7:22"
	rec := p.Parse(line)

	if rec.Timestamp == nil {
		t.Fatal("timestamp is nil")
	}
	eqTime(t, rec.Timestamp, utc(t, "2026-01-01T17:30:00Z"))
}

// 内层时间在没有外壳时仍按 LOG_TIMEZONE 解释。
func TestParseBareLineStillUsesConfiguredZone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	p := New(config.FormatFakessh, loc)
	rec := p.Parse("2026/01/02 01:30:00.000000 198.51.100.7:22")

	eqTime(t, rec.Timestamp, utc(t, "2026-01-01T17:30:00Z"))
}

// 没有结束冒号或 PID 非数字的 tag 不构成外壳，不得被"看起来像"就剥掉。
func TestParseSyslogPrefixRejectsMalformedTags(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	inner := "2026/05/04 12:00:01.123456 198.51.100.31:56222"
	for _, prefix := range []string{
		"2026-05-04T20:00:01.123456+08:00 loghost fakessh ",
		"2026-05-04T20:00:01.123456+08:00 loghost fakessh[abc]: ",
		"2026-05-04T20:00:01.123456+08:00 loghost fakessh[]: ",
		"2026-05-04T20:00:01.123456+08:00 loghost fakessh[1]:",
	} {
		rec := p.Parse(prefix + inner)
		if rec.Kind != event.KindUnknown {
			t.Fatalf("prefix %q: kind = %q, want unknown (fail closed, not misfire)", prefix, rec.Kind)
		}
	}
}

// 不带偏移的外壳（RFC3339 的另一种写法）不被采信，避免把无时区时间当权威。
func TestParseSyslogPrefixRequiresOffset(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	line := "2026-05-04 20:00:01 loghost fakessh[1]: 2026/05/04 12:00:01.123456 198.51.100.31:56222"
	rec := p.Parse(line)

	if rec.Kind != event.KindUnknown {
		t.Fatalf("kind = %q, want unknown", rec.Kind)
	}
	if rec.Timestamp != nil {
		t.Fatalf("timestamp = %s, want nil", rec.Timestamp.Format(time.RFC3339Nano))
	}
}

// tag 也可以不带 PID。
func TestParseSyslogTagWithoutPID(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	line := "2026-05-04T20:00:01.123456+08:00 loghost fakessh: 2026/05/04 12:00:01.123456 198.51.100.31:56222"
	rec := p.Parse(line)

	if rec.Kind != event.KindConnection || rec.Addr == nil {
		t.Fatalf("kind = %q addr = %v, want connection with addr", rec.Kind, rec.Addr)
	}
}

// 外壳成立但内层不是 fakessh 行时，仍然判为无法识别，不猜测内容。
func TestParseSyslogWrappedGarbageStaysUnknown(t *testing.T) {
	p := New(config.FormatFakessh, time.UTC)
	line := "2026-05-04T20:00:01.123456+08:00 loghost fakessh[1]: something else entirely"
	rec := p.Parse(line)

	if rec.Kind != event.KindUnknown || rec.ParseStatus != event.ParseUnrecognized {
		t.Fatalf("kind = %q status = %q, want unknown/unrecognized", rec.Kind, rec.ParseStatus)
	}
	if rec.Addr != nil {
		t.Fatalf("addr = %v, want nil", rec.Addr)
	}
}
