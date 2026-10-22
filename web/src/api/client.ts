import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  KeysResponse,
  ModelInfo,
  ProbeResult,
  QuotaInfo,
  SecretInfo,
  Settings,
  StatusResponse,
  UsageResponse,
} from './types'

/**
 * API 客户端。
 *
 * ⚠️ **所有请求都带 `credentials: 'include'`** ——
 * 管理面的鉴权靠 HttpOnly Cookie，缺了它一切都 401。
 */

class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

/**
 * 这个错误**不该重试**。
 *
 * ⚠️ **为什么 401 不重试** ——
 *
 * React Query 默认 `retry: 3`，带指数退避（1s → 2s → 4s）。
 *
 * 401 是"**你的会话没了**"，不是"网络抖了" ——
 * 再试三次也是 401，只是白白让用户等 7 秒。
 *
 * ⚠️ 这个症状当时表现为"改完密码后界面卡住好几秒" ——
 *   实际上是在**重试三个注定失败的请求**。
 *
 * ⚠️ 429 和 5xx **要**重试 —— 那些是"等一下会好"。
 */
function isRetryable(err: unknown): boolean {
  if (err instanceof ApiError) {
    // 401 未登录 / 403 无权限 —— 重试没意义。
    if (err.status === 401 || err.status === 403) return false
    // 404 —— 端点不存在，重试没意义。
    if (err.status === 404) return false
    // 400 / 409 / 422 —— 请求本身有问题，重试没意义。
    if (err.status >= 400 && err.status < 500) return false
  }
  return true
}

/** 发一个请求。 */
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(init?.headers ?? {}),
    },
  })

  const text = await res.text()
  let body: unknown = null
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = text
    }
  }

  if (!res.ok) {
    // ⚠️ 后端的错误形状有两种（OpenAI 风格 / 简单风格），都兼容。
    let message = `请求失败（${res.status}）`
    if (body && typeof body === 'object') {
      const b = body as Record<string, unknown>
      if (typeof b.error === 'string') {
        message = b.error
      } else if (b.error && typeof b.error === 'object') {
        const e = b.error as Record<string, unknown>
        if (typeof e.message === 'string') message = e.message
      } else if (typeof b.hint === 'string' && typeof b.error === 'string') {
        message = `${b.error} —— ${b.hint}`
      }
    }
    throw new ApiError(message, res.status)
  }

  return body as T
}

function get<T>(path: string): Promise<T> {
  return request<T>(path)
}

function post<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: 'POST',
    body: body === undefined ? undefined : JSON.stringify(body),
  })
}

// 查询

/** 总体状态。 */
export function useStatus() {
  return useQuery({
    queryKey: ['status'],
    queryFn: () => get<StatusResponse>('/api/status'),
    // ⚠️ 状态轮询要**便宜** —— 它不消耗任何上游额度。
    refetchInterval: 15_000,
    // ⚠️ **401 不重试** —— 会话没了就是没了，
    //   重试只是让用户白等（详见 `isRetryable`）。
    retry: isRetryable,
  })
}

/** 模型清单。 */
export function useModels() {
  return useQuery({
    queryKey: ['models'],
    queryFn: () => get<{ models: ModelInfo[] }>('/api/models'),
    refetchInterval: 30_000,
    retry: isRetryable,
  })
}

/**
 * Key。
 *
 * ⚠️ 默认**不 reveal** —— 明文只在用户显式点"显示"时才取。
 *
 * ⚠️ **不要用 `gcTime: 0`** ——
 *   那会让 reveal 的 query 在**每次渲染**时被回收并立刻重新请求，
 *   于是 `data` 永远是 `undefined`，界面上的"显示完整 Key"点了没反应。
 *   （实测踩过：请求发了、200、响应体里有完整 Key，但 DOM 还是指纹。）
 *
 *   正确做法：reveal 的 query 用**较短的 gcTime**（1 分钟就够），
 *   `staleTime: 0` 保证点"显示"时一定重新取。
 */
export function useKeys(reveal = false) {
  return useQuery({
    queryKey: ['keys', reveal],
    queryFn: () => get<KeysResponse>(`/api/keys${reveal ? '?reveal=1' : ''}`),
    // ⚠️ reveal=true 时 staleTime 必须是 0 —— 点"显示"要立刻取明文。
    staleTime: reveal ? 0 : Infinity,
    // ⚠️ 不能是 0 —— 见上面的说明。
    //    1 分钟：明文在内存里最多停留 1 分钟，且下次 reveal 会重新取。
    gcTime: reveal ? 60_000 : 5 * 60_000,
  })
}

/** 上游凭据列表。 */
export function useSecrets() {
  return useQuery({
    queryKey: ['secrets'],
    queryFn: () => get<{ secrets: SecretInfo[] }>('/api/secrets'),
  })
}

/** 用量。 */
export function useUsage() {
  return useQuery({
    queryKey: ['usage'],
    queryFn: () => get<UsageResponse>('/api/usage'),
    refetchInterval: 30_000,
  })
}

/** 探测结论。 */
export function useProbes() {
  return useQuery({
    queryKey: ['probes'],
    queryFn: () =>
      get<{
        probes: ProbeResult[]
        summary: { probed: number; available: number; failed: number; unknown: number }
        progress?: ProbeProgress
      }>('/api/probes'),
    // ⚠️ **正在探测时轮询快一点** ——
    //   否则进度条会一跳一大格，看着像卡住了。
    //
    //   ⚠️ **1 秒**（不是 2 秒）——
    //   探测一轮可能 20~60 秒，1 秒的粒度让进度看着是**连续的**，
    //   而且"探完了"最多晚 1 秒就被发现。
    //
    //   平时 30 秒一次就够（状态没在变，轮询快也没意义）。
    refetchInterval: (q) => (q.state.data?.progress?.running ? 1_000 : 30_000),
  })
}

/** 手动探测一个模型。 */
export function useProbeOne() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (model: string) =>
      post<{ ok: boolean; result: ProbeResult }>('/api/probes/run', { model }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['models'] })
      qc.invalidateQueries({ queryKey: ['probes'] })
    },
  })
}

/** 探测进度。 */
export interface ProbeProgress {
  /** 是否正在跑一轮。 */
  running: boolean
  /** 主体探完了，正在收尾（复核限流 / 落结论）。 */
  finalizing: boolean
  /** 本轮的模型总数。 */
  total: number
  /** 已探完的数量。 */
  done: number
  /** **已跑完**的轮次编号 —— 每跑完一轮就 +1。 */
  round: number
  /** 上轮结束的时间（epoch 毫秒）。 */
  lastRunAt?: number
  /**
   * 上轮的**共性失败原因**。
   *
   * ⚠️ 「探测完成但全是未探测」要说清为什么 ——
   *   绝大多数情况是所有模型因同一个原因失败。
   */
  lastFailure?: string
  /** 那个原因占了多少个模型。 */
  lastFailureCount?: number
  /**
   * 上轮探通了几个。
   *
   * ⚠️ 只说"8 个失败"会让人以为全挂了 ——
   *   真实情况可能是"16 个可用、8 个限流"。
   */
  lastAvailable?: number
}

/**
 * 触发一整轮探测。
 *
 * ⚠️ **这是异步的** —— 后端立刻返回 202，探测在后台跑（可能几十秒）。
 *   所以 `onSuccess` 只表示"**开始了**"，不表示"**完成了**"。
 *
 *   前端要靠 `useProbes()` 里的 `progress` 判断真的完成了。
 */
export function useProbeAll() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => post<{ ok: boolean; note: string; async: boolean }>('/api/probes/run'),
    onSuccess: () => {
      // ⚠️ 立刻刷新一次 —— 让 `progress.running` 变成 true。
      qc.invalidateQueries({ queryKey: ['probes'] })
    },
  })
}

/** 设置。 */
export function useSettings() {
  return useQuery({
    queryKey: ['settings'],
    queryFn: () => get<Settings>('/api/settings'),
  })
}

/** 车道详情（含额度）。 */
export function useLanes() {
  return useQuery({
    queryKey: ['lanes'],
    queryFn: () => get<{
      lanes: Array<{
        id: string
        name: string
        ready: boolean
        modelCount: number
        quota?: QuotaInfo
      }>
    }>('/api/lanes'),
    refetchInterval: 30_000,
  })
}

/**
 * 开/关一条车道。
 *
 * ⚠️ 关掉的车道的模型**不进候选链**（但仍然显示在清单里）。
 */
export function useLaneSwitch() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { lane: string; enabled: boolean }) =>
      post<{ ok: boolean; lanes: Record<string, boolean> }>('/api/lanes/switch', body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['status'] })
      qc.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

// 变更

/** 轮换一把 Key。 */
export function useRotateKey() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (which: 'local' | 'lan') =>
      post<{ ok: boolean; value: string; hint: string; note: string }>(
        '/api/keys/rotate',
        { which },
      ),
    onSuccess: () => {
      // ⚠️ 轮换后所有 Key 相关的缓存都失效 ——
      // 特别是 reveal 的明文。
      qc.invalidateQueries({ queryKey: ['keys'] })
      qc.invalidateQueries({ queryKey: ['status'] })
    },
  })
}

/** 保存一个上游凭据。 */
export function useSaveSecret() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { name: string; value: string }) =>
      post<{ ok: boolean; hint?: string; deleted?: boolean }>('/api/secrets', body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['secrets'] })
      qc.invalidateQueries({ queryKey: ['lanes'] })
      qc.invalidateQueries({ queryKey: ['status'] })
    },
  })
}

/** 保存设置。 */
export function useSaveSettings() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: Partial<Settings>) => post<{ ok: boolean }>('/api/settings', body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['settings'] })
      qc.invalidateQueries({ queryKey: ['status'] })
    },
  })
}

/**
 * 登录。
 *
 * ⚠️ **登录成功后立刻把 status 的缓存种进去** ——
 *
 * 之前登录后要**等 `/api/status` 往返**才渲染应用：
 *
 *	点登录 → /api/login (285ms，bcrypt) → invalidateQueries()
 *	→ /api/status (+220ms) → 才渲染
 *
 * 用户看到的是"点了登录，**卡住不动**，然后突然进应用"。
 *
 * 根本原因：`useStatus` 在登录前是 **401**（没登录），
 * 那个**失败的 query 也被缓存**了 —— `invalidateQueries`
 * 虽然会重取，但那是**一个完整的网络往返**。
 *
 * 现在：登录接口返回的 `mustChangePassword` **已经够用了**，
 * 直接把它写进 `['status']` 缓存 → **零延迟**渲染应用。
 * 真正的 status 数据随后在后台刷新（`invalidateQueries` 照旧）。
 */
export function useLogin() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (password: string) =>
      post<{ ok: boolean; mustChangePassword: boolean }>('/api/login', { password }),
    onSuccess: (res) => {
      // ⚠️ **先种缓存** —— 让界面**立刻**切走，不等网络往返。
      //
      //   ⚠️ **`lanes` 必须给空数组** ——
      //   `QuickStart` 里是 `status?.lanes.map(...)`，
      //   `Sources` 里是 `status?.lanes.find(...)` ——
      //   少了 `?? []` 会直接抛 "Cannot read properties of undefined"。
      qc.setQueryData(['status'], (old: StatusResponse | undefined) => ({
        ...(old ?? ({} as StatusResponse)),
        lanes: old?.lanes ?? [],
        mustChangePassword: res.mustChangePassword,
      }))
      // 登录后所有东西都要重新拿。
      qc.invalidateQueries()
      // ⚠️ **广播"已登录"** —— 让界面立刻从登录页切走，
      //   不等 `useStatus` 的轮询往返。
      window.dispatchEvent(new CustomEvent('fmh:login'))
    },
  })
}

/** 修改管理台密码。 */
export function useChangePassword() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { current: string; new: string; confirm: string }) =>
      post<{ ok: boolean; note: string }>('/api/change-password', body),
    onSuccess: () => {
      // ⚠️ 改完密码**所有会话都作废**了 —— 清空本地缓存。
      //
      // ⚠️ **再广播一次"登出"** ——
      //   清缓存会让 React Query 重新发请求，
      //   那些请求会带**已经失效的 Cookie** → 401 →
      //   界面会闪一下、还会等重试。
      //
      //   广播 `fmh:logout` 让界面**立刻**切到登录页，
      //   不等任何网络往返。
      qc.clear()
      window.dispatchEvent(new CustomEvent('fmh:logout'))
      // ⚠️ **单独再广播一次"密码改了"** ——
      //   登录页要提示"请用最新的密码登录"。
      //   跟普通的退出登录区分开（那个不用提示）。
      window.dispatchEvent(new CustomEvent('fmh:password-changed'))
    },
  })
}

/** 注销。 */
export function useLogout() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => post<{ ok: boolean }>('/api/logout'),
    onSuccess: () => {
      qc.clear()
      // ⚠️ **广播"已登出"** —— 界面立刻回登录页。
      window.dispatchEvent(new CustomEvent('fmh:logout'))
    },
  })
}
