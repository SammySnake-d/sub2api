import type { Account } from '@/types'

export interface MirasimPoolControls {
  baseRPM: number
  maxSessions: number
  idleMinutes: number
  windowCost: number
  stickyReserve: number
}

export function isMirasimPoolAccount(account: Pick<Account, 'platform' | 'type' | 'credentials'> | null | undefined): boolean {
  return account?.platform === 'anthropic' && account.type === 'apikey' &&
    String(account.credentials?.provider ?? '').trim().toLowerCase() === 'mirasim'
}

export function readMirasimPoolControls(extra: Record<string, unknown> | null | undefined): MirasimPoolControls {
  const n = (key: string, fallback = 0) => {
    const value = Number(extra?.[key] ?? fallback)
    return Number.isFinite(value) && value >= 0 ? value : fallback
  }
  return { baseRPM: n('base_rpm'), maxSessions: n('max_sessions'), idleMinutes: n('session_idle_timeout_minutes', 5),
    windowCost: n('window_cost_limit'), stickyReserve: n('window_cost_sticky_reserve', 10) }
}

// Merge only owned controls. Account/provider credentials, prompt and quota
// state are deliberately not reconstructed by this editor.
export function writeMirasimPoolControls(extra: Record<string, unknown>, value: MirasimPoolControls): Record<string, unknown> {
  const out = { ...extra }
  out.base_rpm = Math.min(10000, Math.max(0, Math.trunc(Number(value.baseRPM) || 0)))
  out.max_sessions = Math.min(1000, Math.max(0, Math.trunc(Number(value.maxSessions) || 0)))
  out.session_idle_timeout_minutes = Math.min(1440, Math.max(1, Math.trunc(Number(value.idleMinutes) || 5)))
  out.window_cost_limit = Math.max(0, Number(value.windowCost) || 0)
  out.window_cost_sticky_reserve = Math.max(0, Number(value.stickyReserve) || 0)
  // This path enforces attempts atomically. A sticky exemption would defeat it.
  out.rpm_strategy = 'strict'
  delete out.rpm_sticky_buffer
  return out
}
