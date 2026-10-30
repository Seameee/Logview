// Package privacy 把解析结果投影成"可以公开发出去"的事件。
//
// 进入事件缓冲、HTTP、SSE、复制内容和 DOM 的文本都必须经过同一个投影。
// 原文只在读取/解析时短暂存在，投影完成后不再保留可绕过策略的 raw 字段。
//
// 固定顺序：解析原记录 → 选允许公开字段 → 关键词替换 → IP 处理 →
// 控制字符可见转义。
package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
	"github.com/Seameee/Logview/internal/parser"
)

const (
	// filteredMarker 是关键词替换后的固定占位符。用它而不是删除，
	// 避免删掉后前后文拼接出新含义（这与旧 sed 删除行为不同）。
	filteredMarker = "[filtered]"
	// v4FullPlaceholder 沿用旧实现的占位值，便于阅读时一眼认出是 IPv4。
	v4FullPlaceholder = "xxx.xxx.xxx.xxx"
	// v6FullPlaceholder 是统一的 IPv6 占位符，明确表示这里原本是 IPv6。
	v6FullPlaceholder = "xxxx:xxxx:xxxx:xxxx:xxxx:xxxx:xxxx:xxxx"
	// hashDisplayBytes 是 HMAC 展示的十六进制字符数（至少 16）。
	hashDisplayBytes = 16
	// maxVisibleTextRunes 是单条事件可见文本的上限。
	maxVisibleTextRunes = 4096
)

// 说明文案。它们会出现在可见投影里，因此不得包含原始日志内容。
const (
	noteHiddenLegacyFormat = "认证行详情已隐藏：无法可靠区分客户端版本、用户名与密码"
	noteHiddenUnrecognized = "未识别的日志，详情已隐藏"
	noteHiddenPlain        = "文本详情已隐藏：需要管理员显式开启详情公开"
	noteTruncated          = "详情已截断：超出单条长度上限"
	noteIPHidden           = "部分内容无法可靠脱敏，已隐藏"
)

// 候选地址扫描。先粗筛出可能的地址片段，再用 netip 精确校验；
// 校验失败的原样保留，因此不会误改普通文字。
//   - `[::1]:22` 带方括号的 IPv6 加端口
//   - `1.2.3.4:22`、`::ffff:1.2.3.4`、`2001:db8::1` 等裸地址
var ipCandidateRE = regexp.MustCompile(`\[[0-9A-Fa-f:.]{2,}\](?::[0-9]{1,5})?|[0-9A-Fa-f:.]{4,}`)

// Projector 持有本进程的隐私策略。
type Projector struct {
	mode          config.Mode
	words         []string // 已按长度降序排列，保证"重叠词优先较长词"
	exposeDetails bool
	hashKey       []byte
}

// New 创建投影器。hash 模式的密钥在此生成：每次启动随机，进程内稳定，
// 重启即变化（首版不持久化密钥）。
func New(mode config.Mode, words []string, exposeDetails bool) (*Projector, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	sorted := make([]string, 0, len(words))
	for _, w := range words {
		if w != "" {
			sorted = append(sorted, w)
		}
	}
	// 长词优先：同一起点上先匹配更长的词。
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	return &Projector{mode: mode, words: sorted, exposeDetails: exposeDetails, hashKey: key}, nil
}

// Mode 返回生效的匿名化模式。
func (p *Projector) Mode() config.Mode { return p.mode }

// Project 生成一条只含可见数据的事件。ID 由调用方（stream）分配，
// 因为只有它知道进程内单调序号。
func (p *Projector) Project(rec parser.Record, observedAt time.Time) event.Event {
	ev := event.Event{
		Type:        event.TypeLog,
		Timestamp:   cloneTime(rec.Timestamp),
		ObservedAt:  observedAt.UTC(),
		Kind:        rec.Kind,
		ParseStatus: rec.ParseStatus,
	}

	// 1) 选允许公开字段：先把地址投影好，后面文本里再出现同一地址也要一致。
	var sourceAddr string
	if rec.Addr != nil {
		sourceAddr = p.anonymizeAddr(*rec.Addr)
		src := &event.Source{Addr: sourceAddr}
		if rec.HasPort {
			port := rec.Port
			src.Port = &port
		}
		ev.Source = src
	}

	// 2) 文本投影。默认详情关闭，此时只有结构化字段可以公开。
	details := &event.Details{}
	switch {
	case rec.Kind == event.KindConnection && rec.ParseStatus == event.ParseComplete:
		// 只有地址端口的记录不含凭据，文本留空由结构化字段表达，避免重复。
		ev.Text = ""
	case rec.Kind == event.KindAuthAttempt && !p.exposeDetails:
		// 参考 fakessh 的模糊认证后缀：无法确认敏感边界时整段隐藏，
		// 绝不先下发原文再用 CSS 遮盖。
		ev.Text = ""
		details.Note = strPtr(noteHiddenLegacyFormat)
	case !p.exposeDetails && rec.Kind == event.KindMessage:
		ev.Text = ""
		details.Note = strPtr(noteHiddenPlain)
	case !p.exposeDetails:
		ev.Text = ""
		details.Note = strPtr(noteHiddenUnrecognized)
	default:
		// 管理员显式开启了详情公开。
		text, truncated := p.visibleText(rec.Body)
		ev.Text = text
		if truncated {
			details.Note = strPtr(noteTruncated)
		}
	}

	// 3) 只有在来源结构明确时才展示客户端版本；用户名无法可靠还原，不展示。
	if rec.ClientVersion != "" && (p.exposeDetails || rec.Kind == event.KindConnection) {
		version := p.visibleTextNoTruncate(rec.ClientVersion)
		details.Version = &version
	}

	if details.Version != nil || details.Note != nil {
		ev.Details = details
	}
	return ev
}

// visibleText 按固定顺序投影正文，并限制长度。
func (p *Projector) visibleText(s string) (string, bool) {
	out := p.projectString(s)
	truncated := false
	if utf8.RuneCountInString(out) > maxVisibleTextRunes {
		runes := []rune(out)
		out = string(runes[:maxVisibleTextRunes])
		truncated = true
	}
	return out, truncated
}

func (p *Projector) visibleTextNoTruncate(s string) string { return p.projectString(s) }

// projectString 是关键词 → IP → 控制字符的固定管线。
func (p *Projector) projectString(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = p.replaceKeywords(s)
	s = p.replaceAddresses(s)
	return escapeControl(s)
}

// replaceKeywords 用字面量、区分大小写的子串匹配替换关键词。
//
// 单次扫描原始字符串：同一位置上优先匹配较长的词，已匹配的区间不再参与
// 后续匹配，所以生成的 [filtered] 标记不会被再次处理。
func (p *Projector) replaceKeywords(s string) string {
	if len(p.words) == 0 || s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		matched := ""
		for _, w := range p.words {
			if strings.HasPrefix(s[i:], w) {
				matched = w
				break // words 已按长度降序，第一个命中即最长
			}
		}
		if matched != "" {
			b.WriteString(filteredMarker)
			i += len(matched)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// replaceAddresses 用有效地址解析替换所有识别得到的地址。
func (p *Projector) replaceAddresses(s string) string {
	if !strings.ContainsAny(s, "0123456789") {
		return s
	}
	locs := ipCandidateRE.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		addr, port, portSet, ok := parseCandidate(s[start:end])
		if !ok {
			continue
		}
		b.WriteString(s[prev:start])
		b.WriteString(p.anonymizeAddr(addr))
		if portSet {
			b.WriteByte(':')
			b.WriteString(itoa(int(port)))
		}
		prev = end
	}
	if prev == 0 {
		return s
	}
	b.WriteString(s[prev:])
	return b.String()
}

// parseCandidate 尝试把候选片段解析成地址和可选端口。
func parseCandidate(cand string) (netip.Addr, uint16, bool, bool) {
	// 带方括号的 IPv6，可带端口。
	if strings.HasPrefix(cand, "[") {
		close := strings.IndexByte(cand, ']')
		if close < 0 {
			return netip.Addr{}, 0, false, false
		}
		addr, err := netip.ParseAddr(cand[1:close])
		if err != nil || !addr.Is6() {
			return netip.Addr{}, 0, false, false
		}
		rest := cand[close+1:]
		if rest == "" {
			return addr, 0, false, true
		}
		if !strings.HasPrefix(rest, ":") {
			return netip.Addr{}, 0, false, false
		}
		port, ok := parsePort(rest[1:])
		if !ok {
			return netip.Addr{}, 0, false, false
		}
		return addr, port, true, true
	}

	// 裸地址，可能带尾部端口。
	if addr, err := netip.ParseAddr(cand); err == nil {
		return addr, 0, false, true
	}
	if i := strings.LastIndexByte(cand, ':'); i > 0 {
		// 只在冒号后全是数字时当作端口，避免破坏 IPv6 本身。
		if port, ok := parsePort(cand[i+1:]); ok {
			if addr, err := netip.ParseAddr(cand[:i]); err == nil {
				return addr, port, true, true
			}
		}
	}
	return netip.Addr{}, 0, false, false
}

func parsePort(s string) (uint16, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	if n > 65535 {
		return 0, false
	}
	return uint16(n), true
}

// anonymizeAddr 按生效模式处理单个地址。
//
// IPv4-mapped IPv6（::ffff:1.2.3.4）先归一成 IPv4 再处理，避免同一主机
// 因写法不同得到两种展示值。
func (p *Projector) anonymizeAddr(addr netip.Addr) string {
	addr = addr.Unmap()

	switch p.mode {
	case config.ModeFull:
		if addr.Is4() {
			return v4FullPlaceholder
		}
		return v6FullPlaceholder

	case config.ModeHash:
		mac := hmac.New(sha256.New, p.hashKey)
		mac.Write([]byte(addr.String()))
		sum := mac.Sum(nil)
		return "[H:" + strings.ToUpper(hex.EncodeToString(sum)[:hashDisplayBytes]) + "]"

	default: // partial
		if addr.Is4() {
			// 隐藏 /24 之后的末段。
			b := addr.As4()
			return itoa(int(b[0])) + "." + itoa(int(b[1])) + "." + itoa(int(b[2])) + ".xxx"
		}
		// 保留 /48 前缀，隐藏其余部分。
		pfx, err := addr.Prefix(48)
		if err != nil {
			return v6FullPlaceholder
		}
		return pfx.String()
	}
}

// escapeControl 把控制字符转成可见转义，不交给终端解释，也不转成 HTML。
//
// 规则（明确且固定）：
//   - C0（U+0000..U+001F）与 DEL（U+007F）→ `\xNN` 两位小写十六进制
//   - C1（U+0080..U+009F）→ `\uNNNN`
//   - 双向覆盖字符（U+202A..U+202E、U+2066..U+2069）→ `\uNNNN`，避免视觉欺骗
//
// ANSI/OSC 序列本身由 ESC 与 C1 起头，因此落到上面的规则，只会变成可见文本。
func escapeControl(s string) string {
	needs := false
	for _, r := range s {
		if isEscapeTarget(r) {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	const hexDigits = "0123456789abcdef"
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x`)
			b.WriteByte(hexDigits[byte(r)>>4])
			b.WriteByte(hexDigits[byte(r)&0x0f])
		case r >= 0x80 && r <= 0x9f:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[byte(r)>>4])
			b.WriteByte(hexDigits[byte(r)&0x0f])
		case isBidiOverride(r):
			b.WriteString(`\u`)
			b.WriteString(strings.ToLower(hex4(uint16(r))))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isEscapeTarget(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || isBidiOverride(r)
}

func isBidiOverride(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

func hex4(v uint16) string {
	const digits = "0123456789abcdef"
	return string([]byte{
		digits[(v>>12)&0xf], digits[(v>>8)&0xf], digits[(v>>4)&0xf], digits[v&0xf],
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

func strPtr(s string) *string { return &s }
