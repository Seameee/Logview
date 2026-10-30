// 展示格式化与筛选。
//
// 所有输出都是纯文本：时间格式、地址拼接、明细说明都不含 HTML 片段，
// 也不产生可点击的 URL（日志内容不可信，不能变成链接）。

import type { LogEvent } from './wire'

const pad2 = (n: number): string => (n < 10 ? `0${n}` : `${n}`)

/**
 * formatTime 把 RFC3339 时间显示为 `MM-DD HH:MM:SS.mmm`。
 *
 * 返回本地时区的时间；无法解析时返回原始字符串，绝不吞掉信息。
 */
export function formatTime(value: string | null): string {
  if (value === null || value === '') return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const ms = `${d.getMilliseconds()}`.padStart(3, '0')
  return `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(
    d.getMinutes(),
  )}:${pad2(d.getSeconds())}.${ms}`
}

/** formatClock 只显示 `HH:MM:SS`，用于窄屏。 */
export function formatClock(value: string | null): string {
  if (value === null || value === '') return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`
}

/** sourceLabel 组合地址与端口；没有地址时给出中文占位。 */
export function sourceLabel(event: LogEvent): string {
  if (event.source === null) return '未知来源'
  const port = event.source.port
  return port === null ? event.source.addr : `${event.source.addr}:${port}`
}

/** kindLabel 是事件类型的中文标签。 */
export function kindLabel(kind: LogEvent['kind']): string {
  switch (kind) {
    case 'connection':
      return '连接'
    case 'auth_attempt':
      return '认证尝试'
    case 'message':
      return '文本'
    case 'unknown':
      return '未知'
  }
}

/** kindClass 是类型标签的 daisyUI badge 颜色。 */
export function kindClass(kind: LogEvent['kind']): string {
  switch (kind) {
    case 'connection':
      return 'badge-info'
    case 'auth_attempt':
      return 'badge-warning'
    case 'message':
      return 'badge-ghost'
    case 'unknown':
      return 'badge-neutral'
  }
}

/** parseLabel 说明解析完整度。 */
export function parseLabel(event: LogEvent): string {
  switch (event.parseStatus) {
    case 'complete':
      return '完整'
    case 'partial':
      return '部分'
    case 'unrecognized':
      return '未识别'
  }
}

/**
 * eventBody 返回一行日志的可读正文。
 *
 * 详情关闭时服务端会清空 text 并给出 note，此时正文位置显示说明文字，
 * 让用户明白"为什么看不到"而不是以为日志为空。
 */
export function eventBody(event: LogEvent): string {
  if (event.text !== '') return event.text
  if (event.details?.note != null && event.details.note !== '') {
    return event.details.note
  }
  if (event.kind === 'connection') return '连接记录（无附加文本）'
  return '（无可见文本）'
}

/**
 * matchesQuery 做字面、区分大小写的子串筛选。
 *
 * 与服务端 FILTER_WORDS 的语义保持一致：不解释正则、不做大小写折叠，
 * 也不支持空格分隔的多关键词；用户输入的整体就是一个字面子串。
 */
export function matchesQuery(event: LogEvent, query: string): boolean {
  if (query === '') return true
  if (event.text.includes(query)) return true
  if (event.source !== null && event.source.addr.includes(query)) return true
  if (event.details?.version != null && event.details.version.includes(query)) {
    return true
  }
  return false
}
