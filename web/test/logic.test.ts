// 前端纯逻辑的运行时校验测试。
//
// 只覆盖不依赖 DOM 的模块（wire/format/window）：运行时 JSON 校验、
// 有界窗口淘汰、筛选与格式化。用 node 直接运行，不引入测试框架。
//
//   node --test web/test/logic.test.ts      （Node 22.18+/24 原生类型剥离）
//   ./scripts/test-web.sh                  （推荐，统一入口）

import assert from 'node:assert/strict'
import { test } from 'node:test'

import { eventBody, formatTime, matchesQuery, sourceLabel } from '../src/lib/format.ts'
import { EventWindow, MAX_EVENTS } from '../src/lib/window.ts'
import {
  parseFrame,
  parseLogEvent,
  parseSnapshot,
  WireError,
} from '../src/lib/wire.ts'
import type { LogEvent } from '../src/lib/wire.ts'

function makeEvent(over: Partial<LogEvent> = {}): LogEvent {
  return {
    type: 'log',
    id: 'e-1',
    timestamp: '2026-05-04T12:00:01.123Z',
    observedAt: '2026-05-04T12:00:01.200Z',
    kind: 'auth_attempt',
    source: { addr: '203.0.113.xxx', port: 51235 },
    parseStatus: 'partial',
    text: 'hello',
    details: { version: null, note: null },
    ...over,
  }
}

test('parseLogEvent 接受服务端实际载荷', () => {
  const raw: unknown = {
    type: 'log',
    id: 'abc-7',
    timestamp: null,
    observedAt: '2026-05-04T12:00:01.2Z',
    kind: 'connection',
    source: { addr: '203.0.113.xxx', port: 22 },
    parseStatus: 'complete',
    text: '',
    details: null,
  }
  const e = parseLogEvent(raw)
  assert.equal(e.id, 'abc-7')
  assert.equal(e.timestamp, null)
  assert.equal(e.source?.addr, '203.0.113.xxx')
  assert.equal(e.details, null)
})

test('parseLogEvent 拒绝非法字段而不是猜测', () => {
  const bad: unknown[] = [
    null,
    [],
    { id: 'x' },
    { id: 1, observedAt: 'x', kind: 'message', parseStatus: 'complete', text: '' },
    {
      id: 'x',
      observedAt: 'x',
      kind: 'login_success',
      parseStatus: 'complete',
      text: '',
    },
    { id: 'x', observedAt: 'x', kind: 'message', parseStatus: 'maybe', text: '' },
    {
      id: 'x',
      observedAt: 'x',
      kind: 'message',
      parseStatus: 'complete',
      text: '',
      source: { addr: 'a', port: 99999 },
    },
  ]
  for (const value of bad) {
    assert.throws(() => parseLogEvent(value), WireError)
  }
})

test('parseSnapshot 校验全部必需字段', () => {
  const snap = parseSnapshot({
    type: 'snapshot',
    events: [],
    watermark: 'e-1',
    source: { state: 'live', message: null, since: null },
    privacy: {
      anonymizeMode: 'partial',
      detailsExposed: false,
      timeZone: 'UTC',
      timeZoneExplicit: false,
      filterWordCount: 0,
    },
    historyIncomplete: false,
    reason: null,
  })
  assert.equal(snap.type, 'snapshot')
  assert.equal(snap.events.length, 0)

  assert.throws(
    () =>
      parseSnapshot({
        type: 'snapshot',
        events: [],
        watermark: 'e-1',
        source: { state: 'zombie', message: null, since: null },
        privacy: {
          anonymizeMode: 'partial',
          detailsExposed: false,
          timeZone: 'UTC',
          timeZoneExplicit: false,
          filterWordCount: 0,
        },
        historyIncomplete: false,
        reason: null,
      }),
    WireError,
  )
})

test('parseFrame 按 type 分派并拒绝未知类型', () => {
  assert.throws(() => parseFrame('null'), WireError)
  assert.throws(() => parseFrame('not json'), WireError)
  assert.throws(() => parseFrame('{"type":"nope"}'), WireError)

  const st = parseFrame(JSON.stringify({ type: 'status', source: { state: 'live' } }))
  assert.equal(st.type, 'status')
})

test('EventWindow 按 ID 去重，不依赖时间戳', () => {
  const w = new EventWindow()
  assert.equal(w.append(makeEvent({ id: 'a' })), true)
  // 同一时间戳不同 ID：两条都要保留。
  assert.equal(w.append(makeEvent({ id: 'b' })), true)
  // 重复 ID：忽略。
  assert.equal(w.append(makeEvent({ id: 'a' })), false)
  assert.equal(w.events.length, 2)
})

test('EventWindow 在超过条数上限时淘汰最旧事件', () => {
  const w = new EventWindow()
  for (let i = 0; i < MAX_EVENTS + 25; i += 1) {
    w.append(makeEvent({ id: `id-${i}` }))
  }
  assert.equal(w.events.length, MAX_EVENTS)
  assert.equal(w.evicted, 25)
  // 最旧的 25 条已被淘汰，最新一条仍在。
  assert.equal(w.events[0]?.id, 'id-25')
  assert.equal(w.events[w.events.length - 1]?.id, `id-${MAX_EVENTS + 24}`)
})

test('EventWindow replace 重置窗口与去重集合', () => {
  const w = new EventWindow()
  w.append(makeEvent({ id: 'a' }))
  w.replace([makeEvent({ id: 'b' })])
  assert.deepEqual(
    w.events.map((e) => e.id),
    ['b'],
  )
  // 替换后 'a' 不再占用去重集合。
  assert.equal(w.append(makeEvent({ id: 'a' })), true)
})

test('formatTime 无法解析时保留原始值', () => {
  assert.equal(formatTime(null), '—')
  assert.equal(formatTime('2026-13-99'), '2026-13-99')
  assert.match(formatTime('2026-05-04T12:00:01.123Z'), /^\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.123$/)
})

test('sourceLabel 与 eventBody 处理缺失字段', () => {
  assert.equal(sourceLabel(makeEvent()), '203.0.113.xxx:51235')
  assert.equal(sourceLabel(makeEvent({ source: null })), '未知来源')
  assert.equal(sourceLabel(makeEvent({ source: { addr: 'x', port: null } })), 'x')
  // 详情被隐藏时显示说明，而不是空白。
  assert.equal(
    eventBody(
      makeEvent({ text: '', details: { version: null, note: '认证行详情已隐藏' } }),
    ),
    '认证行详情已隐藏',
  )
  assert.equal(eventBody(makeEvent({ text: '', details: null, kind: 'connection' })), '连接记录（无附加文本）')
})

test('matchesQuery 是字面且区分大小写的子串匹配', () => {
  const e = makeEvent({ text: 'bad/word.*&' })
  assert.equal(matchesQuery(e, ''), true)
  assert.equal(matchesQuery(e, 'bad/word.*&'), true)
  assert.equal(matchesQuery(e, 'word.*'), true)
  assert.equal(matchesQuery(e, 'BAD/WORD'), false)
  // 正则元字符按字面处理，不解释为模式。
  assert.equal(matchesQuery(e, 'w.r'), false)
  assert.equal(matchesQuery(makeEvent({ text: 'abc' }), '.+'), false)
})
