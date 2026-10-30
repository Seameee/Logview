// 有界事件窗口。
//
// 浏览器端必须自己限流：服务端只保证单条上限与快照 100 条，跟随过程中
// 新事件会持续到来。这里同时按条数（500）与字节（2 MiB）双配额淘汰最旧的
// 事件，避免长时间打开页面后内存无限增长。

import type { LogEvent } from './wire'

/** 浏览器端事件条数上限。 */
export const MAX_EVENTS = 500
/** 浏览器端事件内容字节上限。 */
export const MAX_BYTES = 2 * 1024 * 1024
/** 估算每条事件的固定开销（对象、ID、时间戳等）。 */
const EVENT_OVERHEAD = 192

function eventSize(e: LogEvent): number {
  let n = EVENT_OVERHEAD + e.text.length + e.id.length
  if (e.source !== null) n += e.source.addr.length + 8
  if (e.details !== null) {
    n += (e.details.version?.length ?? 0) + (e.details.note?.length ?? 0)
  }
  return n
}

/**
 * EventWindow 保存按追加顺序排列的可见事件。
 *
 * 去重只依据服务端分配的 ID，不依赖时间戳：同一时间戳的重复投递必须被识别，
 * 而乱序或相等时间戳的正常事件不能被丢弃。
 */
export class EventWindow {
  private items: LogEvent[] = []
  private ids = new Set<string>()
  private bytes = 0
  private dropped = 0

  /** 当前可见事件，按追加顺序。 */
  get events(): readonly LogEvent[] {
    return this.items
  }

  /** 因容量限制而淘汰的事件数。 */
  get evicted(): number {
    return this.dropped
  }

  /** 当前占用的估算字节数。 */
  get size(): number {
    return this.bytes
  }

  /** replace 用快照内容替换整个窗口（重置与首连）。 */
  replace(events: readonly LogEvent[]): void {
    this.items = []
    this.ids.clear()
    this.bytes = 0
    for (const e of events) this.append(e)
  }

  /** append 追加一条事件；ID 已存在时忽略并返回 false。 */
  append(e: LogEvent): boolean {
    if (this.ids.has(e.id)) return false
    this.ids.add(e.id)
    this.items.push(e)
    this.bytes += eventSize(e)
    this.trim()
    return true
  }

  clear(): void {
    this.items = []
    this.ids.clear()
    this.bytes = 0
  }

  private trim(): void {
    while (
      this.items.length > MAX_EVENTS ||
      (this.bytes > MAX_BYTES && this.items.length > 1)
    ) {
      const oldest = this.items.shift()
      if (oldest === undefined) break
      this.ids.delete(oldest.id)
      this.bytes -= eventSize(oldest)
      this.dropped += 1
    }
  }
}
