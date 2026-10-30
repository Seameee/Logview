<script setup lang="ts">
// 日志查看器主界面。
//
// 数据流：StreamClient → EventWindow（有界）→ 计算属性筛选 → 列表渲染。
// 所有渲染都经过文本插值；本组件不持有任何 HTML 字符串。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import LogRow from './components/LogRow.vue'
import { matchesQuery } from './lib/format'
import { StreamClient } from './lib/stream'
import type { StreamPhase } from './lib/stream'
import { EventWindow } from './lib/window'
import type { LogEvent, PrivacyInfo, SourceStatus } from './lib/wire'

const window_ = new EventWindow()

const events = ref<readonly LogEvent[]>([])
const privacy = ref<PrivacyInfo | null>(null)
const source = ref<SourceStatus | null>(null)
const phase = ref<StreamPhase>('connecting')
const historyIncomplete = ref(false)
const resetReason = ref<string | null>(null)
const lastError = ref<string | null>(null)

const query = ref('')
const follow = ref(true)
const selectedId = ref<string | null>(null)
const theme = ref<'dark' | 'light'>('dark')
const scroller = ref<HTMLElement | null>(null)
const newBelow = ref(0)

const filtered = computed(() => {
  const q = query.value
  if (q === '') return events.value
  return events.value.filter((e) => matchesQuery(e, q))
})

const selected = computed<LogEvent | null>(() => {
  const id = selectedId.value
  if (id === null) return null
  return events.value.find((e) => e.id === id) ?? null
})

const isNarrow = ref(false)
const statusLabel = computed(() => {
  const st = source.value
  if (st === null) return '连接中'
  switch (st.state) {
    case 'connecting':
      return '连接中'
    case 'live':
      return '实时'
    case 'reconnecting':
      return '重连中'
    case 'unreadable':
      return '不可读'
  }
})

const statusClass = computed(() => {
  const st = source.value
  if (st === null) return 'badge-ghost'
  switch (st.state) {
    case 'connecting':
      return 'badge-ghost'
    case 'live':
      return 'badge-success'
    case 'reconnecting':
      return 'badge-warning'
    case 'unreadable':
      return 'badge-error'
  }
})

const connectionClass = computed(() => {
  switch (phase.value) {
    case 'open':
      return 'badge-success'
    case 'connecting':
      return 'badge-warning'
    case 'closed':
      return 'badge-error'
  }
})

function sync() {
  events.value = [...window_.events]
}

function scrollToBottom() {
  const el = scroller.value
  if (el === null) return
  el.scrollTop = el.scrollHeight
  newBelow.value = 0
}

const client = new StreamClient('/api/stream', {
  onSnapshot: (snap) => {
    privacy.value = snap.privacy
    source.value = snap.source
    historyIncomplete.value = snap.historyIncomplete
    resetReason.value = snap.reason
    window_.replace(snap.events)
    sync()
    if (follow.value) void nextTick(scrollToBottom)
  },
  onStatus: (st) => {
    source.value = st.source
  },
  onLog: (event) => {
    window_.append(event)
    const st = source.value
    if (st !== null && st.state !== 'live') {
      source.value = { state: 'live', message: null, since: st.since }
    }
    sync()
    if (follow.value) {
      void nextTick(scrollToBottom)
    } else {
      newBelow.value += 1
    }
  },
  onPhase: (p) => {
    phase.value = p
    if (p === 'open') lastError.value = null
  },
  onError: (message) => {
    lastError.value = message
  },
})

// 用户向上滚动即暂停跟随；回到底部自动恢复。
function onScroll() {
  const el = scroller.value
  if (el === null) return
  const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24
  if (atBottom) {
    if (!follow.value) {
      follow.value = true
      newBelow.value = 0
    }
  } else if (follow.value) {
    follow.value = false
  }
}

function toggleFollow() {
  follow.value = !follow.value
  if (follow.value) void nextTick(scrollToBottom)
}

function select(event: LogEvent) {
  selectedId.value = selectedId.value === event.id ? null : event.id
}

function toggleTheme() {
  theme.value = theme.value === 'dark' ? 'light' : 'dark'
  document.documentElement.dataset['theme'] = theme.value
}

// 键盘：Esc 关闭详情，f 切换跟随，/ 聚焦筛选框。
function onKeydown(e: KeyboardEvent) {
  const target = e.target
  const inField =
    target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement
  if (e.key === 'Escape') {
    selectedId.value = null
    if (inField && target instanceof HTMLElement) target.blur()
    return
  }
  if (inField) return
  if (e.key === 'f') {
    e.preventDefault()
    toggleFollow()
  }
  if (e.key === '/') {
    e.preventDefault()
    const el = document.getElementById('log-filter')
    if (el instanceof HTMLInputElement) el.focus()
  }
}

function measure() {
  isNarrow.value = window.innerWidth < 640
}

onMounted(() => {
  document.documentElement.dataset['theme'] = theme.value
  measure()
  window.addEventListener('keydown', onKeydown)
  window.addEventListener('resize', measure)
  client.start()
})

// 组件销毁时断开 SSE 并移除监听器：不留后台连接与计时器。
onBeforeUnmount(() => {
  client.close()
  window.removeEventListener('keydown', onKeydown)
  window.removeEventListener('resize', measure)
})

watch(filtered, () => {
  if (follow.value) void nextTick(scrollToBottom)
})

// 快照被重置时清掉可能已不存在的选中项。
watch(events, (list) => {
  const id = selectedId.value
  if (id !== null && !list.some((e) => e.id === id)) selectedId.value = null
})
</script>

<template>
  <div class="flex h-full flex-col bg-base-100 text-base-content">
    <header class="border-b border-base-300 bg-base-200/60 px-3 py-2">
      <div class="flex flex-wrap items-center gap-2">
        <h1 class="mr-1 font-mono text-sm font-semibold tracking-wide">Logview</h1>

        <span data-testid="status" class="badge badge-sm gap-1" :class="connectionClass">
          {{ phase === 'open' ? '已连接' : phase === 'connecting' ? '连接中' : '已断开' }}
        </span>

        <span class="badge badge-sm gap-1" :class="statusClass" title="日志源状态">
          源：{{ statusLabel }}
        </span>

        <label class="input input-xs ml-auto flex w-40 items-center gap-1 sm:w-64">
          <span class="text-base-content/50" aria-hidden="true">/</span>
          <input
            id="log-filter"
            v-model="query"
            type="search"
            class="grow"
            placeholder="字面筛选（区分大小写）"
            aria-label="按字面筛选日志"
            autocomplete="off"
            spellcheck="false"
          />
        </label>

        <button
          type="button"
          class="btn btn-xs"
          :class="follow ? 'btn-primary' : 'btn-ghost'"
          :aria-pressed="follow"
          title="快捷键：f"
          @click="toggleFollow"
        >
          {{ follow ? '跟随中' : '已暂停' }}
        </button>

        <button
          type="button"
          data-testid="theme-toggle"
          class="btn btn-xs btn-ghost"
          :title="theme === 'dark' ? '切换到浅色' : '切换到深色'"
          @click="toggleTheme"
        >
          {{ theme === 'dark' ? '浅色' : '深色' }}
        </button>
      </div>

      <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-base-content/60">
        <span>{{ filtered.length }} / {{ events.length }} 条</span>
        <span v-if="window_.evicted > 0">已淘汰 {{ window_.evicted }} 条</span>
        <span v-if="privacy !== null">IP 掩码：{{ privacy.anonymizeMode }}</span>
        <span v-if="privacy !== null && privacy.filterWordCount > 0">
          关键词 {{ privacy.filterWordCount }} 个
        </span>
        <span v-if="privacy !== null">
          详情：{{ privacy.detailsExposed ? '已公开' : '已隐藏' }}
        </span>
        <span
          v-if="privacy !== null"
          :title="privacy.timeZoneExplicit ? '由 LOG_TIMEZONE 显式指定' : '未指定 LOG_TIMEZONE，默认按 UTC 解释'"
        >
          无时区时间按 {{ privacy.timeZone }} 解释{{ privacy.timeZoneExplicit ? '' : '（默认）' }}
        </span>
      </div>
    </header>

    <div
      v-if="historyIncomplete || resetReason !== null || lastError !== null || privacy?.detailsExposed === true"
      class="space-y-1 px-3 py-1.5 text-[11px]"
    >
      <p v-if="historyIncomplete" class="text-base-content/70">
        更早的历史未加载：启动时只读日志尾部窗口，更早的内容也可能已被淘汰。
      </p>
      <p v-if="resetReason !== null" class="text-warning">
        {{ resetReason }}
      </p>
      <p v-if="lastError !== null" class="text-error">
        连接问题：{{ lastError }}（正在自动重连）
      </p>
      <p v-if="privacy?.detailsExposed === true" class="text-warning">
        详情已公开：本实例启用了 EXPOSE_LOG_DETAILS，任何能访问此页面的人都能看到正文与客户端版本。
      </p>
    </div>

    <main class="relative min-h-0 flex-1">
      <ol
        ref="scroller"
        class="h-full overflow-y-auto overflow-x-hidden font-mono"
        aria-live="polite"
        aria-relevant="additions"
        @scroll.passive="onScroll"
      >
        <LogRow
          v-for="(event, i) in filtered"
          :key="event.id"
          :event="event"
          :index="i"
          :compact="isNarrow"
          :class="event.id === selectedId ? 'bg-primary/10' : 'hover:bg-base-200/60'"
          class="cursor-pointer"
          @click="select(event)"
        />
        <li v-if="filtered.length === 0" class="px-3 py-6 text-center text-xs text-base-content/50">
          {{ events.length === 0 ? '暂无日志（等待源数据）' : '没有匹配当前筛选条件的记录' }}
        </li>
      </ol>

      <button
        v-if="!follow && newBelow > 0"
        type="button"
        class="btn btn-xs btn-primary absolute bottom-3 left-1/2 -translate-x-1/2 shadow"
        @click="toggleFollow"
      >
        恢复跟随（下方 {{ newBelow }} 条）
      </button>

      <section
        v-if="selected !== null"
        data-testid="detail-panel"
        class="absolute inset-x-0 bottom-0 max-h-[60%] overflow-y-auto border-t border-base-300 bg-base-200/95 p-3 text-xs shadow-lg backdrop-blur"
        aria-label="事件详情"
      >
        <div class="mb-2 flex items-center justify-between gap-2">
          <h2 class="font-semibold">事件详情</h2>
          <button type="button" class="btn btn-xs btn-ghost" @click="selectedId = null">
            关闭（Esc）
          </button>
        </div>
        <dl class="grid grid-cols-[auto,1fr] gap-x-3 gap-y-1 font-mono">
          <dt class="text-base-content/60">ID</dt>
          <dd class="break-all">{{ selected.id }}</dd>
          <dt class="text-base-content/60">时间</dt>
          <dd>{{ selected.timestamp ?? '（无法解析）' }}</dd>
          <dt class="text-base-content/60">接收</dt>
          <dd>{{ selected.observedAt }}</dd>
          <dt class="text-base-content/60">类型</dt>
          <dd>{{ selected.kind }} / {{ selected.parseStatus }}</dd>
          <dt class="text-base-content/60">来源</dt>
          <dd class="break-all">
            {{ selected.source === null ? '未知' : selected.source.addr }}
            <span v-if="selected.source?.port != null">:{{ selected.source.port }}</span>
          </dd>
          <dt class="text-base-content/60">版本</dt>
          <dd class="break-all">{{ selected.details?.version ?? '—' }}</dd>
          <dt class="text-base-content/60">说明</dt>
          <dd class="break-all">{{ selected.details?.note ?? '—' }}</dd>
        </dl>
        <pre class="log-line mt-2 rounded bg-base-300/50 p-2 text-[12px]">{{
          selected.text === '' ? '（无可见文本）' : selected.text
        }}</pre>
      </section>
    </main>
  </div>
</template>
