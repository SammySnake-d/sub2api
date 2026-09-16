<template>
  <div
    v-if="quota"
    class="min-w-0 max-w-full space-y-1"
    data-testid="mirasim-quota-cell"
  >
    <!-- 套餐：权威档位徽章 + 达标未结算的目标档位 + 订阅到期日 -->
    <div
      v-if="planLabel || planExpiresText"
      class="flex flex-wrap items-center gap-1"
      data-testid="mirasim-plan"
    >
      <span
        v-if="planLabel"
        :class="['rounded px-1.5 py-0.5 text-[10px] font-semibold', planBadgeClass]"
        :title="planTooltip"
        data-testid="mirasim-plan-badge"
      >{{ planLabel }}<span v-if="planIsClaimed" data-testid="mirasim-plan-claimed">?</span></span>
      <span
        v-if="nextPlanLabel"
        class="text-[10px] text-gray-500 dark:text-gray-400"
        :title="t('admin.accounts.usageWindow.mirasimQuota.pendingUpgradeTooltip')"
        data-testid="mirasim-plan-next"
      >&rarr; {{ nextPlanLabel }}</span>
      <span
        v-if="planExpiresText"
        class="text-[10px] text-gray-500 dark:text-gray-400"
        data-testid="mirasim-plan-expires"
      >{{ planExpiresText }}</span>
    </div>

    <!-- 上游把整个账号的额度挂起：窗口读数已无意义，但仍照原样展示 -->
    <div
      v-if="quota.suspended"
      class="text-[10px] font-medium text-red-600 dark:text-red-400"
      data-testid="mirasim-suspended"
    >
      {{ t('admin.accounts.usageWindow.mirasimQuota.suspended') }}
    </div>

    <!-- 从未探测：显示「未探测」+ 探测按钮。绝不显示 0% —— 「没探到」和「探到 0」是两件事 -->
    <div
      v-if="!hasReading"
      class="flex flex-wrap items-center gap-1.5"
      data-testid="mirasim-unprobed"
    >
      <span class="text-[10px] text-gray-400 dark:text-dark-500">
        {{ t('admin.accounts.usageWindow.mirasimQuota.unprobed') }}
      </span>
      <span
        v-if="staleReading"
        class="text-[10px] text-amber-600 dark:text-amber-400"
        :title="t('admin.accounts.usageWindow.mirasimQuota.probeFailedTooltip')"
        data-testid="mirasim-quota-probe-failed"
      >{{ t('admin.accounts.usageWindow.mirasimQuota.probeFailed') }}</span>
      <button
        type="button"
        class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="probing"
        :title="t('admin.accounts.usageWindow.mirasimQuota.probeTooltip')"
        data-testid="mirasim-quota-probe"
        @click="probe"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': probing }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.usageWindow.mirasimQuota.probe') }}
      </button>
    </div>

    <template v-else>
      <!-- 四个并存窗口的剩余比例：5h / 7d 一行，7d_claude / 7d_fable 一行 -->
      <div class="grid grid-cols-2 gap-x-2 gap-y-0.5" data-testid="mirasim-quota-windows">
        <div v-for="slot in windowSlots" :key="slot.name" :title="slot.tooltip">
          <UsageProgressBar
            v-if="slot.remainingPercent !== null"
            :label="slot.label"
            :utilization="slot.remainingPercent"
            :color="slot.color"
            remaining-capacity
            :data-testid="`mirasim-window-${slot.name}`"
          />
          <!-- 没读数：显示占位文字，不画一条 0% 的空条 -->
          <div v-else class="flex items-center gap-1" :data-testid="`mirasim-window-${slot.name}-empty`">
            <span :class="['w-[32px] shrink-0 rounded px-1 text-center text-[10px] font-medium', slot.labelClass]">
              {{ slot.label }}
            </span>
            <span class="truncate text-[10px] text-gray-400 dark:text-dark-500">{{ slot.placeholder }}</span>
          </div>
        </div>
      </div>

      <!-- 分族受限说明：哪一族的哪个窗口用尽了 -->
      <div
        v-for="restriction in restrictions"
        :key="restriction.window"
        class="text-[10px] font-medium text-red-600 dark:text-red-400"
        :title="restriction.tooltip"
        :data-testid="`mirasim-restriction-${restriction.window}`"
      >
        {{ restriction.text }}
      </div>

      <!-- 上游返回了不属于已知四窗口的名字：原样带出，不静默塞进某个已知槽位 -->
      <div
        v-for="extra in unmappedWindows"
        :key="extra.name"
        class="flex items-center gap-1 text-[10px] text-amber-600 dark:text-amber-400"
        :title="extra.tooltip"
        :data-testid="`mirasim-unmapped-${extra.name}`"
      >
        <svg class="h-2.5 w-2.5 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M12 9v4m0 4h.01M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"
          />
        </svg>
        <span class="truncate">{{ extra.text }}</span>
      </div>

      <!-- 快照时间 + 重新探测 -->
      <div class="flex flex-wrap items-center gap-1">
        <span
          v-if="snapshotText"
          class="text-[10px] text-gray-400 dark:text-dark-500"
          :title="sourceTooltip"
          data-testid="mirasim-quota-snapshot-at"
        >{{ snapshotText }}</span>
        <span
          v-if="staleReading"
          class="text-[10px] text-amber-600 dark:text-amber-400"
          :title="t('admin.accounts.usageWindow.mirasimQuota.staleReadingTooltip')"
          data-testid="mirasim-quota-stale"
        >{{ t('admin.accounts.usageWindow.mirasimQuota.staleReading') }}</span>
        <button
          type="button"
          class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
          :disabled="probing"
          :title="t('admin.accounts.usageWindow.mirasimQuota.probeTooltip')"
          data-testid="mirasim-quota-probe"
          @click="probe"
        >
          <svg
            class="h-2.5 w-2.5"
            :class="{ 'animate-spin': probing }"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
          >
            <path
              stroke-linecap="round"
              stroke-linejoin="round"
              stroke-width="2"
              d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
            />
          </svg>
          {{ t('admin.accounts.usageWindow.mirasimQuota.probe') }}
        </button>
      </div>
    </template>

    <div
      v-if="probeNote"
      class="truncate text-[10px] text-amber-600 dark:text-amber-400"
      :title="probeNote"
      data-testid="mirasim-quota-probe-note"
    >
      {{ probeNote }}
    </div>

    <div
      v-if="probeError"
      class="truncate text-[10px] text-red-600 dark:text-red-400"
      :title="probeError"
      data-testid="mirasim-quota-probe-error"
    >
      {{ truncatedProbeError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { apiClient } from '@/api/client'
import type {
  Account,
  MirasimQuotaProbeResponse,
  MirasimQuotaSnapshot,
  MirasimQuotaWindow,
  MirasimQuotaWindowName
} from '@/types'
import { formatDateOnly, formatDateTimeToMinute } from '@/utils/format'
import UsageProgressBar from './UsageProgressBar.vue'

// ---------------------------------------------------------------------------
// 共享时钟
// ---------------------------------------------------------------------------
// 「某窗口已用尽」取决于 reset_at 是否还在未来，所以这个判断必须跟着时间走 ——
// 否则 reset 过去之后页面会继续挂着「已用尽」，那是在撒谎。但账号列表可能有上百
// 行 mirasim 账号，不能一行一个 timer（UsageProgressBar 就是为此才按需起停）。
// 这里用一个模块级时钟 + 引用计数，全表只有一个 60s interval。
const sharedNow = ref(Date.now())
let clockHolders = 0
let clockTimer: ReturnType<typeof setInterval> | null = null

const retainClock = () => {
  clockHolders += 1
  if (clockHolders === 1 && clockTimer === null && typeof window !== 'undefined') {
    clockTimer = setInterval(() => {
      sharedNow.value = Date.now()
    }, 60_000)
  }
}

const releaseClock = () => {
  clockHolders = Math.max(0, clockHolders - 1)
  if (clockHolders === 0 && clockTimer !== null) {
    clearInterval(clockTimer)
    clockTimer = null
  }
}

type BarColor = 'indigo' | 'emerald' | 'purple' | 'amber'

interface KnownWindow {
  /** 上游窗口名，与 /v1/limits 的 windows[].name 逐字相同。 */
  name: MirasimQuotaWindowName
  /** 紧凑徽章文字，沿用本仓已有的 `7d S` / `7d F` 写法。 */
  label: string
  color: BarColor
  /** 账号级全局窗口（5h / 7d）耗尽 ⇒ 全部模型受限；族窗口耗尽 ⇒ 部分模型受限。 */
  scope: 'account' | 'claude' | 'fable'
}

// 固定四槽位、固定顺序。缺的窗口保留槽位显示占位符，这样同一列纵向可比。
const KNOWN_WINDOWS: KnownWindow[] = [
  { name: '5h', label: '5h', color: 'indigo', scope: 'account' },
  { name: '7d', label: '7d', color: 'emerald', scope: 'account' },
  { name: '7d_claude', label: '7d C', color: 'purple', scope: 'claude' },
  { name: '7d_fable', label: '7d F', color: 'amber', scope: 'fable' }
]

const LABEL_CLASSES: Record<BarColor, string> = {
  indigo: 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-300',
  emerald: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300',
  purple: 'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-300',
  amber: 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
}

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{ updated: [snapshot: MirasimQuotaSnapshot] }>()

const { t } = useI18n()

const snapshot = ref<MirasimQuotaSnapshot | null>(props.account.mirasim_quota ?? null)
const probing = ref(false)
const probeError = ref<string | null>(null)
/** 上一次点击的诚实注脚（例如：这次调用并没有真的走上游）。 */
const probeNote = ref<string | null>(null)

watch(
  () => props.account.mirasim_quota,
  (next) => {
    snapshot.value = next ?? null
    probeError.value = null
    probeNote.value = null
  }
)

onMounted(retainClock)
onBeforeUnmount(releaseClock)

const quota = computed(() => snapshot.value)

// ---------------------------------------------------------------------------
// 读数解析
// ---------------------------------------------------------------------------

/**
 * Go 的零值 `time.Time` 会序列化成 `0001-01-01T00:00:00Z`（结构体上的 omitempty
 * 不生效）。那不是「重置时间在公元 1 年」，是根本没有这个读数。
 */
const parseTimestamp = (value?: string | null): number | null => {
  if (!value) return null
  const ms = new Date(value).getTime()
  if (!Number.isFinite(ms) || ms < Date.UTC(2000, 0, 1)) return null
  return ms
}

const finiteNumber = (value?: number | null): number | null =>
  typeof value === 'number' && Number.isFinite(value) ? value : null

/**
 * 已用比例（0..1），读不出来时返回 null —— null 一路传到 UI 层都不许被当成 0。
 *
 * 绝对值优先：`budget > 0` 才拿 used/budget 算（`budget <= 0` 是「没探到额度」，
 * 不是「额度为 0」，这条与 ma-relay 调度判据 pool.go:1052 同口径）。退到
 * `utilization` 时注意它是 0..1 小数，不是百分数。
 */
const usedRatio = (window: MirasimQuotaWindow): number | null => {
  const budget = finiteNumber(window.budget)
  const used = finiteNumber(window.used)
  if (budget !== null && budget > 0 && used !== null) {
    return used / budget
  }
  return finiteNumber(window.utilization)
}

/** 0..1 比例 → 0..100 百分数，顺手消掉 1-0.9 这类浮点毛刺（保留 2 位）。 */
const clampPercent = (ratio: number): number =>
  Math.round(Math.min(Math.max(ratio, 0), 1) * 10000) / 100


const windowsByName = computed(() => {
  const map = new Map<string, MirasimQuotaWindow>()
  for (const window of quota.value?.windows ?? []) {
    if (typeof window?.name === 'string' && window.name !== '') {
      map.set(window.name, window)
    }
  }
  return map
})

/**
 * 有没有任何读数。`source` 为空且 windows 为空 = 从未探测；此时只展示「未探测」
 * 与探测入口，不渲染任何进度条。
 */
const hasReading = computed(() => {
  const current = quota.value
  if (!current) return false
  if (current.source === 'limits' || current.source === 'headers') return true
  return windowsByName.value.size > 0
})

const resetText = (window: MirasimQuotaWindow): string => {
  const reset = parseTimestamp(window.reset_at)
  return reset === null
    ? t('admin.accounts.usageWindow.mirasimQuota.resetUnknown')
    : t('admin.accounts.usageWindow.mirasimQuota.resetAt', {
        time: formatDateTimeToMinute(new Date(reset))
      })
}

/** 用尽 = 读数满了且重置时刻还在未来（与 ma-relay `quotaExhaustedLocked` 同判据）。 */
const isExhausted = (window: MirasimQuotaWindow, ratio: number | null): boolean => {
  if (ratio === null || ratio < 1) return false
  const reset = parseTimestamp(window.reset_at)
  return reset !== null && reset > sharedNow.value
}

interface WindowSlot {
  name: string
  label: string
  labelClass: string
  color: BarColor
  scope: KnownWindow['scope']
  /** null = 无读数，此时渲染 placeholder 文字而不是进度条。 */
  remainingPercent: number | null
  placeholder: string
  tooltip: string
  exhausted: boolean
}

const windowSlots = computed<WindowSlot[]>(() =>
  KNOWN_WINDOWS.map((known) => {
    const base = {
      name: known.name,
      label: known.label,
      labelClass: LABEL_CLASSES[known.color],
      color: known.color,
      scope: known.scope
    }
    const window = windowsByName.value.get(known.name)

    // 上游这次没返回这个窗口：显示「—」，不画 0% 的条。
    if (!window) {
      return {
        ...base,
        remainingPercent: null,
        placeholder: '—',
        tooltip: `${known.name} · ${t('admin.accounts.usageWindow.mirasimQuota.noReadingTooltip')}`,
        exhausted: false
      }
    }

    const ratio = usedRatio(window)
    const parts: string[] = [known.name]

    // 窗口在，但既没有绝对额度也没有使用率：明说「剩余未知」。
    if (ratio === null) {
      parts.push(t('admin.accounts.usageWindow.mirasimQuota.remainingUnknown'))
      parts.push(resetText(window))
      return {
        ...base,
        remainingPercent: null,
        placeholder: t('admin.accounts.usageWindow.mirasimQuota.remainingUnknown'),
        tooltip: parts.join(' · '),
        exhausted: false
      }
    }

    const remainingPercent = clampPercent(1 - ratio)
    parts.push(
      t('admin.accounts.usageWindow.mirasimQuota.remaining', {
        percent: Math.round(remainingPercent)
      })
    )
    // utilization 可能 > 1（1.02 = 用掉了额度的 102%）。剩余只能 clamp 到 0，
    // 但超额这件事不能就此消失 —— 原样写进 tooltip。
    if (ratio > 1) {
      parts.push(
        t('admin.accounts.usageWindow.mirasimQuota.overUsed', {
          percent: Math.round(ratio * 100)
        })
      )
    }
    const budget = finiteNumber(window.budget)
    const used = finiteNumber(window.used)
    if (budget !== null && budget > 0 && used !== null) {
      parts.push(
        t('admin.accounts.usageWindow.mirasimQuota.usedOfBudget', {
          used: Math.round(used).toLocaleString(),
          budget: Math.round(budget).toLocaleString()
        })
      )
    }
    parts.push(resetText(window))

    return {
      ...base,
      remainingPercent,
      placeholder: '',
      tooltip: parts.join(' · '),
      exhausted: isExhausted(window, ratio)
    }
  })
)

/**
 * 上游返回的、不在已知四槽位里的窗口名。
 *
 * **绝不静默映射。** `7d_claude` / `7d_fable` 这两个 token 目前仍是推断值（真实
 * anthropic 响应头里出现过 `7d_oi`）。把陌生名字当成某个已知窗口采纳，会把真实的
 * 多日冷却显示成别的窗口的读数；丢掉它则等于没看见上游改了口径。所以原样带出来。
 */
const unmappedWindows = computed(() => {
  const known = new Set<string>(KNOWN_WINDOWS.map((item) => item.name))
  return (quota.value?.windows ?? [])
    .filter((window) => typeof window?.name === 'string' && window.name !== '' && !known.has(window.name))
    .map((window) => {
      const ratio = usedRatio(window)
      const remaining =
        ratio === null
          ? t('admin.accounts.usageWindow.mirasimQuota.remainingUnknown')
          : t('admin.accounts.usageWindow.mirasimQuota.remaining', {
              percent: Math.round(clampPercent(1 - ratio))
            })
      return {
        name: window.name,
        text: `${t('admin.accounts.usageWindow.mirasimQuota.unmappedWindow', { name: window.name })} · ${remaining}`,
        tooltip: `${t('admin.accounts.usageWindow.mirasimQuota.unmappedWindowTooltip')} · ${resetText(window)}`
      }
    })
})

/**
 * 分族受限说明，一个用尽的窗口一行。
 * 账号级窗口（5h / 7d）用尽 ⇒ 「全部模型受限 · 7d 已用尽」。
 * 族窗口用尽 ⇒ 「部分模型受限 · Claude（非 Fable）· 7d_claude 已用尽」。
 */
const restrictions = computed(() =>
  windowSlots.value
    .filter((slot) => slot.exhausted)
    .map((slot) => {
      const scopeLabel =
        slot.scope === 'claude'
          ? t('admin.accounts.usageWindow.mirasimQuota.scopeClaude')
          : slot.scope === 'fable'
            ? t('admin.accounts.usageWindow.mirasimQuota.scopeFable')
            : ''
      const head = scopeLabel
        ? `${t('admin.accounts.usageWindow.mirasimQuota.restrictedPartial')} · ${scopeLabel}`
        : t('admin.accounts.usageWindow.mirasimQuota.restrictedAll')
      return {
        window: slot.name,
        text: `${head} · ${t('admin.accounts.usageWindow.mirasimQuota.windowExhausted', { window: slot.name })}`,
        tooltip: slot.tooltip
      }
    })
)

// ---------------------------------------------------------------------------
// 套餐 / 快照时间
// ---------------------------------------------------------------------------

const planLabel = computed(() => (quota.value?.plan ?? '').trim().toUpperCase())

/**
 * 档位是探回来的事实还是导入时的声明。
 * `claimed` 已知会虚标（「标 max 实为 plus」），所以徽章按等级换样式并在
 * tooltip 里说明来源 —— 把未核实的声明画成实心事实徽章就是那个 bug 的 UI 版本。
 */
const planIsClaimed = computed(() => quota.value?.plan_source === 'claimed')

const planBadgeClass = computed(() =>
  planIsClaimed.value
    ? 'border border-dashed border-gray-400 text-gray-600 dark:border-dark-500 dark:text-gray-300'
    : 'bg-violet-100 text-violet-700 dark:bg-violet-900/40 dark:text-violet-300'
)

const planTooltip = computed(() => {
  if (quota.value?.plan_source === 'authoritative') {
    return t('admin.accounts.usageWindow.mirasimQuota.planAuthoritative')
  }
  if (planIsClaimed.value) {
    return t('admin.accounts.usageWindow.mirasimQuota.planClaimed')
  }
  return ''
})

/** 达标未结算：当前档位与下一档位不同，说明升级还没落地。 */
const nextPlanLabel = computed(() => {
  const next = (quota.value?.next_plan ?? '').trim().toUpperCase()
  return next && next !== planLabel.value ? next : ''
})

const planExpiresText = computed(() => {
  const expires = parseTimestamp(quota.value?.plan_expires_at)
  if (expires === null) return ''
  return t('admin.accounts.usageWindow.mirasimQuota.planExpiresAt', {
    date: formatDateOnly(new Date(expires))
  })
})

const snapshotText = computed(() => {
  // observed_at 是 windows 这份读数被取到的时刻，不是这一行被渲染的时刻。
  const at = parseTimestamp(quota.value?.observed_at)
  if (at === null) return ''
  return t('admin.accounts.usageWindow.mirasimQuota.snapshotAt', {
    time: formatDateTimeToMinute(new Date(at))
  })
})

/**
 * 上次探测失败：windows 是上一次的好读数被带过来的，不是新鲜读数。
 * 不标出来的话，运营者会把陈旧读数当成当前额度。
 */
const staleReading = computed(() => quota.value?.status === 'failed')

const sourceTooltip = computed(() => {
  if (quota.value?.source === 'limits') {
    return t('admin.accounts.usageWindow.mirasimQuota.sourceLimits')
  }
  if (quota.value?.source === 'headers') {
    return t('admin.accounts.usageWindow.mirasimQuota.sourceHeaders')
  }
  return ''
})

// ---------------------------------------------------------------------------
// 主动探测
// ---------------------------------------------------------------------------

const truncatedProbeError = computed(() => {
  if (!probeError.value) return ''
  return probeError.value.length > 80 ? `${probeError.value.slice(0, 80)}...` : probeError.value
})

const extractErrorMessage = (error: unknown): string => {
  const err = error as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const probe = async () => {
  if (probing.value) return
  probing.value = true
  probeError.value = null
  probeNote.value = null
  try {
    // 直接打 admin 端点：api/admin/accounts.ts 不在本次改动归属内。
    const { data } = await apiClient.post<MirasimQuotaProbeResponse>(
      `/admin/accounts/${props.account.id}/mirasim-quota`
    )
    if (data?.quota) {
      snapshot.value = data.quota
      emit('updated', data.quota)
    }
    // probed=false 表示这次根本没走上游，只是把存量读数重新读了一遍。
    // 在它上面说「探测成功」是运营者无法察觉的谎，所以照实说。
    probeNote.value = data?.probed
      ? null
      : t('admin.accounts.usageWindow.mirasimQuota.probeNotPerformed')
    if (!data?.quota) {
      probeError.value = t('admin.accounts.usageWindow.mirasimQuota.probeNoSnapshot')
    }
  } catch (error) {
    probeError.value = extractErrorMessage(error)
  } finally {
    probing.value = false
  }
}
</script>
