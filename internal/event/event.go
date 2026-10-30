// Package event 定义服务端与浏览器之间的线上契约。
//
// 这里只允许出现"完成投影后"的可见数据。原始日志行、原始 IP、密码以及
// "前端暂时用不到" 的敏感字段都不得进入本包的类型。
package event

import "time"

// Kind 是事件类型。只依据受支持的 parser 判定，不推测 SSH 会话、
// 命令执行或登录成功。
type Kind string

const (
	// KindConnection 表示只有远端地址端口的连接记录。
	KindConnection Kind = "connection"
	// KindAuthAttempt 表示带客户端版本与凭据后缀的认证尝试记录。
	KindAuthAttempt Kind = "auth_attempt"
	// KindMessage 表示已按普通文本处理的行。
	KindMessage Kind = "message"
	// KindUnknown 表示无法归类的行。
	KindUnknown Kind = "unknown"
)

// ParseStatus 表示解析完整程度。缺失数据不能伪造。
type ParseStatus string

const (
	// ParseComplete 表示该记录的全部字段均已可靠解析。
	ParseComplete ParseStatus = "complete"
	// ParsePartial 表示只可靠解析了一部分字段。
	ParsePartial ParseStatus = "partial"
	// ParseUnrecognized 表示没有可靠的字段解析。
	ParseUnrecognized ParseStatus = "unrecognized"
)

// Type 是 SSE data 的判别字段。
const (
	TypeSnapshot = "snapshot"
	TypeLog      = "log"
	TypeStatus   = "status"
)

// Source 是处理后的地址展示值。识别不出地址时为 nil。
type Source struct {
	// Addr 是已按匿名化模式处理过的地址展示值。
	Addr string `json:"addr"`
	// Port 是端口；没有端口时为 null。
	Port *uint16 `json:"port"`
}

// Details 是可选的详情投影。它永远不含密码。
type Details struct {
	// Version 是已确认结构的客户端版本；不展示时为 null。
	Version *string `json:"version"`
	// Note 说明详情被隐藏或截断的原因；无说明时为 null。
	Note *string `json:"note"`
}

// Event 是一条已完成服务端可见投影的日志事件。
type Event struct {
	// Type 恒为 TypeLog，用于让浏览器区分 snapshot/log/status。
	Type string `json:"type"`
	// ID 是"进程 epoch + 单调序号"，用于去重、排序与重连，不含原 IP。
	ID string `json:"id"`
	// Timestamp 是解析后的 UTC 时间；解析不了为 null。
	Timestamp *time.Time `json:"timestamp"`
	// ObservedAt 是服务端接收该记录的 UTC 时间，与日志自身时间区分。
	ObservedAt time.Time `json:"observedAt"`
	// Kind 是事件类型。
	Kind Kind `json:"kind"`
	// Source 是处理后的地址展示值。
	Source *Source `json:"source"`
	// ParseStatus 是解析完整程度。
	ParseStatus ParseStatus `json:"parseStatus"`
	// Text 是可见文本。详情关闭时可能为空，此时由 Details.Note 说明原因。
	Text string `json:"text"`
	// Details 是可选详情。
	Details *Details `json:"details"`
}

// SourceState 是采集源的状态。
type SourceState string

const (
	// StateConnecting 表示尚未首次读到文件。
	StateConnecting SourceState = "connecting"
	// StateLive 表示正在实时跟随。
	StateLive SourceState = "live"
	// StateReconnecting 表示文件暂时不可用，正在重试。
	StateReconnecting SourceState = "reconnecting"
	// StateUnreadable 表示文件存在但无法读取。
	StateUnreadable SourceState = "unreadable"
)

// SourceStatus 描述采集源状态。它不得包含宿主绝对路径或日志内容。
type SourceStatus struct {
	State SourceState `json:"state"`
	// Message 是给用户看的原因说明，不含宿主路径。
	Message *string `json:"message"`
	// Since 是该状态开始的 UTC 时间。
	Since *time.Time `json:"since"`
}

// PrivacyInfo 是本实例实际生效的隐私策略，供页面显示。
type PrivacyInfo struct {
	AnonymizeMode string `json:"anonymizeMode"`
	// DetailsExposed 为 true 时所有能访问该实例的客户端都会收到允许公开的详情。
	// 页面提示不构成访问控制。
	DetailsExposed bool `json:"detailsExposed"`
	// TimeZone 是不带时区日志时间的解释假设。
	TimeZone string `json:"timeZone"`
	// TimeZoneExplicit 表示该假设是否由管理员显式配置。
	TimeZoneExplicit bool `json:"timeZoneExplicit"`
	// FilterWordCount 只暴露词表规模，不暴露词本身。
	FilterWordCount int `json:"filterWordCount"`
}

// Snapshot 是首连与重置时的完整状态。
type Snapshot struct {
	Type string `json:"type"`
	// Events 是按追加顺序排列的有限历史窗口。
	Events []Event `json:"events"`
	// Watermark 是高水位游标，同时作为 SSE 的 id: 写出。
	Watermark string `json:"watermark"`
	// Source 是采集源状态。
	Source SourceStatus `json:"source"`
	// Privacy 是生效的隐私策略。
	Privacy PrivacyInfo `json:"privacy"`
	// HistoryIncomplete 表示窗口之前的历史已被淘汰或未完整读取。
	HistoryIncomplete bool `json:"historyIncomplete"`
	// Reason 说明本次为何发送快照（首连时为 null）。
	Reason *string `json:"reason"`
}

// StatusEvent 是采集源状态变化事件。
type StatusEvent struct {
	Type   string       `json:"type"`
	Source SourceStatus `json:"source"`
}
