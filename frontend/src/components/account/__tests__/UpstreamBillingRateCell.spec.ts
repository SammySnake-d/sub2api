import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import UpstreamBillingRateCell from '../UpstreamBillingRateCell.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${Object.values(params).join(',')}` : key
    })
  }
})

const makeAccount = (overrides: Partial<Account> = {}): Account => ({
  id: 1,
  name: 'upstream',
  platform: 'openai',
  type: 'apikey',
  proxy_id: null,
  concurrency: 1,
  priority: 1,
  status: 'active',
  error_message: null,
  last_used_at: null,
  expires_at: null,
  auto_pause_on_expired: false,
  created_at: '2026-07-13T00:00:00Z',
  updated_at: '2026-07-13T00:00:00Z',
  schedulable: true,
  rate_limited_at: null,
  rate_limit_reset_at: null,
  overload_until: null,
  temp_unschedulable_until: null,
  temp_unschedulable_reason: null,
  session_window_start: null,
  session_window_end: null,
  session_window_status: null,
  ...overrides
})

const billingData = {
  object: 'sub2api.key_billing' as const,
  schema_version: 1 as const,
  billing_scope: 'token' as const,
  group_rate_multiplier: 0.8,
  resolved_rate_multiplier: 0.6,
  peak_rate_enabled: true,
  peak_start: '09:00',
  peak_end: '18:00',
  peak_rate_multiplier: 1.5,
  applied_peak_multiplier: 1.5,
  effective_rate_multiplier: 0.9,
  timezone: 'Asia/Shanghai',
  observed_at: '2026-07-13T00:00:00Z'
}

describe('UpstreamBillingRateCell', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-07-13T00:30:00Z'))
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('recomputes the current effective rate and keeps the icon-only probe action', async () => {
    const wrapper = mount(UpstreamBillingRateCell, {
      props: {
        account: makeAccount({
          extra: {
            upstream_billing_probe_enabled: true,
            upstream_billing_probe: {
              status: 'ok',
              data: billingData,
              received_at: '2026-07-13T00:00:00Z',
              fresh_until: '2026-07-14T00:00:00Z',
              last_attempt_at: '2026-07-13T00:00:00Z',
              next_probe_at: '2026-07-13T00:30:00Z'
            }
          }
        }),
        now: Date.now()
      }
    })

    expect(wrapper.text()).toContain('0.60x')
    await wrapper.setProps({ now: Date.parse('2026-07-13T01:00:00Z') })
    expect(wrapper.text()).toContain('0.90x')
    await wrapper.setProps({ now: Date.parse('2026-07-13T10:00:00Z') })
    expect(wrapper.text()).toContain('0.60x')
    expect(wrapper.text()).not.toContain('admin.accounts.upstreamBilling.latest')
    expect(wrapper.get('[data-testid="upstream-billing-probe"]').text()).toBe('')
    expect(wrapper.get('[data-testid="upstream-billing-probe"]').attributes('aria-label')).toBe(
      'admin.accounts.upstreamBilling.manualProbe'
    )
  })

  it('uses retained failed data only while it is still fresh', async () => {
    const account = makeAccount({
      extra: {
        upstream_billing_probe: {
          status: 'ok',
          data: billingData,
          received_at: '2026-07-12T22:00:00Z',
          fresh_until: '2026-07-12T23:00:00Z',
          last_attempt_at: '2026-07-12T22:00:00Z',
          next_probe_at: '2026-07-12T22:30:00Z'
        }
      }
    })
    const wrapper = mount(UpstreamBillingRateCell, { props: { account, now: Date.now() } })
    expect(wrapper.text()).toContain('admin.accounts.upstreamBilling.stale')
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('admin.accounts.upstreamBilling.stale')

    await wrapper.setProps({
      account: makeAccount({
        extra: {
          upstream_billing_probe: {
            status: 'failed',
            data: billingData,
            received_at: '2026-07-13T00:00:00Z',
            fresh_until: '2026-07-13T01:00:00Z',
            last_attempt_at: '2026-07-13T00:00:00Z',
            next_probe_at: '2026-07-13T01:00:00Z',
            last_error: 'http_error'
          }
        }
      })
    })
    expect(wrapper.text()).toContain('0.60x')
    expect(wrapper.text()).toContain('admin.accounts.upstreamBilling.failed')

    await wrapper.setProps({ now: Date.parse('2026-07-13T01:00:00Z') })
    expect(wrapper.text()).toContain('0.90x')
    expect(wrapper.text()).not.toContain('admin.accounts.upstreamBilling.stale')

    await wrapper.setProps({ now: Date.parse('2026-07-13T01:00:00.001Z') })
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('admin.accounts.upstreamBilling.stale')
    expect(wrapper.text()).toContain('admin.accounts.upstreamBilling.stale')

    await wrapper.setProps({
      now: Date.now(),
      account: makeAccount({
        extra: {
          upstream_billing_probe: {
            status: 'failed',
            data: billingData,
            received_at: '2026-07-12T22:00:00Z',
            fresh_until: '2026-07-12T23:00:00Z',
            last_attempt_at: '2026-07-13T00:00:00Z',
            next_probe_at: '2026-07-13T01:00:00Z',
            last_error: 'http_error'
          }
        }
      })
    })
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('admin.accounts.upstreamBilling.stale')
    expect(wrapper.text()).toContain('admin.accounts.upstreamBilling.stale')
  })

  it('shows stale snapshot details, local next probe time, and the account probe state', async () => {
    const wrapper = mount(UpstreamBillingRateCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({
          extra: {
            upstream_billing_probe_enabled: true,
            upstream_billing_probe: {
              status: 'ok',
              data: billingData,
              received_at: '2026-07-12T22:00:00Z',
              fresh_until: '2026-07-12T23:00:00Z',
              last_attempt_at: '2026-07-12T22:00:00Z',
              next_probe_at: '2026-07-13T01:00:00Z'
            }
          }
        }),
        now: Date.now()
      }
    })

    expect(wrapper.getComponent(HelpTooltip).props('widthClass')).toBe('w-max max-w-[calc(100vw-2rem)]')
    await wrapper.get('[data-testid="upstream-billing-details"]').trigger('mouseenter')
    await flushPromises()

    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    expect(tooltip.textContent).toContain('admin.accounts.upstreamBilling.lastDetectedRate:0.9')
    expect(tooltip.textContent).toContain('admin.accounts.upstreamBilling.lastDetectedAt:')
    expect(tooltip.textContent).toContain('admin.accounts.upstreamBilling.elapsedSince:admin.accounts.upstreamBilling.hoursAgo:2')
    expect(tooltip.textContent).toContain('admin.accounts.upstreamBilling.nextProbeAt:')
    expect(tooltip.textContent).not.toContain('admin.accounts.upstreamBilling.stale')
    expect(tooltip.querySelector('[data-testid="upstream-billing-probe-state"] span')?.className).toContain('text-emerald-400')

    await wrapper.setProps({
      account: makeAccount({
        extra: {
          upstream_billing_probe_enabled: false,
          upstream_billing_probe: {
            status: 'unsupported',
            last_attempt_at: '2026-07-13T00:00:00Z',
            next_probe_at: '2026-07-13T01:00:00Z'
          }
        }
      })
    })
    expect(tooltip.querySelector('[data-testid="upstream-billing-next-probe"]')).toBeNull()
    expect(tooltip.querySelector('[data-testid="upstream-billing-probe-state"] span')?.className).toContain('text-red-400')
    wrapper.unmount()
  })

  it('stacks the global-off state below the account state and hides it when globally enabled', async () => {
    const wrapper = mount(UpstreamBillingRateCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({
          extra: {
            upstream_billing_probe_enabled: true,
            upstream_billing_probe: {
              status: 'unsupported',
              last_attempt_at: '2026-07-13T00:00:00Z',
              next_probe_at: '2026-07-13T01:00:00Z'
            }
          }
        }),
        globalProbeEnabled: false,
        now: Date.now()
      }
    })

    await wrapper.get('[data-testid="upstream-billing-details"]').trigger('mouseenter')
    await flushPromises()

    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    const accountState = tooltip.querySelector('[data-testid="upstream-billing-probe-state"]')
    const globalState = tooltip.querySelector('[data-testid="upstream-billing-global-probe-state"]')
    expect(accountState?.querySelector('span')?.className).toContain('text-emerald-400')
    expect(globalState?.textContent).toContain('admin.accounts.upstreamBilling.globalProbeState')
    expect(globalState?.querySelector('span')?.className).toContain('text-red-400')
    expect(tooltip.querySelector('[data-testid="upstream-billing-next-probe"]')).toBeNull()

    await wrapper.setProps({ globalProbeEnabled: true })
    expect(tooltip.querySelector('[data-testid="upstream-billing-global-probe-state"]')).toBeNull()
    expect(tooltip.querySelector('[data-testid="upstream-billing-next-probe"]')).not.toBeNull()

    await wrapper.setProps({
      globalProbeEnabled: false,
      account: makeAccount({
        extra: { upstream_billing_probe_enabled: false }
      })
    })
    expect(accountState?.querySelector('span')?.className).toContain('text-red-400')
    expect(tooltip.querySelector('[data-testid="upstream-billing-global-probe-state"]')).not.toBeNull()
    expect(tooltip.querySelector('[data-testid="upstream-billing-next-probe"]')).toBeNull()
    wrapper.unmount()
  })

  it('emits manual probe commands only for eligible accounts', async () => {
    const wrapper = mount(UpstreamBillingRateCell, {
      props: { account: makeAccount(), now: Date.now() }
    })
    await wrapper.get('[data-testid="upstream-billing-probe"]').trigger('click')
    expect(wrapper.emitted('probe')).toHaveLength(1)

    // 探测已放宽到全部 API-key 平台：grok API-key 账号同样可探测。
    await wrapper.setProps({ account: makeAccount({ platform: 'grok' }) })
    await wrapper.get('[data-testid="upstream-billing-probe"]').trigger('click')
    expect(wrapper.emitted('probe')).toHaveLength(2)

    await wrapper.setProps({ account: makeAccount({ type: 'oauth' }) })
    expect(wrapper.findAll('button')).toHaveLength(0)
    expect(wrapper.text()).toBe('-')
  })

  it('fails neutral for malformed data and timestamps', async () => {
    const malformedAccount = (
      dataOverrides: Partial<typeof billingData> = {},
      snapshotOverrides: Record<string, unknown> = {}
    ) => makeAccount({
      extra: {
        upstream_billing_probe: {
          status: 'ok',
          data: { ...billingData, ...dataOverrides },
          received_at: '2026-07-13T00:00:00Z',
          fresh_until: '2026-07-13T01:00:00Z',
          last_attempt_at: '2026-07-13T00:00:00Z',
          next_probe_at: '2026-07-13T01:00:00Z',
          ...snapshotOverrides
        }
      }
    })
    const wrapper = mount(UpstreamBillingRateCell, {
      props: {
        account: malformedAccount({
          resolved_rate_multiplier: -1,
          peak_rate_enabled: false,
          effective_rate_multiplier: -1
        }),
        now: Date.now()
      }
    })

    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('-')
    await wrapper.setProps({ account: malformedAccount({ billing_scope: 'request' as 'token' }) })
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('-')
    await wrapper.setProps({ account: malformedAccount({}, { received_at: 'not-a-time' }) })
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('admin.accounts.upstreamBilling.stale')
    await wrapper.setProps({ account: malformedAccount({}, { received_at: '2026-07-13T00:31:00Z' }) })
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('0.60x')
    await wrapper.setProps({ account: malformedAccount({}, { received_at: '2026-07-13T00:36:00Z' }) })
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('admin.accounts.upstreamBilling.stale')
    await wrapper.setProps({ account: malformedAccount({}, { fresh_until: '2026-07-12T23:59:00Z' }) })
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('admin.accounts.upstreamBilling.stale')

    await wrapper.setProps({
      account: makeAccount({
        extra: {
          upstream_billing_probe: {
            status: 'failed',
            last_attempt_at: '2026-07-13T00:00:00Z',
            next_probe_at: '2026-07-13T01:00:00Z',
            last_error: 'network_error'
          }
        }
      })
    })
    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe('admin.accounts.upstreamBilling.failed')
    expect(wrapper.text()).toContain('admin.accounts.upstreamBilling.failed')
    expect(wrapper.text()).not.toContain('admin.accounts.upstreamBilling.stale')
  })

  it('uses unsupported as the primary tooltip trigger without a dash', () => {
    const wrapper = mount(UpstreamBillingRateCell, {
      props: {
        account: makeAccount({
          extra: {
            upstream_billing_probe: {
              status: 'unsupported',
              last_attempt_at: '2026-07-13T00:00:00Z',
              next_probe_at: '2026-07-13T00:30:00Z',
              last_error: 'unsupported'
            }
          }
        }),
        now: Date.now()
      }
    })

    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe(
      'admin.accounts.upstreamBilling.unsupported'
    )
    expect(wrapper.text()).not.toContain('-admin.accounts.upstreamBilling.unsupported')
  })

  // 「从没探过」与「探过了、上游没有这个接口」是两个结论，界面上必须分得开：
  // 同形显示会让用户把已完成的探测读成按钮没反应。
  it('separates never-probed from a completed upstream-unsupported verdict', async () => {
    const wrapper = mount(UpstreamBillingRateCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({ extra: { upstream_billing_probe_enabled: true } }),
        now: Date.now()
      }
    })
    await wrapper.get('[data-testid="upstream-billing-details"]').trigger('mouseenter')
    await flushPromises()
    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    const probeButton = () => wrapper.get('[data-testid="upstream-billing-probe"]')
    const tooltipText = (testid: string) =>
      tooltip.querySelector(`[data-testid="${testid}"]`)?.textContent?.trim()

    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe(
      'admin.accounts.upstreamBilling.notProbed'
    )
    expect(tooltipText('upstream-billing-not-probed-hint')).toBe(
      'admin.accounts.upstreamBilling.notProbedHint'
    )
    expect(tooltip.querySelector('[data-testid="upstream-billing-unsupported-hint"]')).toBeNull()
    expect(probeButton().attributes('aria-label')).toBe('admin.accounts.upstreamBilling.manualProbe')
    expect(probeButton().classes()).toContain('text-blue-600')

    await wrapper.setProps({
      account: makeAccount({
        extra: {
          upstream_billing_probe_enabled: true,
          upstream_billing_probe: {
            status: 'unsupported',
            http_status: 404,
            failure_count: 1,
            last_attempt_at: '2026-07-13T00:00:00Z',
            next_probe_at: '2026-07-13T04:00:00Z',
            last_error: 'unsupported'
          }
        }
      })
    })

    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe(
      'admin.accounts.upstreamBilling.unsupported'
    )
    expect(tooltipText('upstream-billing-unsupported-title')).toBe(
      'admin.accounts.upstreamBilling.unsupportedTitle'
    )
    expect(tooltipText('upstream-billing-unsupported-hint')).toBe(
      'admin.accounts.upstreamBilling.unsupportedHint'
    )
    expect(tooltipText('upstream-billing-http-status')).toBe(
      'admin.accounts.upstreamBilling.httpStatus:404'
    )
    // 时间串按运行环境时区渲染，断言只钉住「用的是自动重判文案而不是待办式的下次探测」。
    expect(tooltipText('upstream-billing-next-probe')).toContain(
      'admin.accounts.upstreamBilling.nextRecheckAt:'
    )
    expect(tooltipText('upstream-billing-next-probe')).not.toContain(
      'admin.accounts.upstreamBilling.nextProbeAt'
    )
    expect(tooltip.querySelector('[data-testid="upstream-billing-not-probed-hint"]')).toBeNull()
    expect(probeButton().attributes('aria-label')).toBe(
      'admin.accounts.upstreamBilling.recheckUnsupported'
    )
    expect(probeButton().classes()).not.toContain('text-blue-600')
    wrapper.unmount()
  })

  it('names the probe failure reason and keeps unknown codes verbatim', async () => {
    const failedAccount = (lastError: string) => makeAccount({
      extra: {
        upstream_billing_probe: {
          status: 'failed',
          failure_count: 3,
          http_status: 500,
          last_attempt_at: '2026-07-13T00:00:00Z',
          next_probe_at: '2026-07-13T01:00:00Z',
          last_error: lastError
        }
      }
    })
    const wrapper = mount(UpstreamBillingRateCell, {
      attachTo: document.body,
      props: { account: failedAccount('http_error'), now: Date.now() }
    })
    await wrapper.get('[data-testid="upstream-billing-details"]').trigger('mouseenter')
    await flushPromises()
    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    const tooltipText = (testid: string) =>
      tooltip.querySelector(`[data-testid="${testid}"]`)?.textContent?.trim()

    expect(wrapper.get('[data-testid="upstream-billing-rate"]').text()).toBe(
      'admin.accounts.upstreamBilling.failed'
    )
    expect(tooltipText('upstream-billing-failure-title')).toBe(
      'admin.accounts.upstreamBilling.failedTitle'
    )
    expect(tooltipText('upstream-billing-failure-reason')).toBe(
      'admin.accounts.upstreamBilling.failureReason.http_error'
    )
    expect(tooltipText('upstream-billing-failure-count')).toBe(
      'admin.accounts.upstreamBilling.failureCount:3'
    )

    await wrapper.setProps({ account: failedAccount('network_error') })
    expect(tooltipText('upstream-billing-failure-reason')).toBe(
      'admin.accounts.upstreamBilling.failureReasonUnknown:network_error'
    )
    wrapper.unmount()
  })
})
