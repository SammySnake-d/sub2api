<template>
  <div v-if="eligible" class="flex h-6 min-w-[7rem] items-center gap-1">
    <HelpTooltip class="-ml-1" width-class="w-max max-w-[calc(100vw-2rem)]" data-testid="upstream-billing-details">
      <template #trigger>
        <span
          class="cursor-help border-b border-dotted border-gray-300 text-sm font-medium dark:border-dark-600"
          :class="hasEffectiveRate ? 'font-mono text-gray-800 dark:text-gray-200' : statusClass || 'text-gray-400 dark:text-gray-500'"
          data-testid="upstream-billing-rate"
        >
          {{ primaryValue }}
        </span>
      </template>
      <div class="space-y-1">
        <template v-if="hasEffectiveRate && data">
          <p>{{ t('admin.accounts.upstreamBilling.groupRate', { value: data.group_rate_multiplier }) }}</p>
          <p v-if="data.user_rate_multiplier != null">
            {{ t('admin.accounts.upstreamBilling.userRate', { value: data.user_rate_multiplier }) }}
          </p>
          <p>
            {{
              data.peak_rate_enabled
                ? t('admin.accounts.upstreamBilling.peakRate', {
                    start: data.peak_start,
                    end: data.peak_end,
                    value: data.peak_rate_multiplier,
                    timezone: data.timezone
                  })
                : t('admin.accounts.upstreamBilling.noPeakRate')
            }}
          </p>
          <p>{{ t('admin.accounts.upstreamBilling.effectiveRate', { value: currentEffectiveRate ?? '-' }) }}</p>
          <p>{{ t('admin.accounts.upstreamBilling.updatedAt', { value: formatDate(snapshot?.received_at) }) }}</p>
        </template>
        <!-- 「上游不支持」必须先于陈旧数据分支判定：这条快照是探测跑完得到的结论，
             不能退化成和「从未探测」同形的显示，也不能只剩一句过期倍率。 -->
        <template v-else-if="probeState === 'unsupported'">
          <p data-testid="upstream-billing-unsupported-title">
            {{ t('admin.accounts.upstreamBilling.unsupportedTitle') }}
          </p>
          <p data-testid="upstream-billing-unsupported-hint">
            {{ t('admin.accounts.upstreamBilling.unsupportedHint') }}
          </p>
          <p v-if="httpStatus" data-testid="upstream-billing-http-status">
            {{ t('admin.accounts.upstreamBilling.httpStatus', { value: httpStatus }) }}
          </p>
          <p v-if="lastAttemptAt" data-testid="upstream-billing-last-attempt">
            {{ t('admin.accounts.upstreamBilling.lastAttemptAt', { value: formatDate(lastAttemptAt) }) }}
          </p>
          <template v-if="lastDetectedRate != null">
            <p data-testid="upstream-billing-last-rate">
              {{ t('admin.accounts.upstreamBilling.lastDetectedRate', { value: lastDetectedRate }) }}
            </p>
            <p data-testid="upstream-billing-last-time">
              {{ t('admin.accounts.upstreamBilling.lastDetectedAt', { value: formatDate(snapshot?.received_at) }) }}
            </p>
          </template>
        </template>
        <template v-else-if="stale && lastDetectedRate != null">
          <p data-testid="upstream-billing-last-rate">
            {{ t('admin.accounts.upstreamBilling.lastDetectedRate', { value: lastDetectedRate }) }}
          </p>
          <p data-testid="upstream-billing-last-time">
            {{ t('admin.accounts.upstreamBilling.lastDetectedAt', { value: formatDate(snapshot?.received_at) }) }}
          </p>
          <p data-testid="upstream-billing-elapsed">
            {{ t('admin.accounts.upstreamBilling.elapsedSince', { value: elapsedSinceLastSuccess }) }}
          </p>
        </template>
        <!-- 探测失败要给出失败本身的证据（原因 / HTTP 状态 / 连续失败次数），
             否则与「没测到任何东西」无法区分。 -->
        <template v-else-if="probeState === 'failed'">
          <p data-testid="upstream-billing-failure-title">
            {{ t('admin.accounts.upstreamBilling.failedTitle') }}
          </p>
          <p data-testid="upstream-billing-failure-reason">{{ failureReasonLabel }}</p>
          <p v-if="httpStatus" data-testid="upstream-billing-http-status">
            {{ t('admin.accounts.upstreamBilling.httpStatus', { value: httpStatus }) }}
          </p>
          <p v-if="lastAttemptAt" data-testid="upstream-billing-last-attempt">
            {{ t('admin.accounts.upstreamBilling.lastAttemptAt', { value: formatDate(lastAttemptAt) }) }}
          </p>
          <p v-if="failureCount > 1" data-testid="upstream-billing-failure-count">
            {{ t('admin.accounts.upstreamBilling.failureCount', { count: failureCount }) }}
          </p>
        </template>
        <template v-else-if="probeState === 'not-probed'">
          <p data-testid="upstream-billing-not-probed">{{ t('admin.accounts.upstreamBilling.notProbed') }}</p>
          <p data-testid="upstream-billing-not-probed-hint">
            {{ t('admin.accounts.upstreamBilling.notProbedHint') }}
          </p>
        </template>
        <p v-else>{{ statusLabel || '-' }}</p>
        <p
          v-if="probeEnabled && globalProbeEnabled !== false && nextProbeAt"
          data-testid="upstream-billing-next-probe"
        >
          {{
            probeState === 'unsupported'
              ? t('admin.accounts.upstreamBilling.nextRecheckAt', { value: formatDate(nextProbeAt) })
              : t('admin.accounts.upstreamBilling.nextProbeAt', { value: formatDate(nextProbeAt) })
          }}
        </p>
        <p class="mt-2 border-t border-white/15 pt-2" data-testid="upstream-billing-probe-state">
          {{ t('admin.accounts.upstreamBilling.accountProbeState') }}
          <span :class="probeEnabled ? 'text-emerald-400' : 'text-red-400'">
            {{ probeEnabled ? t('admin.accounts.upstreamBilling.enabled') : t('admin.accounts.upstreamBilling.disabled') }}
          </span>
        </p>
        <p
          v-if="globalProbeEnabled === false"
          class="mt-1"
          data-testid="upstream-billing-global-probe-state"
        >
          {{ t('admin.accounts.upstreamBilling.globalProbeState') }}
          <span class="text-red-400">{{ t('admin.accounts.upstreamBilling.disabled') }}</span>
        </p>
      </div>
    </HelpTooltip>
    <span v-if="hasEffectiveRate && statusLabel" :class="statusClass" class="whitespace-nowrap text-[10px] font-medium">
      {{ statusLabel }}
    </span>
    <button
      type="button"
      class="inline-flex h-6 w-6 flex-shrink-0 items-center justify-center rounded transition-colors disabled:cursor-not-allowed disabled:opacity-50"
      :class="probeState === 'unsupported'
        ? 'text-gray-400 hover:bg-gray-100 dark:text-gray-500 dark:hover:bg-dark-700'
        : 'text-blue-600 hover:bg-blue-50 dark:text-blue-400 dark:hover:bg-blue-900/30'"
      :disabled="probing"
      :aria-label="probeActionLabel"
      :title="probeActionLabel"
      data-testid="upstream-billing-probe"
      @click="$emit('probe')"
    >
      <Icon name="refresh" size="xs" :class="{ 'animate-spin': probing }" />
    </button>
  </div>
  <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Icon from '@/components/icons/Icon.vue'
import { formatMultiplier } from '@/utils/formatters'
import type { Account, UpstreamBillingProbeSnapshot } from '@/types'

const props = withDefaults(defineProps<{
  account: Account
  now: number
  probing?: boolean
  globalProbeEnabled?: boolean
}>(), {
  globalProbeEnabled: true
})

defineEmits<{
  (event: 'probe'): void
}>()

const { t } = useI18n()
const CLOCK_SKEW_TOLERANCE_MS = 5 * 60 * 1000
// 探测资格已放宽到全部 API-key 平台（上游是 sub2api 即可应答）。
const eligible = computed(() => props.account.type === 'apikey')
const snapshot = computed<UpstreamBillingProbeSnapshot | undefined>(() => props.account.extra?.upstream_billing_probe)
const data = computed(() => snapshot.value?.data)
const probeEnabled = computed(() => props.account.extra?.upstream_billing_probe_enabled === true)
const nextProbeAt = computed(() => {
  const value = snapshot.value?.next_probe_at
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) ? value : ''
})
const receivedAt = computed(() => typeof snapshot.value?.received_at === 'string' ? Date.parse(snapshot.value.received_at) : Number.NaN)
const freshUntil = computed(() => {
  if (typeof snapshot.value?.fresh_until === 'string') return Date.parse(snapshot.value.fresh_until)
  if (snapshot.value?.status !== 'ok' || typeof snapshot.value.next_probe_at !== 'string') return Number.NaN
  const nextProbeAt = Date.parse(snapshot.value.next_probe_at)
  return Number.isFinite(nextProbeAt) && nextProbeAt > receivedAt.value
    ? receivedAt.value + 2 * (nextProbeAt - receivedAt.value)
    : Number.NaN
})
const validTimestamps = computed(() => {
  if (!Number.isFinite(receivedAt.value) || receivedAt.value > props.now + CLOCK_SKEW_TOLERANCE_MS) return false
  return Number.isFinite(freshUntil.value) && freshUntil.value > receivedAt.value
})
const stale = computed(() => {
  if (!snapshot.value) return false
  if (!Number.isFinite(receivedAt.value)) return snapshot.value.status === 'ok'
  if (!validTimestamps.value) return true
  return props.now > freshUntil.value
})
const parseMinute = (value?: string) => {
  if (typeof value !== 'string') return null
  const match = /^(\d{2}):(\d{2})$/.exec(value)
  if (!match) return null
  const hour = Number(match[1])
  const minute = Number(match[2])
  return hour < 24 && minute < 60 ? hour * 60 + minute : null
}
const minuteInTimeZone = (timestamp: number, timeZone?: string) => {
  if (!timeZone) return null
  try {
    const parts = new Intl.DateTimeFormat('en-GB', {
      timeZone,
      hour: '2-digit',
      minute: '2-digit',
      hourCycle: 'h23'
    }).formatToParts(new Date(timestamp))
    const hour = Number(parts.find(part => part.type === 'hour')?.value)
    const minute = Number(parts.find(part => part.type === 'minute')?.value)
    return Number.isInteger(hour) && Number.isInteger(minute) ? hour * 60 + minute : null
  } catch {
    return null
  }
}
const currentEffectiveRate = computed(() => {
  const billing = data.value
  if (!billing) return null
  if (billing.billing_scope !== 'token') return null
  const base = billing.resolved_rate_multiplier
  if (typeof base !== 'number' || !Number.isFinite(base) || base < 0) return null
  if (typeof billing.peak_rate_enabled !== 'boolean') return null
  if (!billing.peak_rate_enabled) return base
  const start = parseMinute(billing.peak_start)
  const end = parseMinute(billing.peak_end)
  const minute = minuteInTimeZone(props.now, billing.timezone)
  const peak = billing.peak_rate_multiplier
  if (start == null || end == null || minute == null || start >= end || typeof peak !== 'number' || !Number.isFinite(peak) || peak < 0) return null
  const value = minute >= start && minute < end ? base * peak : base
  return Number.isFinite(value) ? value : null
})
const lastDetectedRate = computed(() => {
  const value = data.value?.effective_rate_multiplier
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
    ? Number(value.toPrecision(12))
    : null
})
const elapsedSinceLastSuccess = computed(() => {
  if (!Number.isFinite(receivedAt.value)) return '-'
  const elapsedMinutes = Math.max(0, Math.floor((props.now - receivedAt.value) / 60_000))
  if (elapsedMinutes < 1) return t('admin.accounts.upstreamBilling.justNow')
  if (elapsedMinutes < 60) return t('admin.accounts.upstreamBilling.minutesAgo', { count: elapsedMinutes })
  const elapsedHours = Math.floor(elapsedMinutes / 60)
  if (elapsedHours < 24) return t('admin.accounts.upstreamBilling.hoursAgo', { count: elapsedHours })
  return t('admin.accounts.upstreamBilling.daysAgo', { count: Math.floor(elapsedHours / 24) })
})
const effectiveRate = computed(() => {
  if (!validTimestamps.value || stale.value || !['ok', 'failed'].includes(snapshot.value?.status ?? '')) return '-'
  const value = currentEffectiveRate.value
  return value == null ? '-' : `${formatMultiplier(value)}x`
})
// 探测结果的四种含义必须各自成一个渲染分支：「从未探过」和「探过了但上游没有这个
// 接口」在界面上同形就是在撒谎——用户点完按钮看到同一句话，只会判断成按钮没反应。
type ProbeState = 'not-probed' | 'ok' | 'stale' | 'unsupported' | 'failed'
const probeState = computed<ProbeState>(() => {
  if (!snapshot.value) return 'not-probed'
  // unsupported 是探测跑完的终态结论，优先于「数据过期」：它不会因为等久了而变好。
  if (snapshot.value.status === 'unsupported') return 'unsupported'
  if (stale.value) return 'stale'
  if (snapshot.value.status === 'failed') return 'failed'
  return 'ok'
})
const lastAttemptAt = computed(() => {
  const value = snapshot.value?.last_attempt_at
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) ? value : ''
})
const httpStatus = computed(() => {
  const value = snapshot.value?.http_status
  return typeof value === 'number' && Number.isInteger(value) && value > 0 ? value : 0
})
const failureCount = computed(() => {
  const value = snapshot.value?.failure_count
  return typeof value === 'number' && Number.isInteger(value) && value > 0 ? value : 0
})
// 只翻译后端确实会写入的原因码；未知码原样带出，不猜一个更好听的说法。
const KNOWN_FAILURE_REASONS = new Set([
  'transport_unavailable',
  'missing_api_key',
  'invalid_base_url',
  'proxy_unavailable',
  'request_build_failed',
  'request_failed',
  'empty_response',
  'response_read_failed',
  'response_too_large',
  'http_error',
  'invalid_response'
])
const failureReasonLabel = computed(() => {
  const reason = snapshot.value?.last_error
  if (typeof reason !== 'string' || reason === '') {
    return t('admin.accounts.upstreamBilling.failureReasonUnknown', { code: '-' })
  }
  return KNOWN_FAILURE_REASONS.has(reason)
    ? t(`admin.accounts.upstreamBilling.failureReason.${reason}`)
    : t('admin.accounts.upstreamBilling.failureReasonUnknown', { code: reason })
})
// 上游确定不提供该接口时，探测按钮不再是一个待办动作；保留可点是为了换了上游后
// 能立刻重判，但文案与配色都不再邀请用户去点。
const probeActionLabel = computed(() => probeState.value === 'unsupported'
  ? t('admin.accounts.upstreamBilling.recheckUnsupported')
  : t('admin.accounts.upstreamBilling.manualProbe'))
const statusLabel = computed(() => {
  switch (probeState.value) {
    case 'not-probed':
      return t('admin.accounts.upstreamBilling.notProbed')
    case 'unsupported':
      return t('admin.accounts.upstreamBilling.unsupported')
    case 'stale':
      return t('admin.accounts.upstreamBilling.stale')
    case 'failed':
      return t('admin.accounts.upstreamBilling.failed')
    default:
      return ''
  }
})
const statusClass = computed(() => {
  switch (probeState.value) {
    case 'not-probed':
      return 'text-gray-400 dark:text-gray-500'
    case 'unsupported':
      return 'text-gray-500 dark:text-gray-400'
    case 'stale':
      return 'text-amber-600 dark:text-amber-400'
    case 'failed':
      return 'text-red-600 dark:text-red-400'
    default:
      return ''
  }
})
const hasEffectiveRate = computed(() => effectiveRate.value !== '-')
const primaryValue = computed(() => hasEffectiveRate.value ? effectiveRate.value : statusLabel.value || '-')
const formatDate = (value?: string) => value
  ? new Date(value).toLocaleString(undefined, {
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit'
    })
  : '-'
</script>
