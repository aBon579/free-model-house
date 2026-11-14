import { useQuery } from '@tanstack/react-query'

export type ProviderId = 'qoder' | 'qodercn' | 'trae' | 'zcode'
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
  /** 仅 ZCode：按官方客户端形态发送推理请求。默认关闭。 */
  officialIdentity?: boolean
  credits?: Credits
  error?: string
  cooldownUntil?: number
}

export interface Provider {
  id: ProviderId
  name: string
  models: Model[]
  loginModes: string[]
}

export interface CreditState {
  providers: Provider[]
  accounts: Account[]
  autoClaim: boolean
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
  /** 仅 ZCode 账号可改；其他提供方带这个字段会被后端拒绝。 */
  officialIdentity?: boolean
  move?: 'up' | 'down'
}

export interface AccountAction {
  id: string
  action: 'refresh' | 'models' | 'credits' | 'claim' | 'test' | 'reset'
  campaignId?: string
  model?: string
  captchaVerifyParam?: string
}

export interface ActionResult {
  ok: boolean
  credits?: Credits
  results?: { id: string; name: string; claimed: boolean; alreadyClaimed: boolean; amount?: number; message: string }[]
  message?: string
}

export interface CaptchaRequest {
  id: string
  accountId: string
  provider: ProviderId
  expiresAt: CreditDate
}

export interface CaptchaConfig {
  sceneId: string
  prefix: string
  region: string
  sdkUrl: string
  requests?: { id: string; accountId: string }[]
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
  settings: (autoClaim: boolean) => request<{ ok: boolean }>('/settings', { autoClaim }),
  login: (body: { provider: ProviderId; mode?: string; accountId?: string }) => request<Login>('/login', body),
  loginStatus: (id: string, signal?: AbortSignal) => request<Login>(`/login?id=${encodeURIComponent(id)}`, undefined, signal),
  completeLogin: (id: string, callback: string) => request<unknown>('/login/complete', { id, callback }),
  cancelLogin: (id: string) => request<unknown>('/login/cancel', { id }),
  import: (accounts: ImportAccount[]) => request<{ imported: number }>('/import', { accounts }),
  export: () => request<{ format: 'fmh-credit'; version: 1; accounts: ImportAccount[] }>('/export', { confirm: true }),
  captchaConfig: (id: string, signal?: AbortSignal) => request<CaptchaConfig>(`/captcha?id=${encodeURIComponent(id)}`, undefined, signal),
  captcha: (body: { id: string; requestId?: string; captchaVerifyParam: string }) => request<{ ok?: boolean }>('/captcha', body),
  captchaRequests: (signal?: AbortSignal) => request<{ requests: CaptchaRequest[] }>('/captcha/requests', undefined, signal),
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

export function useCreditCaptchaRequests() {
  return useQuery({
    queryKey: ['credit-captcha-requests'],
    queryFn: ({ signal }) => creditApi.captchaRequests(signal),
    refetchInterval: 5_000,
    refetchIntervalInBackground: true,
    retry: false,
    gcTime: 0,
  })
}
