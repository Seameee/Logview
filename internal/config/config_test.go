package config

import (
	"strings"
	"testing"
	"time"
)

// env 构造一个最小 getenv，便于逐项验证默认值与校验。
func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestDefaults(t *testing.T) {
	cfg, warns, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogFile != DefaultLogFile {
		t.Fatalf("LogFile = %q, want %q", cfg.LogFile, DefaultLogFile)
	}
	if cfg.ListenAddr != ":7681" {
		t.Fatalf("ListenAddr = %q, want :7681", cfg.ListenAddr)
	}
	if cfg.AnonymizeMode != ModePartial {
		t.Fatalf("Mode = %q, want partial", cfg.AnonymizeMode)
	}
	if len(cfg.FilterWords) != 0 {
		t.Fatalf("FilterWords = %v, want empty (README 与实现的默认值必须一致)", cfg.FilterWords)
	}
	if cfg.ExposeLogDetails {
		t.Fatal("ExposeLogDetails must default to false")
	}
	if cfg.LogFormat != FormatAuto {
		t.Fatalf("Format = %q, want auto", cfg.LogFormat)
	}
	if cfg.TimeZone != time.UTC || cfg.TimeZoneName != "UTC" || cfg.TimeZoneExplicit {
		t.Fatalf("TimeZone = %v/%q/%v, want UTC/UTC/false", cfg.TimeZone, cfg.TimeZoneName, cfg.TimeZoneExplicit)
	}
	if cfg.MaxSSEClients != DefaultMaxClients {
		t.Fatalf("MaxSSEClients = %d, want %d", cfg.MaxSSEClients, DefaultMaxClients)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
}

func TestEmptyLogFileFallsBackToDefault(t *testing.T) {
	cfg, _, err := Load(env(map[string]string{"LOG_FILE": "   "}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogFile != DefaultLogFile {
		t.Fatalf("LogFile = %q, want default", cfg.LogFile)
	}
}

func TestRelativeLogFileRejected(t *testing.T) {
	if _, _, err := Load(env(map[string]string{"LOG_FILE": "logs/0.log"})); err == nil {
		t.Fatal("relative LOG_FILE must be rejected")
	}
}

func TestAnonymizeModes(t *testing.T) {
	for _, want := range []Mode{ModePartial, ModeFull, ModeHash} {
		cfg, _, err := Load(env(map[string]string{"ANONYMIZE_MODE": string(want)}))
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if cfg.AnonymizeMode != want {
			t.Fatalf("got %q, want %q", cfg.AnonymizeMode, want)
		}
	}
}

func TestInvalidAnonymizeModeFailsStartup(t *testing.T) {
	// 旧脚本遇到未知值会悄悄走 full；新实现必须拒绝启动。
	for _, bad := range []string{"none", "PARTIALX", "xxx"} {
		if _, _, err := Load(env(map[string]string{"ANONYMIZE_MODE": bad})); err == nil {
			t.Fatalf("mode %q must be rejected", bad)
		}
	}
}

func TestFormatValues(t *testing.T) {
	for _, want := range []Format{FormatAuto, FormatPlain, FormatFakessh} {
		cfg, _, err := Load(env(map[string]string{"LOG_FORMAT": string(want)}))
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if cfg.LogFormat != want {
			t.Fatalf("got %q, want %q", cfg.LogFormat, want)
		}
	}
	if _, _, err := Load(env(map[string]string{"LOG_FORMAT": "syslog"})); err == nil {
		t.Fatal("unknown LOG_FORMAT must be rejected")
	}
}

func TestFilterWordsSplitOnWhitespace(t *testing.T) {
	cfg, _, err := Load(env(map[string]string{"FILTER_WORDS": "  LeasewebSG\tfoo\nbar  "}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"LeasewebSG", "foo", "bar"}
	if len(cfg.FilterWords) != len(want) {
		t.Fatalf("FilterWords = %v, want %v", cfg.FilterWords, want)
	}
	for i := range want {
		if cfg.FilterWords[i] != want[i] {
			t.Fatalf("FilterWords = %v, want %v", cfg.FilterWords, want)
		}
	}
}

func TestExposeLogDetailsBooleanForms(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		cfg, _, err := Load(env(map[string]string{"EXPOSE_LOG_DETAILS": v}))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if !cfg.ExposeLogDetails {
			t.Fatalf("%q must enable details", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off"} {
		cfg, _, err := Load(env(map[string]string{"EXPOSE_LOG_DETAILS": v}))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if cfg.ExposeLogDetails {
			t.Fatalf("%q must disable details", v)
		}
	}
	if _, _, err := Load(env(map[string]string{"EXPOSE_LOG_DETAILS": "maybe"})); err == nil {
		t.Fatal("invalid boolean must be rejected rather than guessed")
	}
}

func TestTimeZone(t *testing.T) {
	cfg, _, err := Load(env(map[string]string{"LOG_TIMEZONE": "Asia/Shanghai"}))
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	if cfg.TimeZoneName != "Asia/Shanghai" || !cfg.TimeZoneExplicit {
		t.Fatalf("got %q explicit=%v", cfg.TimeZoneName, cfg.TimeZoneExplicit)
	}
	if _, _, err := Load(env(map[string]string{"LOG_TIMEZONE": "Mars/Olympus"})); err == nil {
		t.Fatal("invalid timezone must fail startup")
	}
}

func TestLWSLogLevelWarnsOnceAndIsIgnored(t *testing.T) {
	cfg, warns, err := Load(env(map[string]string{"LWS_LOG_LEVEL": "7"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one deprecation notice", warns)
	}
	if !strings.Contains(warns[0], "LWS_LOG_LEVEL") || !strings.Contains(warns[0], "已弃用") {
		t.Fatalf("warning text = %q", warns[0])
	}
	// 不接受非法值导致启动失败：旧部署可能留着任何数字。
	if _, _, err := Load(env(map[string]string{"LWS_LOG_LEVEL": "not-a-level"})); err != nil {
		t.Fatalf("deprecated variable must never block startup: %v", err)
	}
	// 该变量不得影响任何有效配置。
	if cfg.MaxSSEClients != DefaultMaxClients {
		t.Fatal("LWS_LOG_LEVEL must not map onto other settings")
	}
}

func TestMaxSSEClientsBounds(t *testing.T) {
	cfg, _, err := Load(env(map[string]string{"MAX_SSE_CLIENTS": "8"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxSSEClients != 8 {
		t.Fatalf("got %d, want 8", cfg.MaxSSEClients)
	}
	for _, bad := range []string{"0", "-1", "100000", "abc"} {
		if _, _, err := Load(env(map[string]string{"MAX_SSE_CLIENTS": bad})); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
