import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import MirasimQuotaCell from '../MirasimQuotaCell.vue'
import UsageProgressBar from '../UsageProgressBar.vue'
import zh from '@/i18n/locales/zh'
import type { Account, MirasimQuotaSnapshot, MirasimQuotaWindow } from '@/types'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))

vi.mock('@/api/client', () => ({
  apiClient: { post }
}))

// 用真实的中文文案解析 key（只补一个最小插值器）：这些断言要证明运营者真正看到的
// 那句话是对的，而不是证明「某个 key 被引用了」。
const resolve = (key: string): unknown =>
  key.split('.').reduce<unknown>((acc, part) => {
    if (acc && typeof acc === 'object') return (acc as Record<string, unknown>)[part]
    return undefined
  }, zh)

const translate = (key: string, params?: Record<string, unknown>): string => {
  const raw = resolve(key)
  if (typeof raw !== 'string') return key
  return raw.replace(/\{(\w+)\}/g, (_, name: string) => String(params?.[name] ?? `{${name}}`))
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: translate })
  }
})

const FUTURE = '2099-01-01T00:00:00Z'
const PAST = '2001-01-01T00:00:00Z'

/** 窗口默认全 null —— 后端 DTO 里这四个字段是指针，null 表示「上游没给」。 */
const win = (name: string, over: Partial<MirasimQuotaWindow> = {}): MirasimQuotaWindow => ({
  name,
  used: null,
  budget: null,
  utilization: null,
  reset_at: null,
  ...over
})

/** 快照默认是「mirasim 账号，但两半的生产者都还没写过东西」。 */
const snap = (over: Partial<MirasimQuotaSnapshot> = {}): MirasimQuotaSnapshot => ({
  source: '',
  status: '',
  suspended: false,
  observed_at: null,
  windows: [],
  plan: '',
  next_plan: '',
  plan_expires_at: '',
  plan_source: 'unknown',
  ...over
})

const account = (quota: MirasimQuotaSnapshot | null): Account =>
  ({
    id: 42,
    name: 'mirasim-1',
    platform: 'anthropic',
    type: 'apikey',
    mirasim_quota: quota
  }) as Account

const mountCell = (quota: MirasimQuotaSnapshot | null) =>
  mount(MirasimQuotaCell, { props: { account: account(quota) } })

describe('MirasimQuotaCell', () => {
  beforeEach(() => {
    post.mockReset()
  })

  it('非 mirasim 账号（mirasim_quota 为 null）什么都不渲染', () => {
    const wrapper = mountCell(null)
    expect(wrapper.find('[data-testid="mirasim-quota-cell"]').exists()).toBe(false)
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
    expect(wrapper.text()).toBe('')
  })

  it('从未探测时显示「未探测」和探测入口，绝不画 0% 的条', () => {
    const wrapper = mountCell(snap())

    expect(wrapper.get('[data-testid="mirasim-unprobed"]').text()).toContain('未探测')
    expect(wrapper.find('[data-testid="mirasim-quota-probe"]').exists()).toBe(true)
    // 关键：一条进度条都不能有 —— 0% 和「没探到」是两件事。
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
    expect(wrapper.text()).not.toContain('0%')
    expect(wrapper.find('[data-testid="mirasim-quota-windows"]').exists()).toBe(false)
  })

  it('四个窗口按 5h / 7d / 7d C / 7d F 渲染剩余比例，小数 utilization 换算成百分数', () => {
    const wrapper = mountCell(
      snap({
        source: 'limits',
        status: 'ok',
        observed_at: '2026-09-16T10:00:00Z',
        windows: [
          win('5h', { used: 25, budget: 100, utilization: 0.25, reset_at: FUTURE }),
          win('7d', { used: 300, budget: 1000, utilization: 0.3, reset_at: FUTURE }),
          win('7d_claude', { used: 900, budget: 1000, utilization: 0.9, reset_at: FUTURE }),
          win('7d_fable', { used: 100, budget: 1000, utilization: 0.1, reset_at: FUTURE })
        ]
      })
    )

    const bars = wrapper.findAllComponents(UsageProgressBar)
    expect(bars).toHaveLength(4)
    expect(bars.map((bar) => bar.props('label'))).toEqual(['5h', '7d', '7d C', '7d F'])
    // 显示的是「剩余」，不是「已用」。
    expect(bars.map((bar) => bar.props('utilization'))).toEqual([75, 70, 10, 90])
    expect(bars.every((bar) => bar.props('remainingCapacity') === true)).toBe(true)
    expect(bars.map((bar) => bar.props('color'))).toEqual(['indigo', 'emerald', 'purple', 'amber'])
    expect(wrapper.get('[data-testid="mirasim-quota-snapshot-at"]').text()).toContain('快照')
    expect(wrapper.find('[data-testid="mirasim-quota-stale"]').exists()).toBe(false)
  })

  it('windows 里没有的窗口显示「—」，不补一条 0% 的空条', () => {
    const wrapper = mountCell(
      snap({
        source: 'limits',
        windows: [win('5h', { used: 10, budget: 100, utilization: 0.1, reset_at: FUTURE })]
      })
    )

    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(1)
    for (const name of ['7d', '7d_claude', '7d_fable']) {
      const empty = wrapper.get(`[data-testid="mirasim-window-${name}-empty"]`)
      expect(empty.text()).toContain('—')
      expect(empty.text()).not.toContain('%')
    }
  })

  it('窗口在但没有绝对额度也没有使用率时显示「剩余未知」，不画条', () => {
    const wrapper = mountCell(
      snap({
        source: 'limits',
        windows: [
          win('5h', { used: 0, budget: 0, reset_at: FUTURE }),
          win('7d', { used: 200, budget: 1000, utilization: 0.2, reset_at: FUTURE })
        ]
      })
    )

    expect(wrapper.get('[data-testid="mirasim-window-5h-empty"]').text()).toContain('剩余未知')
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(1)
  })

  it('被动响应头来源只有 utilization 时照样算剩余', () => {
    const wrapper = mountCell(
      snap({
        source: 'headers',
        observed_at: '2026-09-16T10:00:00Z',
        windows: [win('7d', { utilization: 0.42, reset_at: FUTURE })]
      })
    )

    const bars = wrapper.findAllComponents(UsageProgressBar)
    expect(bars).toHaveLength(1)
    expect(bars[0].props('utilization')).toBeCloseTo(58, 6)
    expect(wrapper.get('[data-testid="mirasim-quota-snapshot-at"]').text()).toContain('快照')
  })

  it('utilization 超过 1 时剩余压到 0，但超额事实写进 tooltip', () => {
    const wrapper = mountCell(
      snap({
        source: 'headers',
        windows: [win('7d', { utilization: 1.02, reset_at: FUTURE })]
      })
    )

    const bar = wrapper.findAllComponents(UsageProgressBar)[0]
    expect(bar.props('utilization')).toBe(0)
    expect(wrapper.get('[data-testid="mirasim-quota-windows"]').html()).toContain('已用 102%')
  })

  it('7d_claude 用尽时给出分族说明', () => {
    const wrapper = mountCell(
      snap({
        source: 'limits',
        windows: [
          win('5h', { used: 10, budget: 100, utilization: 0.1, reset_at: FUTURE }),
          win('7d', { used: 100, budget: 1000, utilization: 0.1, reset_at: FUTURE }),
          win('7d_claude', { used: 1000, budget: 1000, utilization: 1, reset_at: FUTURE }),
          win('7d_fable', { used: 100, budget: 1000, utilization: 0.1, reset_at: FUTURE })
        ]
      })
    )

    expect(wrapper.get('[data-testid="mirasim-restriction-7d_claude"]').text()).toBe(
      '部分模型受限 · Claude（非 Fable） · 7d_claude 已用尽'
    )
    expect(wrapper.find('[data-testid="mirasim-restriction-7d_fable"]').exists()).toBe(false)
  })

  it('账号级窗口用尽说全部受限；已经过了 reset 的窗口不算用尽', () => {
    const wrapper = mountCell(
      snap({
        source: 'limits',
        windows: [
          win('7d', { used: 1000, budget: 1000, utilization: 1, reset_at: FUTURE }),
          win('7d_fable', { used: 1000, budget: 1000, utilization: 1, reset_at: PAST })
        ]
      })
    )

    expect(wrapper.get('[data-testid="mirasim-restriction-7d"]').text()).toBe(
      '全部模型受限 · 7d 已用尽'
    )
    expect(wrapper.find('[data-testid="mirasim-restriction-7d_fable"]').exists()).toBe(false)
  })

  it('上游给的陌生窗口名原样带出，不静默当成 7d_fable', () => {
    const wrapper = mountCell(
      snap({
        source: 'limits',
        windows: [win('7d_oi', { used: 500, budget: 1000, utilization: 0.5, reset_at: FUTURE })]
      })
    )

    const unmapped = wrapper.get('[data-testid="mirasim-unmapped-7d_oi"]')
    expect(unmapped.text()).toContain('未映射窗口 7d_oi')
    expect(unmapped.text()).toContain('剩余 50%')
    // 7d_fable 槽位仍然是「没读数」，不能被 7d_oi 顶上。
    expect(wrapper.get('[data-testid="mirasim-window-7d_fable-empty"]').text()).toContain('—')
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
  })

  it('探测失败带过来的旧读数会被标成可能过期', () => {
    const wrapper = mountCell(
      snap({
        source: 'limits',
        status: 'failed',
        observed_at: '2026-09-16T10:00:00Z',
        windows: [win('5h', { used: 10, budget: 100, utilization: 0.1, reset_at: FUTURE })]
      })
    )

    expect(wrapper.get('[data-testid="mirasim-quota-stale"]').text()).toContain('过期')
  })

  it('探测失败且一条读数都没有时，除了「未探测」还说清探测失败过', () => {
    const wrapper = mountCell(snap({ status: 'failed' }))

    expect(wrapper.get('[data-testid="mirasim-unprobed"]').text()).toContain('未探测')
    expect(wrapper.get('[data-testid="mirasim-quota-probe-failed"]').text()).toContain('失败')
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
  })

  it('权威档位画成事实徽章，导入声明的档位另做标记', () => {
    const authoritative = mountCell(
      snap({
        source: 'limits',
        plan: 'plus',
        next_plan: 'max',
        plan_expires_at: '2026-10-08T19:20:40Z',
        plan_source: 'authoritative',
        windows: [win('5h', { used: 10, budget: 100, utilization: 0.1, reset_at: FUTURE })]
      })
    )
    expect(authoritative.get('[data-testid="mirasim-plan-badge"]').text()).toBe('PLUS')
    expect(authoritative.find('[data-testid="mirasim-plan-claimed"]').exists()).toBe(false)
    expect(authoritative.get('[data-testid="mirasim-plan-next"]').text()).toContain('MAX')
    expect(authoritative.get('[data-testid="mirasim-plan-expires"]').text()).toContain('2026')

    // claimed 是导入时的声明值，已知会虚标：不能和探回来的事实长得一样。
    const claimed = mountCell(snap({ plan: 'max', plan_source: 'claimed' }))
    const badge = claimed.get('[data-testid="mirasim-plan-badge"]')
    expect(claimed.find('[data-testid="mirasim-plan-claimed"]').exists()).toBe(true)
    expect(badge.attributes('title')).toContain('未经上游核实')
    expect(badge.classes()).not.toEqual(
      authoritative.get('[data-testid="mirasim-plan-badge"]').classes()
    )
  })

  it('Go 零值 / 空字符串时间戳不当成真的读数', () => {
    const wrapper = mountCell(
      snap({
        source: 'limits',
        observed_at: '0001-01-01T00:00:00Z',
        plan: 'plus',
        plan_expires_at: '',
        windows: [win('5h', { used: 100, budget: 100, utilization: 1, reset_at: null })]
      })
    )

    expect(wrapper.find('[data-testid="mirasim-quota-snapshot-at"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="mirasim-plan-expires"]').exists()).toBe(false)
    // 满额但重置时刻未知：进度条照画（0% 剩余），但不敢断言「已用尽」。
    expect(wrapper.findAllComponents(UsageProgressBar)[0].props('utilization')).toBe(0)
    expect(wrapper.find('[data-testid="mirasim-restriction-5h"]').exists()).toBe(false)
  })

  it('点探测按钮打 admin 端点，落回快照并向上透传', async () => {
    const next = snap({
      source: 'limits',
      status: 'ok',
      observed_at: '2026-09-16T11:00:00Z',
      windows: [win('5h', { used: 50, budget: 100, utilization: 0.5, reset_at: FUTURE })]
    })
    post.mockResolvedValueOnce({ data: { account_id: 42, probed: true, quota: next } })

    const wrapper = mountCell(snap())
    await wrapper.get('[data-testid="mirasim-quota-probe"]').trigger('click')
    await flushPromises()

    expect(post).toHaveBeenCalledWith('/admin/accounts/42/mirasim-quota')
    expect(wrapper.findAllComponents(UsageProgressBar)[0].props('utilization')).toBe(50)
    expect(wrapper.emitted<MirasimQuotaSnapshot[]>('updated')?.[0]?.[0]).toEqual(next)
    expect(wrapper.find('[data-testid="mirasim-quota-probe-note"]').exists()).toBe(false)
  })

  it('probed=false 时说清这次没走上游，不把重新读取包装成探测成功', async () => {
    const stored = snap({
      source: 'limits',
      status: 'ok',
      observed_at: '2026-09-10T11:00:00Z',
      windows: [win('5h', { used: 50, budget: 100, utilization: 0.5, reset_at: FUTURE })]
    })
    post.mockResolvedValueOnce({ data: { account_id: 42, probed: false, quota: stored } })

    const wrapper = mountCell(snap())
    await wrapper.get('[data-testid="mirasim-quota-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="mirasim-quota-probe-note"]').text()).toContain(
      '没有向上游探测'
    )
    expect(wrapper.find('[data-testid="mirasim-quota-probe-error"]').exists()).toBe(false)
  })

  it('探测失败时显示错误，不伪造读数', async () => {
    post.mockRejectedValueOnce({ message: 'proxy_unavailable' })

    const wrapper = mountCell(snap())
    await wrapper.get('[data-testid="mirasim-quota-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="mirasim-quota-probe-error"]').text()).toContain(
      'proxy_unavailable'
    )
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
    expect(wrapper.get('[data-testid="mirasim-unprobed"]').text()).toContain('未探测')
  })

  it('返回里没有 quota 时说清楚，不当成探到了', async () => {
    post.mockResolvedValueOnce({ data: { account_id: 42, probed: true, quota: null } })

    const wrapper = mountCell(snap())
    await wrapper.get('[data-testid="mirasim-quota-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="mirasim-quota-probe-error"]').text()).toContain('没有快照')
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
  })
})
