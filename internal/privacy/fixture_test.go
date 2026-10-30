package privacy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/parser"
)

// fixturePath 指向仓库里的合成日志样本（不含任何真实生产数据）。
func fixturePath(t *testing.T) string {
	t.Helper()
	return namedFixture(t, "sample-fakessh.log")
}

// namedFixture 取 testdata 下的指定样本文件。
func namedFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "..", "testdata", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("样本文件不可读: %v", err)
	}
	return p
}

func loadFixture(t *testing.T) []string {
	t.Helper()
	return loadFixtureFile(t, fixturePath(t))
}

func loadFixtureFile(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取样本失败: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) < 5 {
		t.Fatalf("样本行数异常: %d", len(lines))
	}
	return lines
}

func projectFixture(t *testing.T, mode config.Mode, expose bool) []event.Event {
	t.Helper()
	return projectFixtureFile(t, mode, expose, fixturePath(t))
}

func projectFixtureFile(t *testing.T, mode config.Mode, expose bool, path string) []event.Event {
	t.Helper()
	p := parser.New(config.FormatFakessh, time.UTC)
	proj, err := New(mode, nil, expose)
	if err != nil {
		t.Fatalf("构造投影器失败: %v", err)
	}
	at := time.Date(2026, 5, 4, 12, 0, 30, 0, time.UTC)
	lines := loadFixtureFile(t, path)
	out := make([]event.Event, 0, len(lines))
	for _, line := range lines {
		out = append(out, proj.Project(p.Parse(line), at))
	}
	return out
}

// credentials 是样本里出现的凭据与用户名片段。
var credentials = []string{
	"s3cr3t-not-real", "P@ss word with spaces", "deploy", "admin", "LeasewebSG-Customer",
}

// rawAddresses 是样本里出现的原始地址。任何模式、任何详情开关下都不得出现。
var rawAddresses = []string{
	"203.0.113.45", "203.0.113.46", "198.51.100.7", "192.0.2.9", "192.0.2.44",
	"2001:0db8:abcd:1234:5678:9abc:def0:1234",
	"2001:db8:abcd:1234:5678:9abc:def0:1234",
}

func visibleBlob(ev event.Event) string {
	blob := ev.Text
	if ev.Details != nil {
		if ev.Details.Version != nil {
			blob += "\n" + *ev.Details.Version
		}
		if ev.Details.Note != nil {
			blob += "\n" + *ev.Details.Note
		}
	}
	if ev.Source != nil {
		blob += "\n" + ev.Source.Addr
	}
	return blob
}

// TestFixtureNeverLeaksRawAddresses 断言地址掩码与详情开关无关：三种模式都不出现原始地址。
func TestFixtureNeverLeaksRawAddresses(t *testing.T) {
	for _, mode := range []config.Mode{config.ModePartial, config.ModeFull, config.ModeHash} {
		for _, expose := range []bool{false, true} {
			for _, ev := range projectFixture(t, mode, expose) {
				blob := visibleBlob(ev)
				for _, addr := range rawAddresses {
					if strings.Contains(blob, addr) {
						t.Errorf("mode=%s expose=%v kind=%s 泄漏了原始地址 %q: %q",
							mode, expose, ev.Kind, addr, blob)
					}
				}
			}
		}
	}
}

// TestFixtureHidesCredentialsByDefault 断言默认（详情关闭）时不出现任何凭据片段。
func TestFixtureHidesCredentialsByDefault(t *testing.T) {
	for _, mode := range []config.Mode{config.ModePartial, config.ModeFull, config.ModeHash} {
		for _, ev := range projectFixture(t, mode, false) {
			blob := visibleBlob(ev)
			for _, secret := range credentials {
				if strings.Contains(blob, secret) {
					t.Errorf("mode=%s kind=%s 默认配置下泄漏了凭据 %q: %q", mode, ev.Kind, secret, blob)
				}
			}
		}
	}
}

// TestExposeDetailsDeliberatelyRevealsCredentials 明确记录一条负面事实：
// EXPOSE_LOG_DETAILS=true 时正文按原样公开，凭据会随之出现在 HTTP/SSE 里。
// 这是"进程级开关而非权限"的直接后果，必须由文档告知而不是被测试掩盖。
func TestExposeDetailsDeliberatelyRevealsCredentials(t *testing.T) {
	events := projectFixture(t, config.ModePartial, true)
	var revealed []string
	for _, ev := range events {
		blob := visibleBlob(ev)
		for _, secret := range credentials {
			if strings.Contains(blob, secret) {
				revealed = append(revealed, secret)
			}
		}
	}
	if len(revealed) == 0 {
		t.Fatal("详情开关应公开正文，样本中应能看到凭据；若行为改变需同步更新 README 的风险说明")
	}
	// 地址仍然必须被掩码：详情开关只影响正文，不影响地址投影。
	for _, ev := range events {
		blob := visibleBlob(ev)
		for _, addr := range rawAddresses {
			if strings.Contains(blob, addr) {
				t.Errorf("详情公开时仍不应泄漏原始地址 %q", addr)
			}
		}
	}
}

// TestFixtureDetailsHiddenByDefault 断言默认配置下不会公开正文与版本。
func TestFixtureDetailsHiddenByDefault(t *testing.T) {
	events := projectFixture(t, config.ModePartial, false)
	for _, ev := range events {
		if ev.Text != "" {
			t.Errorf("默认配置下 kind=%s 的 text 应为空，实际 %q", ev.Kind, ev.Text)
		}
		if ev.Details == nil {
			continue
		}
		if ev.Details.Version != nil {
			t.Errorf("默认配置下 kind=%s 不应暴露版本 %q", ev.Kind, *ev.Details.Version)
		}
	}
}

// TestFixtureExposedDetailsAreEscaped 断言开启详情后，控制字符与标签仍只作为
// 可见文本存在：没有任何原始 ESC，也没有形成标记结构。
func TestFixtureExposedDetailsAreEscaped(t *testing.T) {
	events := projectFixture(t, config.ModePartial, true)
	var sawEscapedControl, sawScriptText bool
	for _, ev := range events {
		blob := ev.Text
		if strings.ContainsRune(blob, 0x1b) {
			t.Errorf("可见文本中仍含原始 ESC: %q", blob)
		}
		if strings.Contains(blob, `\x1b[31m`) {
			sawEscapedControl = true
		}
		// "<script>" 只允许作为普通文本出现，不会变成元素；这里断言它没有被吞掉，
		// 同时断言文本本身不含可执行的属性写法。
		if strings.Contains(blob, "<script>") {
			sawScriptText = true
			if strings.Contains(blob, "onerror=") {
				t.Errorf("出现了事件处理属性写法: %q", blob)
			}
		}
	}
	if !sawEscapedControl {
		t.Error("样本中的 ANSI 序列应被转成可见的 \\x1b 文本")
	}
	if !sawScriptText {
		t.Error("样本中的 <script> 文本应作为普通文本保留")
	}
}

// TestFixtureClassification 断言样本的字段分类稳定：只有高置信行才带结构化来源。
func TestFixtureClassification(t *testing.T) {
	events := projectFixture(t, config.ModePartial, false)
	want := []event.Kind{
		event.KindConnection,  // 只有 ip:port
		event.KindAuthAttempt, // ip:port + 后缀
		event.KindAuthAttempt, // 含空格凭据
		event.KindAuthAttempt, // 方括号 IPv6
		event.KindConnection,  // 裸 IPv6（无端口）
		event.KindAuthAttempt, // 版本 + 含关键词的用户名
		event.KindUnknown,     // 坏时间戳
		event.KindAuthAttempt, // 控制字符与标签
	}
	if len(events) != len(want) {
		t.Fatalf("事件数 %d 与预期 %d 不符", len(events), len(want))
	}
	for i, ev := range events {
		if ev.Kind != want[i] {
			t.Errorf("第 %d 行 kind=%s，预期 %s（text=%q）", i+1, ev.Kind, want[i], ev.Text)
		}
	}
	// 裸 IPv6 行识别出地址但没有端口：不得补造端口。
	if ev := events[4]; ev.Source == nil || ev.Source.Port != nil {
		t.Errorf("裸 IPv6 行不应带端口: %+v", ev.Source)
	}
	// 坏时间戳行：没有时间但仍然是 unknown，不带来源。
	if events[6].Timestamp != nil {
		t.Error("坏时间戳不应被猜成时间")
	}
	if events[6].Source != nil {
		t.Error("无法解析的行不应带来源字段")
	}
}

// TestRsyslogWrapperDoesNotChangeProjection 断言"加了 rsyslog 外壳"与"裸行"在
// 投影后应当等价：外壳只影响时间的来源，不改变分类、地址、掩码或文本。
// 这是把真实部署格式纳入回归的依据。
func TestRsyslogWrapperDoesNotChangeProjection(t *testing.T) {
	bare := projectFixtureFile(t, config.ModePartial, true, fixturePath(t))
	wrapped := projectFixtureFile(t, config.ModePartial, true,
		namedFixture(t, "sample-fakessh-rsyslog.log"))

	if len(bare) != len(wrapped) {
		t.Fatalf("事件数不同: bare=%d wrapped=%d", len(bare), len(wrapped))
	}
	for i := range bare {
		b, w := bare[i], wrapped[i]
		if b.Kind != w.Kind || b.ParseStatus != w.ParseStatus {
			t.Errorf("第 %d 行分类不同: bare=%s/%s wrapped=%s/%s",
				i+1, b.Kind, b.ParseStatus, w.Kind, w.ParseStatus)
		}
		if b.Text != w.Text {
			t.Errorf("第 %d 行文本不同:\n bare=%q\n wrapped=%q", i+1, b.Text, w.Text)
		}
		if fmtSource(b.Source) != fmtSource(w.Source) {
			t.Errorf("第 %d 行来源不同: bare=%s wrapped=%s", i+1, fmtSource(b.Source), fmtSource(w.Source))
		}
		if !sameTime(b.Timestamp, w.Timestamp) {
			// 唯一允许的差异：内层无法解析的行，带外壳时仍能拿到可靠的外壳
			// 时间；裸行没有别的来源，只能留空。这是"多知道一点事实"，
			// 不是猜测，因此只允许出现在 unknown 行上。
			if b.Timestamp != nil || b.Kind != event.KindUnknown {
				t.Errorf("第 %d 行时间不同: bare=%v wrapped=%v", i+1, b.Timestamp, w.Timestamp)
			}
		}
	}
}

func fmtSource(s *event.Source) string {
	if s == nil {
		return "<nil>"
	}
	if s.Port == nil {
		return s.Addr
	}
	return s.Addr + "/" + strconv.Itoa(int(*s.Port))
}

func sameTime(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}
