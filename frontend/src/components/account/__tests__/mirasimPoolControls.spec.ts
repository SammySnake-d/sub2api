import { describe, it, expect } from 'vitest'
import { isMirasimPoolAccount, readMirasimPoolControls, writeMirasimPoolControls } from '../mirasimPoolControls'
describe('Mirasim pool controls', () => {
  it('scopes controls and preserves unrelated state', () => {
    expect(isMirasimPoolAccount({ platform: 'anthropic', type: 'apikey', credentials: { provider: 'mirasim' } } as any)).toBe(true)
    expect(isMirasimPoolAccount({ platform: 'anthropic', type: 'apikey', credentials: {} } as any)).toBe(false)
    const original = { model_rate_limits: { family: { reset: 1 } }, provider_state: 'preserve', base_rpm: 12, max_sessions: 3, window_cost_limit: 50 }
    const values = readMirasimPoolControls(original)
    expect(values).toMatchObject({ baseRPM: 12, maxSessions: 3, windowCost: 50 })
    const saved = writeMirasimPoolControls(original, { ...values, baseRPM: 0, stickyReserve: 0 })
    expect(saved).toMatchObject({ base_rpm: 0, max_sessions: 3, window_cost_limit: 50, window_cost_sticky_reserve: 0, rpm_strategy: 'strict', provider_state: 'preserve' })
    expect(saved.model_rate_limits).toEqual(original.model_rate_limits)
    expect(original.base_rpm).toBe(12)
    expect(readMirasimPoolControls(saved).stickyReserve).toBe(0)
  })
})
