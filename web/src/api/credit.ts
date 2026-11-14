import { useQuery } from '@tanstack/react-query'

export type ProviderId = 'qoder' | 'qodercn' | 'trae'
export type CreditDate = number | string

export interface Model {
  id: string
  name: string
  contextWindow: number
  vision: boolean
  reasoning: boolean
  efforts?: string[]
  multiplier?: number
  maxMode?: boolean
}

export interface Credits {
  balances: { name: string; total?: number; used?: number; remaining?: number; unit: string; expiresAt?: CreditDate }[]
  campaigns: { id: string; name: string; amount?: number; claimable: boolean; claimed: boolean; expiresAt?: CreditDate }[]
  updatedAt: CreditDate
  warning?: string
}

export interface Account {
  id: string
  provider: ProviderId
  name: string
  enabled: boolean
  hint: string
  uid?: string
  expiresAt?: CreditDate
  models: Model[]
  disabledModels: string[]
  effort: string
  maxMode: boolean
  // 每日自动领取时刻（UTC+8，"HH:MM"）；缺省或空串 = 该账号不自动领取。
  autoClaimAt?: string
  credits?: Credits
  error?: string
  cooldownUntil?: number
  // 领取失败的分档暂停，只影响自动领取；与 cooldownUntil（推理车道的闸门）无关。
  // claimCooldownUntil 为负 = 停到人工解除（重新登录 / 刷新凭据 / 重置状态）。
  claimCooldownUntil?: number
  claimCooldownKind?: ClaimCooldownKind
}

export type ClaimCooldownKind = 'credential' | 'plan' | 'server' | 'rate' | string

export interface Provider {
  id: ProviderId
  name: string
  models: Model[]
  loginModes: string[]
}

export interface CreditState {
  providers: Provider[]
  accounts: Account[]
}

export interface Login {
  id: string
  provider: ProviderId
  url: string
  state: 'pending' | 'success' | 'error' | 'cancelled'
  message?: string
  accountId?: string
  expiresAt: CreditDate
  interval: number
}

export interface AccountUpdate {
  id: string
  name?: string
  enabled?: boolean
  disabledModels?: string[]
  effort?: string
  maxMode?: boolean
  autoClaimAt?: string
  move?: 'up' | 'down'
}

export interface AccountAction {
  id: string
  action: 'refresh' | 'models' | 'credits' | 'claim' | 'test' | 'reset'
  campaignId?: string
  model?: string
}

export interface ActionResult {
  ok: boolean
  credits?: Credits
  results?: { id: string; name: string; claimed: boolean; alreadyClaimed: boolean; amount?: number; message: string }[]
  message?: string
  // 领取动作回带的分档：批量签到靠它决定要不要停止往下一个号撞。
  claimHold?: { kind?: string; untilMs?: number }
}

export interface ImportAccount {
  provider: ProviderId
  name: string
  credential: Record<string, unknown>
  enabled?: boolean
  disabledModels?: string[]
  effort?: string
  maxMode?: boolean
  [key: string]: unknown
}

export class CreditError extends Error {
  constructor(message: string, readonly status: number, readonly code?: string) {
    super(message)
    this.name = 'CreditError'
  }
}

async function request<T>(path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const response = await fetch(`/api/credit${path}`, {
    method: body === undefined ? 'GET' : 'POST',
    credentials: 'include',
    cache: 'no-store',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  })
  if (response.status === 401) {
    window.dispatchEvent(new CustomEvent('fmh:logout'))
    throw new CreditError('未登录，请重新登录管理台', 401)
  }
  let payload: unknown
  try {
    payload = await response.json()
  } catch {
    throw new CreditError(`HTTP ${response.status}：服务器没有返回有效 JSON`, response.status)
  }
  const error = payload && typeof payload === 'object' && 'error' in payload
    ? payload as { error: unknown; code?: unknown }
    : undefined
  if (!response.ok || error) {
    throw new CreditError(
      typeof error?.error === 'string' ? error.error : `请求失败（HTTP ${response.status}）`,
      response.status,
      typeof error?.code === 'string' ? error.code : undefined,
    )
  }
  return payload as T
}

export const creditApi = {
  state: (signal?: AbortSignal) => request<CreditState>('', undefined, signal),
  update: (body: AccountUpdate) => request<{ ok: boolean }>('/accounts/update', body),
  delete: (id: string) => request<{ ok: boolean }>('/accounts/delete', { id }),
  action: (body: AccountAction) => request<ActionResult>('/accounts/action', body),
  login: (body: { provider: ProviderId; mode?: string; accountId?: string }) => request<Login>('/login', body),
  loginStatus: (id: string, signal?: AbortSignal) => request<Login>(`/login?id=${encodeURIComponent(id)}`, undefined, signal),
  // TRAE 是手动那条：后端刻意**不轮询**它（轮询要 NAS 监听自己的 localhost），
  // 由用户粘贴跳转 URL 换取凭据。其余渠道靠 loginStatus 轮询，不需要这个。
  completeLogin: (id: string, callback: string) => request<unknown>('/login/complete', { id, callback }),
  cancelLogin: (id: string) => request<unknown>('/login/cancel', { id }),
  import: (accounts: ImportAccount[]) => request<{ imported: number }>('/import', { accounts }),
  export: () => request<{ format: 'fmh-credit'; version: 1; accounts: ImportAccount[] }>('/export', { confirm: true }),
}

export function useCredit() {
  return useQuery({
    queryKey: ['credit'],
    queryFn: ({ signal }) => creditApi.state(signal),
    refetchInterval: 30_000,
    retry: false,
    gcTime: 0,
  })
}
