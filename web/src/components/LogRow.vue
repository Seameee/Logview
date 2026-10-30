<script setup lang="ts">
// 单行日志渲染。
//
// 全部内容只用 `{{ }}` 文本插值输出：日志是不可信数据，任何 v-html /
// innerHTML 都可能把日志里的标签或脚本变成可执行内容。
import { computed } from 'vue'

import {
  eventBody,
  formatClock,
  formatTime,
  kindClass,
  kindLabel,
  parseLabel,
  sourceLabel,
} from '../lib/format'
import type { LogEvent } from '../lib/wire'

const props = defineProps<{ event: LogEvent; index: number; compact: boolean }>()

const timeLabel = computed(() =>
  props.compact ? formatClock(props.event.timestamp) : formatTime(props.event.timestamp),
)
const body = computed(() => eventBody(props.event))
const source = computed(() => sourceLabel(props.event))
const note = computed(() => props.event.details?.note ?? null)
const version = computed(() => props.event.details?.version ?? null)
// 隐藏详情的行语气更弱：让"看不到内容"一眼可辨，而不是像正常日志。
const muted = computed(() => props.event.text === '' && props.event.kind !== 'connection')
</script>

<template>
  <li
    data-testid="log-row"
    class="log-line border-b border-base-300/60 px-2 py-1 text-[13px] last:border-b-0"
  >
    <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
      <span
        class="shrink-0 font-mono text-[12px] tabular-nums text-base-content/60"
        :title="event.timestamp ?? '无时间戳'"
      >
        {{ timeLabel }}
      </span>

      <span class="badge badge-xs shrink-0 font-normal" :class="kindClass(event.kind)">
        {{ kindLabel(event.kind) }}
      </span>

      <span
        class="shrink-0 font-mono text-[12px] text-base-content/80"
        :title="`来源：${source}`"
      >
        {{ source }}
      </span>

      <span
        v-if="event.parseStatus !== 'complete'"
        class="shrink-0 rounded bg-base-300/60 px-1 text-[11px] text-base-content/60"
        :title="`解析完整度：${parseLabel(event)}`"
      >
        {{ parseLabel(event) }}
      </span>

      <span
        class="min-w-0 font-mono"
        :class="[
          compact ? 'w-full basis-full' : 'flex-1',
          muted ? 'italic text-base-content/50' : 'text-base-content',
        ]"
      >
        {{ body }}
      </span>

      <span
        v-if="version !== null"
        class="shrink-0 rounded bg-base-300/40 px-1 font-mono text-[11px] text-base-content/60"
        title="客户端版本（已按隐私策略投影）"
      >
        {{ version }}
      </span>
    </div>

    <p
      v-if="note !== null && note !== '' && note !== body"
      class="mt-0.5 pl-2 text-[11px] text-base-content/45"
    >
      {{ note }}
    </p>
  </li>
</template>
