// 线上数据的运行时校验。
//
// 服务端返回的 JSON 一律从 `unknown` 起步，逐字段校验后才转成前端类型。
// 这里不使用类型断言跳过校验，也不 catch 后继续使用半成品对象。

/** 字段不合法时抛出的错误，消息里只带字段路径，不带原始内容。 */
export class WireError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'WireError'
  }
}

export type EventKind = 'connection' | 'auth_attempt' | 'message' | 'unknown'
export type ParseStatus = 'complete' | 'partial' | 'unrecognized'
export type SourceState = 'connecting' | 'live' | 'reconnecting' | 'unreadable'

export interface EventSourceView {
  readonly addr: string
  readonly port: number | null
}

export interface EventDetails {
  readonly version: string | null
  readonly note: string | null
}

export interface LogEvent {
  readonly type: 'log'
  readonly id: string
  readonly timestamp: string | null
  readonly observedAt: string
  readonly kind: EventKind
  readonly source: EventSourceView | null
  readonly parseStatus: ParseStatus
  readonly text: string
  readonly details: EventDetails | null
}

export interface SourceStatus {
  readonly state: SourceState
  readonly message: string | null
  readonly since: string | null
}

export interface PrivacyInfo {
  readonly anonymizeMode: string
  readonly detailsExposed: boolean
  readonly timeZone: string
  readonly timeZoneExplicit: boolean
  readonly filterWordCount: number
}

export interface Snapshot {
  readonly type: 'snapshot'
  readonly events: readonly LogEvent[]
  readonly watermark: string
  readonly source: SourceStatus
  readonly privacy: PrivacyInfo
  readonly historyIncomplete: boolean
  readonly reason: string | null
}

export interface StatusEvent {
  readonly type: 'status'
  readonly source: SourceStatus
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function obj(value: unknown, path: string): Record<string, unknown> {
  if (!isRecord(value)) throw new WireError(`${path}: 期望对象`)
  return value
}

function str(value: unknown, path: string): string {
  if (typeof value !== 'string') throw new WireError(`${path}: 期望字符串`)
  return value
}

function strOrNull(value: unknown, path: string): string | null {
  if (value === null || value === undefined) return null
  return str(value, path)
}

function bool(value: unknown, path: string): boolean {
  if (typeof value !== 'boolean') throw new WireError(`${path}: 期望布尔值`)
  return value
}

function int(value: unknown, path: string): number {
  if (typeof value !== 'number' || !Number.isInteger(value)) {
    throw new WireError(`${path}: 期望整数`)
  }
  return value
}

function oneOf<T extends string>(
  value: unknown,
  path: string,
  allowed: readonly T[],
): T {
  const s = str(value, path)
  for (const candidate of allowed) {
    if (s === candidate) return candidate
  }
  throw new WireError(`${path}: 取值不在允许范围`)
}

const EVENT_KINDS: readonly EventKind[] = [
  'connection',
  'auth_attempt',
  'message',
  'unknown',
]
const PARSE_STATUSES: readonly ParseStatus[] = [
  'complete',
  'partial',
  'unrecognized',
]
const SOURCE_STATES: readonly SourceState[] = [
  'connecting',
  'live',
  'reconnecting',
  'unreadable',
]

function parseSourceView(value: unknown, path: string): EventSourceView | null {
  if (value === null || value === undefined) return null
  const o = obj(value, path)
  const rawPort = o['port']
  const port = rawPort === null || rawPort === undefined ? null : int(rawPort, `${path}.port`)
  if (port !== null && (port < 0 || port > 65535)) {
    throw new WireError(`${path}.port: 端口超出范围`)
  }
  return { addr: str(o['addr'], `${path}.addr`), port }
}

function parseDetails(value: unknown, path: string): EventDetails | null {
  if (value === null || value === undefined) return null
  const o = obj(value, path)
  return {
    version: strOrNull(o['version'], `${path}.version`),
    note: strOrNull(o['note'], `${path}.note`),
  }
}

/** 校验单条日志事件。type 字段可以是 `log` 或缺失，其余一律按 log 处理。 */
export function parseLogEvent(value: unknown, path = 'event'): LogEvent {
  const o = obj(value, path)
  const type = o['type']
  if (type !== undefined && type !== 'log') {
    throw new WireError(`${path}.type: 期望 log`)
  }
  return {
    type: 'log',
    id: str(o['id'], `${path}.id`),
    timestamp: strOrNull(o['timestamp'], `${path}.timestamp`),
    observedAt: str(o['observedAt'], `${path}.observedAt`),
    kind: oneOf(o['kind'], `${path}.kind`, EVENT_KINDS),
    source: parseSourceView(o['source'], `${path}.source`),
    parseStatus: oneOf(o['parseStatus'], `${path}.parseStatus`, PARSE_STATUSES),
    text: str(o['text'], `${path}.text`),
    details: parseDetails(o['details'], `${path}.details`),
  }
}

function parseSourceStatus(value: unknown, path: string): SourceStatus {
  const o = obj(value, path)
  return {
    state: oneOf(o['state'], `${path}.state`, SOURCE_STATES),
    message: strOrNull(o['message'], `${path}.message`),
    since: strOrNull(o['since'], `${path}.since`),
  }
}

function parsePrivacy(value: unknown, path: string): PrivacyInfo {
  const o = obj(value, path)
  return {
    anonymizeMode: str(o['anonymizeMode'], `${path}.anonymizeMode`),
    detailsExposed: bool(o['detailsExposed'], `${path}.detailsExposed`),
    timeZone: str(o['timeZone'], `${path}.timeZone`),
    timeZoneExplicit: bool(o['timeZoneExplicit'], `${path}.timeZoneExplicit`),
    filterWordCount: int(o['filterWordCount'], `${path}.filterWordCount`),
  }
}

/** 校验快照。事件条数上限由服务端保证，这里只做结构校验。 */
export function parseSnapshot(value: unknown): Snapshot {
  const o = obj(value, 'snapshot')
  if (o['type'] !== 'snapshot') {
    throw new WireError('snapshot.type: 期望 snapshot')
  }
  const rawEvents = o['events']
  if (!Array.isArray(rawEvents)) {
    throw new WireError('snapshot.events: 期望数组')
  }
  const events: LogEvent[] = []
  for (let i = 0; i < rawEvents.length; i += 1) {
    events.push(parseLogEvent(rawEvents[i], `snapshot.events[${i}]`))
  }
  return {
    type: 'snapshot',
    events,
    watermark: str(o['watermark'], 'snapshot.watermark'),
    source: parseSourceStatus(o['source'], 'snapshot.source'),
    privacy: parsePrivacy(o['privacy'], 'snapshot.privacy'),
    historyIncomplete: bool(o['historyIncomplete'], 'snapshot.historyIncomplete'),
    reason: strOrNull(o['reason'], 'snapshot.reason'),
  }
}

/** 校验状态事件。 */
export function parseStatusEvent(value: unknown): StatusEvent {
  const o = obj(value, 'status')
  if (o['type'] !== 'status') {
    throw new WireError('status.type: 期望 status')
  }
  return { type: 'status', source: parseSourceStatus(o['source'], 'status.source') }
}

/** 解析 SSE 的 data 载荷，按 type 分派校验。 */
export type ParsedFrame = Snapshot | LogEvent | StatusEvent

export function parseFrame(data: string): ParsedFrame {
  let raw: unknown
  try {
    raw = JSON.parse(data) as unknown
  } catch {
    throw new WireError('data: 不是合法 JSON')
  }
  const o = obj(raw, 'data')
  switch (o['type']) {
    case 'snapshot':
      return parseSnapshot(raw)
    case 'status':
      return parseStatusEvent(raw)
    case 'log':
      return parseLogEvent(raw)
    default:
      throw new WireError('data.type: 未知帧类型')
  }
}
