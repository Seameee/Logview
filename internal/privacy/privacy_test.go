package privacy

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/parser"
)

func mustProj(t *testing.T, mode config.Mode, words []string, details bool) *Projector {
	t.Helper()
	p, err := New(mode, words, details)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func addrRec(addr string, port uint16, hasPort bool) parser.Record {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		panic(err)
	}
	return parser.Record{
		Kind:        event.KindConnection,
		ParseStatus: event.ParseComplete,
		Addr:        &a,
		Port:        port,
		HasPort:     hasPort,
	}
}

var now = time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

// TestAnonymizeModesCoverSourceAndText 验证三种模式对"结构化地址"和"文本中
// 出现的同一地址"使用一致的展示值，即两个公开字段都被覆盖。
func TestAnonymizeModesCoverSourceAndText(t *testing.T) {
	cases := []struct {
		name     string
		mode     config.Mode
		wantAddr string
	}{
		{"partial", config.ModePartial, "198.51.100.xxx"},
		{"full", config.ModeFull, v4FullPlaceholder},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := mustProj(t, c.mode, nil, true)
			rec := addrRec("198.51.100.24", 51234, true)
			// 用认证尝试记录验证文本投影：连接记录按设计不带文本。
			rec.Kind = event.KindAuthAttempt
			rec.ParseStatus = event.ParsePartial
			rec.Body = "connection from 198.51.100.24:51234 seen"
			ev := p.Project(rec, now)

			if ev.Source == nil || ev.Source.Addr != c.wantAddr {
				t.Fatalf("source.addr = %v, want %s", ev.Source, c.wantAddr)
			}
			if strings.Contains(ev.Text, "198.51.100.24") {
				t.Fatalf("original IP leaked into text: %q", ev.Text)
			}
			if !strings.Contains(ev.Text, c.wantAddr) {
				t.Fatalf("text %q does not carry the projected address %q", ev.Text, c.wantAddr)
			}
		})
	}
}

func TestHashModeIsStableWithinProcessAndHidesAddress(t *testing.T) {
	p := mustProj(t, config.ModeHash, nil, true)
	a := p.Project(addrRec("198.51.100.24", 1, true), now)
	b := p.Project(addrRec("198.51.100.24", 2, true), now)
	if a.Source == nil || b.Source == nil {
		t.Fatal("source must be present")
	}
	if a.Source.Addr != b.Source.Addr {
		t.Fatalf("same address must hash identically: %q vs %q", a.Source.Addr, b.Source.Addr)
	}
	if !strings.HasPrefix(a.Source.Addr, "[H:") || strings.Contains(a.Source.Addr, "198.51.100.24") {
		t.Fatalf("hash form invalid or leaks original: %q", a.Source.Addr)
	}
	// 不同地址必须不同（避免把所有地址压成同一个值）。
	other := p.Project(addrRec("203.0.113.9", 1, true), now)
	if other.Source.Addr == a.Source.Addr {
		t.Fatal("distinct addresses must not collide")
	}
}

func TestHashModeIsNotStableAcrossRestarts(t *testing.T) {
	p1 := mustProj(t, config.ModeHash, nil, true)
	p2 := mustProj(t, config.ModeHash, nil, true)
	a := p1.Project(addrRec("198.51.100.24", 1, true), now)
	b := p2.Project(addrRec("198.51.100.24", 1, true), now)
	if a.Source.Addr == b.Source.Addr {
		t.Fatal("hash must change across restarts (key is not persisted)")
	}
}

func TestIPv6Projection(t *testing.T) {
	p := mustProj(t, config.ModePartial, nil, true)
	rec := addrRec("2001:db8:1234:5678::1", 22, true)
	ev := p.Project(rec, now)
	if ev.Source == nil {
		t.Fatal("source must be present")
	}
	if strings.Contains(ev.Source.Addr, "5678::1") {
		t.Fatalf("IPv6 tail leaked: %q", ev.Source.Addr)
	}
	if !strings.HasPrefix(ev.Source.Addr, "2001:db8:1234") {
		t.Fatalf("partial IPv6 must keep /48: %q", ev.Source.Addr)
	}

	full := mustProj(t, config.ModeFull, nil, true).Project(rec, now)
	if full.Source.Addr != v6FullPlaceholder {
		t.Fatalf("full IPv6 = %q, want %q", full.Source.Addr, v6FullPlaceholder)
	}
}

func TestIPv4MappedNormalized(t *testing.T) {
	p := mustProj(t, config.ModePartial, nil, true)
	rec := addrRec("::ffff:198.51.100.24", 1, true)
	ev := p.Project(rec, now)
	if ev.Source.Addr != "198.51.100.xxx" {
		t.Fatalf("mapped address = %q, want IPv4-style partial form", ev.Source.Addr)
	}
}

// TestKeywordLiteralMatching 覆盖 sed 元字符、反斜杠、引号与重叠词。
func TestKeywordLiteralMatching(t *testing.T) {
	words := []string{"a/b", "x.y", "q*z", "m&n", `back\slash`, `quo"te`, "Leaseweb", "LeasewebSG"}
	p := mustProj(t, config.ModePartial, words, true)

	in := `a/b x.y q*z m&n back\slash quo"te LeasewebSG Leaseweb`
	got := p.projectString(in)

	if strings.Contains(got, "a/b") || strings.Contains(got, "x.y") || strings.Contains(got, "q*z") ||
		strings.Contains(got, "m&n") || strings.Contains(got, `back\slash`) || strings.Contains(got, `quo"te`) {
		t.Fatalf("literal keyword survived: %q", got)
	}
	if strings.Contains(got, "LeasewebSG") {
		t.Fatalf("longer keyword must win over its prefix: %q", got)
	}
	if strings.Contains(got, "Leaseweb") {
		t.Fatalf("shorter keyword survived: %q", got)
	}
	// 每个词替换成一个固定占位符，不是删除。
	if n := strings.Count(got, filteredMarker); n != 8 {
		t.Fatalf("marker count = %d, want 8 in %q", n, got)
	}
}

func TestKeywordMatchingIsCaseSensitive(t *testing.T) {
	p := mustProj(t, config.ModePartial, []string{"Leaseweb"}, true)
	got := p.projectString("Leaseweb leaseweb LEASEWEB")
	if !strings.Contains(got, "leaseweb") || !strings.Contains(got, "LEASEWEB") {
		t.Fatalf("matching must be case sensitive: %q", got)
	}
	if strings.Contains(got, "Leaseweb") {
		t.Fatalf("exact case must be replaced: %q", got)
	}
}

func TestKeywordMarkerNotReprocessed(t *testing.T) {
	// 关键词是占位符自身的子串时也不能产生递归替换。
	p := mustProj(t, config.ModePartial, []string{"filtered", "ed"}, true)
	got := p.projectString("filtered")
	if got != filteredMarker {
		t.Fatalf("got %q, want single %q", got, filteredMarker)
	}
}

func TestControlCharactersEscaped(t *testing.T) {
	p := mustProj(t, config.ModePartial, nil, true)
	in := "a\x1b[31mred\x07\tb\u202eevil\u009bend"
	got := p.projectString(in)

	for _, raw := range []string{"\x1b", "\x07", "\t", "\u202e", "\u009b"} {
		if strings.Contains(got, raw) {
			t.Fatalf("raw control %q survived in %q", raw, got)
		}
	}
	for _, want := range []string{`\x1b`, `\x07`, `\x09`, `\u202e`, `\u009b`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing escape %q in %q", want, got)
		}
	}
}

func TestDetailsHiddenByDefault(t *testing.T) {
	p := mustProj(t, config.ModePartial, nil, false)

	auth := parser.Record{
		Kind:                event.KindAuthAttempt,
		ParseStatus:         event.ParsePartial,
		AmbiguousAuthSuffix: true,
		Body:                "198.51.100.24:51234 SSH-2.0-x root s3cret",
		ClientVersion:       "SSH-2.0-x",
	}
	ev := p.Project(auth, now)
	if ev.Text != "" {
		t.Fatalf("text must be empty when details hidden: %q", ev.Text)
	}
	if ev.Details == nil || ev.Details.Note == nil {
		t.Fatal("hidden details must carry a note")
	}
	if ev.Details.Version != nil {
		t.Fatalf("version must not be exposed while details hidden: %q", *ev.Details.Version)
	}
	if strings.Contains(ev.Text, "s3cret") || strings.Contains(*ev.Details.Note, "s3cret") {
		t.Fatal("credential leaked while details hidden")
	}
}

func TestDetailsExposedShowsFilteredText(t *testing.T) {
	p := mustProj(t, config.ModePartial, []string{"s3cret"}, true)
	auth := parser.Record{
		Kind:        event.KindAuthAttempt,
		ParseStatus: event.ParsePartial,
		Body:        "198.51.100.24:51234 SSH-2.0-x root s3cret",
	}
	ev := p.Project(auth, now)
	if ev.Text == "" {
		t.Fatal("details exposed: text must not be empty")
	}
	if strings.Contains(ev.Text, "s3cret") {
		t.Fatalf("keyword filter must still apply with details exposed: %q", ev.Text)
	}
	if !strings.Contains(ev.Text, filteredMarker) {
		t.Fatalf("expected marker in %q", ev.Text)
	}
}

func TestConnectionRecordHasNoText(t *testing.T) {
	p := mustProj(t, config.ModePartial, nil, true)
	rec := addrRec("198.51.100.24", 51234, true)
	ev := p.Project(rec, now)
	if ev.Text != "" {
		t.Fatalf("connection record text must be empty (structured fields carry it): %q", ev.Text)
	}
}

func TestLongTextTruncated(t *testing.T) {
	p := mustProj(t, config.ModePartial, nil, true)
	rec := parser.Record{
		Kind:        event.KindMessage,
		ParseStatus: event.ParseUnrecognized,
		Body:        strings.Repeat("x", maxVisibleTextRunes+50),
	}
	ev := p.Project(rec, now)
	if n := len([]rune(ev.Text)); n != maxVisibleTextRunes {
		t.Fatalf("visible runes = %d, want %d", n, maxVisibleTextRunes)
	}
	if ev.Details == nil || ev.Details.Note == nil || *ev.Details.Note != noteTruncated {
		t.Fatal("truncation must be explained")
	}
}

func TestAddressInTextVariants(t *testing.T) {
	p := mustProj(t, config.ModeFull, nil, true)
	in := "src=[2001:db8::1]:22 and 198.51.100.24:80 and bare 203.0.113.9 plus ::ffff:198.51.100.24"
	got := p.projectString(in)
	for _, leak := range []string{"2001:db8::1", "198.51.100.24", "203.0.113.9"} {
		if strings.Contains(got, leak) {
			t.Fatalf("address %q leaked in %q", leak, got)
		}
	}
	if !strings.Contains(got, ":22") || !strings.Contains(got, ":80") {
		t.Fatalf("ports must be preserved alongside placeholders: %q", got)
	}
}

func TestPlainWordsNotMangled(t *testing.T) {
	p := mustProj(t, config.ModeFull, nil, true)
	in := "version 1.2.3 released, ratio 3:1, time 12:30"
	got := p.projectString(in)
	if got != in {
		t.Fatalf("non-address text must be untouched:\n got %q\nwant %q", got, in)
	}
}

func TestInvalidUTF8Replaced(t *testing.T) {
	p := mustProj(t, config.ModePartial, nil, true)
	got := p.projectString("ok\xff\xfe end")
	if strings.ContainsRune(got, 0xff) {
		t.Fatalf("invalid UTF-8 must be normalised: %q", got)
	}
}
