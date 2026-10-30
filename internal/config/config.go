// Package config 读取并校验进程启动配置。
//
// 所有配置只来自进程启动时的环境变量；HTTP 请求无法改写其中任何一项。
// 空值按默认值处理，非空非法值一律返回错误并让进程退出，不沿用旧脚本
// "未知值悄悄落到 full 模式" 的偶然行为。
package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// 内嵌 IANA 时区数据库，使精简运行镜像不依赖宿主的 zoneinfo。
	_ "time/tzdata"
)

// Mode 是 IP 匿名化模式。
type Mode string

const (
	// ModePartial 保留 IPv4 /24 前缀与 IPv6 /48 前缀。
	ModePartial Mode = "partial"
	// ModeFull 用固定占位符替换地址。
	ModeFull Mode = "full"
	// ModeHash 用带随机密钥的 HMAC-SHA256 做假名化。
	ModeHash Mode = "hash"
)

// Format 是日志格式选择。
type Format string

const (
	// FormatAuto 仅在高置信匹配时使用专用解析，其余走普通文本。
	FormatAuto Format = "auto"
	// FormatPlain 强制普通文本模式。
	FormatPlain Format = "plain"
	// FormatFakessh 强制 fakessh 专用解析，同时容忍坏行。
	FormatFakessh Format = "fakessh"
)

const (
	// DefaultLogFile 是 LOG_FILE 的默认值。
	DefaultLogFile = "/var/log/0.log"
	// DefaultListenAddr 是 LISTEN_ADDR 的默认值（容器内监听 7681）。
	DefaultListenAddr = ":7681"
	// DefaultMaxClients 是 SSE 并发客户端上限的默认值。
	DefaultMaxClients = 64
	// DefaultTimeZoneName 是未设置 LOG_TIMEZONE 时的解释假设。
	DefaultTimeZoneName = "UTC"
	// maxSSEClients 是 SSE 客户端上限的硬性天花板。
	maxSSEClients = 1024
)

// Config 是校验后的进程级配置。
type Config struct {
	// LogFile 是唯一被跟随的日志文件路径。
	LogFile string
	// ListenAddr 是 HTTP 监听地址。
	ListenAddr string
	// AnonymizeMode 是 IP 匿名化模式。
	AnonymizeMode Mode
	// FilterWords 是空白分隔的字面关键词表。
	FilterWords []string
	// ExposeLogDetails 决定详情是否进入可见投影。它是进程级开关，
	// 不存在应用内管理员角色，页面提示不构成访问控制。
	ExposeLogDetails bool
	// LogFormat 是解析器选择。
	LogFormat Format
	// TimeZone 用于解释不带时区的日志时间。
	TimeZone *time.Location
	// TimeZoneName 是 TimeZone 对应的名字，用于在页面上标注解释假设。
	TimeZoneName string
	// TimeZoneExplicit 表示管理员是否显式设置了 LOG_TIMEZONE。
	TimeZoneExplicit bool
	// MaxSSEClients 是并发的 /api/stream 连接上限。
	MaxSSEClients int
}

// Load 从 getenv 读取配置。返回的 warnings 应当以 stderr 提示一次。
func Load(getenv func(string) string) (*Config, []string, error) {
	var warnings []string

	cfg := &Config{}

	logFile := strings.TrimSpace(getenv("LOG_FILE"))
	if logFile == "" {
		logFile = DefaultLogFile
	} else if !filepath.IsAbs(logFile) {
		return nil, warnings, fmt.Errorf("LOG_FILE 需要绝对路径，当前值是 %q", logFile)
	}
	cfg.LogFile = logFile

	listen := strings.TrimSpace(getenv("LISTEN_ADDR"))
	if listen == "" {
		listen = DefaultListenAddr
	}
	cfg.ListenAddr = listen

	mode, err := parseMode(getenv("ANONYMIZE_MODE"))
	if err != nil {
		return nil, warnings, err
	}
	cfg.AnonymizeMode = mode

	// FILTER_WORDS 是空白分隔的字面词表，不支持含空格的单个词。
	cfg.FilterWords = strings.Fields(getenv("FILTER_WORDS"))

	expose, err := parseBool("EXPOSE_LOG_DETAILS", getenv("EXPOSE_LOG_DETAILS"), false)
	if err != nil {
		return nil, warnings, err
	}
	cfg.ExposeLogDetails = expose

	format, err := parseFormat(getenv("LOG_FORMAT"))
	if err != nil {
		return nil, warnings, err
	}
	cfg.LogFormat = format

	tzName := strings.TrimSpace(getenv("LOG_TIMEZONE"))
	if tzName == "" {
		cfg.TimeZone = time.UTC
		cfg.TimeZoneName = DefaultTimeZoneName
		cfg.TimeZoneExplicit = false
	} else {
		loc, err := time.LoadLocation(tzName)
		if err != nil {
			return nil, warnings, fmt.Errorf("LOG_TIMEZONE 取值 %q 无法解析：%w", tzName, err)
		}
		cfg.TimeZone = loc
		cfg.TimeZoneName = loc.String()
		cfg.TimeZoneExplicit = true
	}

	clients, err := parseMaxClients(getenv("MAX_SSE_CLIENTS"))
	if err != nil {
		return nil, warnings, err
	}
	cfg.MaxSSEClients = clients

	// LWS_LOG_LEVEL 是旧 ttyd 的日志级别，新实现没有等价语义。
	// 接收旧变量、提示一次弃用后忽略，不伪造 Go 等级对应关系。
	if v := strings.TrimSpace(getenv("LWS_LOG_LEVEL")); v != "" {
		warnings = append(warnings, fmt.Sprintf(
			"LWS_LOG_LEVEL=%s 已弃用并被忽略：ttyd 已移除，该变量在新实现中没有等价含义", v))
	}

	return cfg, warnings, nil
}

func parseMode(raw string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(raw))) {
	case "":
		return ModePartial, nil
	case ModePartial:
		return ModePartial, nil
	case ModeFull:
		return ModeFull, nil
	case ModeHash:
		return ModeHash, nil
	default:
		return "", fmt.Errorf("ANONYMIZE_MODE 取值 %q 无法识别：需要 partial、full 或 hash", raw)
	}
}

func parseFormat(raw string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(raw))) {
	case "":
		return FormatAuto, nil
	case FormatAuto:
		return FormatAuto, nil
	case FormatPlain:
		return FormatPlain, nil
	case FormatFakessh:
		return FormatFakessh, nil
	default:
		return "", fmt.Errorf("LOG_FORMAT 取值 %q 无法识别：需要 auto、plain 或 fakessh", raw)
	}
}

func parseBool(name, raw string, def bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return def, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return def, fmt.Errorf("%s 取值 %q 无法识别：需要 true 或 false", name, raw)
	}
}

func parseMaxClients(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultMaxClients, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("MAX_SSE_CLIENTS 取值 %q 无法识别：需要正整数", raw)
	}
	if n < 1 || n > maxSSEClients {
		return 0, fmt.Errorf("MAX_SSE_CLIENTS 取值 %d 超出范围 1..%d", n, maxSSEClients)
	}
	return n, nil
}
