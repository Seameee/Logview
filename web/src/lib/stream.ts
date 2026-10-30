// SSE 客户端。
//
// 不使用 EventSource 的原因：需要在重连时带上 `Last-Event-ID`、需要区分
// "连接断开" 与 "服务端要求重置"，并且要能彻底销毁。浏览器原生 EventSource
// 的自动重连会丢失这些控制点。这里用 fetch + ReadableStream 自行解析 SSE。

import { parseFrame, WireError } from './wire'
import type { LogEvent, ParsedFrame, Snapshot, StatusEvent } from './wire'

/** 重连退避：从 1 秒开始翻倍，上限 15 秒，并加入抖动避免同时重连。 */
const RECONNECT_BASE_MS = 1000
const RECONNECT_MAX_MS = 15000

export type StreamPhase = 'connecting' | 'open' | 'closed'

/** 客户端对外回调。所有回调都在解析成功后调用。 */
export interface StreamHandlers {
  onSnapshot: (snap: Snapshot) => void
  onStatus: (status: StatusEvent) => void
  onLog: (event: LogEvent) => void
  onPhase: (phase: StreamPhase) => void
  onError: (message: string) => void
}

/**
 * StreamClient 负责建立并维持一条 SSE 连接。
 *
 * close() 之后不会再有任何回调，也不会再有重连计时器；组件卸载必须调用它。
 */
export class StreamClient {
  private readonly url: string
  private readonly handlers: StreamHandlers
  private controller: AbortController | null = null
  private timer: number | null = null
  private attempt = 0
  private lastId: string | null = null
  private disposed = false
  private running = false

  constructor(url: string, handlers: StreamHandlers) {
    this.url = url
    this.handlers = handlers
  }

  /** start 启动连接；重复调用无副作用。 */
  start(): void {
    if (this.disposed || this.running) return
    this.running = true
    void this.loop()
  }

  /** close 立即停止并释放所有资源；幂等。 */
  close(): void {
    if (this.disposed) return
    this.disposed = true
    this.running = false
    if (this.timer !== null) {
      window.clearTimeout(this.timer)
      this.timer = null
    }
    this.controller?.abort()
    this.controller = null
    this.handlers.onPhase('closed')
  }

  private async loop(): Promise<void> {
    while (!this.disposed) {
      this.handlers.onPhase('connecting')
      const ac = new AbortController()
      this.controller = ac

      let failedMessage: string | null = null
      try {
        await this.connect(ac)
      } catch (err) {
        if (this.disposed) return
        failedMessage = describeError(err)
      } finally {
        if (this.controller === ac) this.controller = null
      }

      if (this.disposed) return

      if (failedMessage !== null) {
        this.handlers.onError(failedMessage)
      }
      const wait = this.nextDelay()
      await this.sleep(wait)
    }
  }

  private nextDelay(): number {
    const factor = 2 ** Math.min(this.attempt, 4)
    const base = Math.min(RECONNECT_BASE_MS * factor, RECONNECT_MAX_MS)
    // 抖动 ±20%：多标签页同时断线时避免一起打回服务端。
    const jitter = base * (Math.random() * 0.4 - 0.2)
    this.attempt += 1
    return Math.round(base + jitter)
  }

  private sleep(ms: number): Promise<void> {
    return new Promise((resolve) => {
      this.timer = window.setTimeout(() => {
        this.timer = null
        resolve()
      }, ms)
    })
  }

  private async connect(ac: AbortController): Promise<void> {
    const headers: Record<string, string> = { Accept: 'text/event-stream' }
    if (this.lastId !== null) {
      headers['Last-Event-ID'] = this.lastId
    }
    const res = await fetch(this.url, {
      headers,
      signal: ac.signal,
      cache: 'no-store',
      credentials: 'same-origin',
      redirect: 'error',
    })

    if (!res.ok) {
      // 503 表示服务端并发上限已满，属于可重试状态。
      throw new StreamError(`服务端返回 HTTP ${res.status}`)
    }
    const body = res.body
    if (body === null) {
      throw new StreamError('响应没有可读正文')
    }

    this.handlers.onPhase('open')
    this.attempt = 0

    const reader = body.getReader()
    const decoder = new TextDecoder('utf-8', { fatal: false })
    let buffer = ''

    try {
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        // SSE 以空行分隔事件；\r\n 与 \n 都要支持。
        buffer = buffer.replaceAll('\r\n', '\n')
        let idx = buffer.indexOf('\n\n')
        while (idx >= 0) {
          const block = buffer.slice(0, idx)
          buffer = buffer.slice(idx + 2)
          this.consumeBlock(block)
          if (this.disposed) return
          idx = buffer.indexOf('\n\n')
        }
        if (buffer.length > 4 * 1024 * 1024) {
          throw new StreamError('单帧数据超过缓冲上限')
        }
      }
    } finally {
      // 主动取消读取：abort 之后 read() 可能仍持有底层连接。
      try {
        await reader.cancel()
      } catch {
        // 关闭阶段的取消错误无需上报。
      }
    }

    if (!this.disposed) {
      throw new StreamError('连接已被服务端关闭')
    }
  }

  /** consumeBlock 解析一个 SSE 事件块。 */
  private consumeBlock(block: string): void {
    let eventName = ''
    let id: string | null = null
    const dataLines: string[] = []

    for (const line of block.split('\n')) {
      if (line === '' || line.startsWith(':')) continue
      const colon = line.indexOf(':')
      const field = colon === -1 ? line : line.slice(0, colon)
      let value = colon === -1 ? '' : line.slice(colon + 1)
      if (value.startsWith(' ')) value = value.slice(1)
      switch (field) {
        case 'event':
          eventName = value
          break
        case 'id':
          id = value
          break
        case 'data':
          dataLines.push(value)
          break
        default:
          // retry/unknown 字段忽略：重连节奏由本模块自己控制。
          break
      }
    }

    if (dataLines.length === 0) return
    const data = dataLines.join('\n')

    let frame: ParsedFrame
    try {
      frame = parseFrame(data)
    } catch (err) {
      const msg = err instanceof WireError ? err.message : '载荷格式无法识别'
      this.handlers.onError(`收到无法识别的数据：${msg}`)
      return
    }

    // 只有在载荷校验通过后才推进游标：否则会带着坏游标继续补发。
    if (id !== null && id !== '') this.lastId = id

    switch (frame.type) {
      case 'snapshot':
        this.handlers.onSnapshot(frame)
        break
      case 'status':
        this.handlers.onStatus(frame)
        break
      case 'log':
        if (eventName !== '' && eventName !== 'log') {
          this.handlers.onError(`事件名与载荷不一致：${eventName}`)
          return
        }
        this.handlers.onLog(frame)
        break
    }
  }
}

class StreamError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'StreamError'
  }
}

function describeError(err: unknown): string {
  if (err instanceof Error) {
    if (err.name === 'AbortError') return '连接已中断'
    return err.message
  }
  return '连接失败'
}
