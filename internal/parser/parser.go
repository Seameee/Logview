// Package parser 把日志行解析成结构化记录。
//
// 解析是纯函数：不读文件、不访问网络、不依赖全局状态。它的产物仍可能
// 含原文，因此只能在读取/解析阶段短暂存在，交给 privacy 投影后立即丢弃。
package parser

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/Seameee/Logview/internal/config"
	"github.com/Seameee/Logview/internal/event"
)

// Record 是解析结果。RawLine 只用于投影，不得写入事件缓冲或 HTTP 响应。
type Record struct {
	// Timestamp 是解析出的 UTC 时间；nil 表示无法解析。
	Timestamp *time.Time
	// Kind 是事件类型。
	Kind event.Kind
	// Addr 是高置信解析出的远端地址；nil 表示无法识别。
	Addr *netip.Addr
	// Port 是与 Addr 配套的端口；0 表示没有端口。
	Port uint16
	// HasPort 表示原始记录确实带了端口。
	HasPort bool
	// ClientVersion 是已确认结构的 SSH 版本串；空表示无法确认。
	ClientVersion string
	// AmbiguousAuthSuffix 表示记录存在无法可靠拆分的认证后缀。
	AmbiguousAuthSuffix bool
	// ParseStatus 是解析完整程度。
	ParseStatus event.ParseStatus
	// Body 是去掉时间前缀后的原始正文，供投影使用。
	Body string
	// RawLine 是完整原始行，供普通文本模式投影使用。
	RawLine string
}

// Parser 是绑定了解释配置的解析器。
type Parser struct {
	format config.Format
	loc    *time.Location
}

// New 创建解析器。loc 用于解释不带时区的日志时间。
func New(format config.Format, loc *time.Location) *Parser {
	if loc == nil {
		loc = time.UTC
	}
	return &Parser{format: format, loc: loc}
}

// fakessh 的时间形如 YYYY/MM/DD HH:MM:SS.ffffff，是生产者本地时间。
const (
	tsLayoutMicro = "2006/01/02 15:04:05.000000"
	tsLayoutSec   = "2006/01/02 15:04:05"
	tsLenMicro    = len("2006/01/02 15:04:05.000000")
	tsLenSec      = len("2006/01/02 15:04:05")
)

// Parse 解析一行。line 不含行尾换行符。
func (p *Parser) Parse(line string) Record {
	rec := Record{RawLine: line, Body: line, ParseStatus: event.ParseUnrecognized, Kind: event.KindUnknown}

	if p.format == config.FormatPlain {
		// 纯文本模式不剥离任何东西：用户明确要求原样看到日志，连 rsyslog
		// 外壳也算他们要看的内容，不替他们判断。
		rec.Kind = event.KindMessage
		return rec
	}

	// auto 与 fakessh 都先剥掉可选的 rsyslog 外壳。外壳是传输层元数据而不是
	// 日志内容：剥离后"是否经过 rsyslog"不再影响可见文本，即使内层无法解析
	// 也保持同一份正文。外壳时间自带偏移，比内层无时区时间可靠，因此凡是
	// 有外壳的行都以它为时间基准。
	var outer *time.Time
	if ts, body, ok := stripSyslogPrefix(line); ok {
		outer = ts
		rec.Timestamp = cloneTime(ts)
		rec.Body = body
	}

	if parsed, ok := p.parseFakessh(line, outer); ok {
		return parsed
	}
	// 显式 fakessh 模式也必须容忍坏行；auto 则退化成普通文本。
	if p.format == config.FormatAuto {
		rec.Kind = event.KindMessage
	}
	return rec
}

// parseFakessh 只在结构完整确认时才返回 ok=true。
//
// outer 非 nil 表示这一行带 rsyslog 外壳，此时外壳时间优先：内层时间没有
// 时区，只能按 LOG_TIMEZONE 猜测，而外壳偏移是生产者/收集器自己写下的。
func (p *Parser) parseFakessh(line string, outer *time.Time) (Record, bool) {
	rec := Record{RawLine: line, ParseStatus: event.ParseUnrecognized, Kind: event.KindUnknown}

	inner := line
	if outer != nil {
		var ok bool
		if _, inner, ok = stripSyslogPrefix(line); !ok {
			return rec, false
		}
	}

	ts, rest, ok := p.splitTimestamp(inner)
	if !ok {
		return rec, false
	}
	if outer != nil {
		ts = cloneTime(outer)
	}
	rec.Timestamp = ts
	rec.Body = rest

	// 远端地址是 "ip:port"（IPv6 需方括号），不含空格，因此可以高置信地
	// 切出第一个 token。裸 IPv6 没有端口时也接受，但绝不补造端口。
	addrTok, after := cutFirstSpace(rest)
	addr, port, hasPort, ok := parseAddrToken(addrTok)
	if !ok {
		return rec, false
	}
	rec.Addr = &addr
	rec.Port = port
	rec.HasPort = hasPort

	if after == "" {
		// 只有地址（可带端口）的记录不含凭据，是完整可公开的结构。
		rec.Kind = event.KindConnection
		if hasPort {
			rec.ParseStatus = event.ParseComplete
		} else {
			// 缺端口属"部分解析"，不补造端口。
			rec.ParseStatus = event.ParsePartial
		}
		return rec, true
	}

	// 后缀是 Go 的 log.Println 用单空格拼接的 "版本 用户名 密码"。
	// 三者都可能含空格，因此无法可靠还原字段边界。
	rec.Kind = event.KindAuthAttempt
	rec.ParseStatus = event.ParsePartial
	rec.AmbiguousAuthSuffix = true

	// 只有 SSH 协议前缀 "SSH-<proto>-<software>" 是结构确定的：
	// 它在第一个空格前结束（RFC 4253）。这里只提取这一段。
	rec.ClientVersion = sshBanner(after)
	return rec, true
}

// parseAddrToken 解析 "ip:port"、"[v6]:port" 或裸地址。
//
// 端口必须严格合法（1..65535）；非法端口时整个 token 不予采信，缺失端口
// 也绝不补造成 0 或默认值。
func parseAddrToken(tok string) (netip.Addr, uint16, bool, bool) {
	if tok == "" {
		return netip.Addr{}, 0, false, false
	}
	if ap, err := netip.ParseAddrPort(tok); err == nil {
		if ap.Port() == 0 {
			return netip.Addr{}, 0, false, false
		}
		return ap.Addr(), ap.Port(), true, true
	}
	addr, err := netip.ParseAddr(tok)
	if err != nil {
		return netip.Addr{}, 0, false, false
	}
	return addr, 0, false, true
}

// stripSyslogPrefix 剥掉 rsyslog 写入的外壳，返回外壳时间与内层消息。
//
// 真实部署里 fakessh 的 stdout 由 rsyslog 收集，落盘形式是
//
//	2026-05-04T20:00:01.123456+08:00 loghost fakessh[4242]: 2026/05/04 12:00:01.123456 203.0.113.5:45802 ...
//
// 外壳时间带明确偏移，是唯一无歧义的时间来源；内层时间没有时区，只能按
// LOG_TIMEZONE 猜测。这里要求外壳结构成立才剥离，否则原样返回，
// 让内层解析照常进行，不因为"看起来像"就丢弃行首内容。
func stripSyslogPrefix(line string) (*time.Time, string, bool) {
	tsTok, afterTS := cutFirstSpace(line)
	if afterTS == "" || !hasOffset(tsTok) {
		return nil, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, tsTok)
	if err != nil {
		return nil, "", false
	}

	// 主机名与 "tag[pid]:"。
	if _, afterHost := cutFirstSpace(afterTS); afterHost != "" {
		tag, rest := cutFirstSpace(afterHost)
		if strings.HasSuffix(tag, ":") && isSyslogTag(tag) && rest != "" {
			utc := t.UTC()
			return &utc, rest, true
		}
	}
	return nil, "", false
}

// hasOffset 判断时间串是否自带时区偏移，避免把无时区的本地时间当权威。
func hasOffset(ts string) bool {
	if len(ts) < 6 {
		return false
	}
	tail := ts[len(ts)-6:]
	if tail[0] != '+' && tail[0] != '-' {
		return false
	}
	if tail[3] != ':' {
		return false
	}
	for _, i := range []int{1, 2, 4, 5} {
		if tail[i] < '0' || tail[i] > '9' {
			return false
		}
	}
	return true
}

// isSyslogTag 校验 "name:" 或 "name[pid]:" 的可打印 ASCII 结构。
func isSyslogTag(tag string) bool {
	if len(tag) < 2 {
		return false
	}
	body := tag[:len(tag)-1] // 去掉结尾冒号
	if body == "" {
		return false
	}
	if i := strings.IndexByte(body, '['); i >= 0 {
		// pid 必须至少有一位数字且全为数字；空 pid（"name[]:"）不是合法外壳。
		if !strings.HasSuffix(body, "]") || i == 0 || i >= len(body)-2 {
			return false
		}
		for _, c := range body[i+1 : len(body)-1] {
			if c < '0' || c > '9' {
				return false
			}
		}
		body = body[:i]
	}
	for i := 0; i < len(body); i++ {
		if body[i] < 0x21 || body[i] > 0x7e || body[i] == '[' || body[i] == ']' {
			return false
		}
	}
	return true
}

// splitTimestamp 切出 fakessh 的时间前缀。
func (p *Parser) splitTimestamp(line string) (*time.Time, string, bool) {
	for _, c := range []struct {
		layout string
		n      int
	}{{tsLayoutMicro, tsLenMicro}, {tsLayoutSec, tsLenSec}} {
		if len(line) < c.n+1 {
			continue
		}
		if line[c.n] != ' ' {
			continue
		}
		t, err := time.ParseInLocation(c.layout, line[:c.n], p.loc)
		if err != nil {
			continue
		}
		utc := t.UTC()
		return &utc, line[c.n+1:], true
	}
	return nil, "", false
}

// cutFirstSpace 按第一个空格切分。
func cutFirstSpace(s string) (head, tail string) {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// sshBanner 提取结构确定的 SSH 版本前缀。
//
// 依据 RFC 4253，ident 形如 "SSH-protoversion-softwareversion SP comments"。
// softwareversion 不含空格，所以第一个空格之前的部分是确定的版本标识；
// 之后的部分（含可能的注释、用户名、密码）一律视为不可靠，不返回。
func sshBanner(s string) string {
	if s == "" {
		return ""
	}
	// Println(addr, "", user, pass) 在版本为空时会产生连续两个空格。
	if s[0] == ' ' {
		return ""
	}
	banner, _ := cutFirstSpace(s)
	if !isSSHBanner(banner) {
		return ""
	}
	return banner
}

// isSSHBanner 校验 "SSH-<major>.<minor>-<software>" 结构且只含可打印 ASCII。
func isSSHBanner(s string) bool {
	if !strings.HasPrefix(s, "SSH-") {
		return false
	}
	rest := s[len("SSH-"):]
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 {
		return false
	}
	proto := rest[:dash]
	software := rest[dash+1:]
	if software == "" {
		return false
	}
	dot := strings.IndexByte(proto, '.')
	if dot <= 0 || dot == len(proto)-1 {
		return false
	}
	for _, part := range []string{proto[:dot], proto[dot+1:]} {
		if part == "" {
			return false
		}
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// String 便于诊断输出，不含原文。
func (r Record) String() string {
	ts := "null"
	if r.Timestamp != nil {
		ts = r.Timestamp.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("kind=%s status=%s ts=%s", r.Kind, r.ParseStatus, ts)
}

// cloneTime 复制时间值，避免多个记录共享同一个指针。
func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
