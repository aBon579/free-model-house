import { useEffect, useState } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { KeyRound, Lock } from 'lucide-react'
import { Shell, type RouteKey } from './components/Shell'
import { BrandMark } from './components/Brand'
import {
  Button,
  Card,
  CardBody,
  CardHeader,
  ErrorNote,
  Field,
  Notice,
  PasswordInput,
} from './components/ui'
import { ToastProvider } from './components/toast'
import { useChangePassword, useLogin, useLogout, useStatus } from './api/client'
import { QuickStart } from './pages/QuickStart'
import { Models } from './pages/Models'
import { Usage } from './pages/Usage'
import { Sources } from './pages/Sources'
import { Settings } from './pages/Settings'

/** 路由表。 */
const routes: RouteKey[] = ['quickstart', 'models', 'usage', 'sources', 'settings']

/** 密码最少几位（必须与后端一致）。 */
const MIN_PASSWORD_LEN = 8

/** 从 hash 读当前路由。 */
function readRoute(): RouteKey {
  const hash = window.location.hash.replace(/^#\/?/, '')
  return (routes as string[]).includes(hash) ? (hash as RouteKey) : 'quickstart'
}

/**
 * 这个错误是不是"没登录"（401）。
 *
 * ⚠️ "没登录"是**预期状态**，不是故障 ——
 *   不该显示成红色错误框。
 */
function isUnauthorized(err: unknown): boolean {
  if (!err || typeof err !== 'object') return false
  const status = (err as { status?: number }).status
  if (status === 401) return true
  // ⚠️ `ApiError` 有 `status`；但万一序列化丢了，退回看消息。
  const msg = (err as Error).message ?? ''
  return msg.includes('未登录')
}

/**
 * 「刚改过密码」的标记 —— 存在 `sessionStorage`。
 *
 * ⚠️ **为什么要存到 sessionStorage** ——
 *
 * 改密码 → 广播 → 回登录页，这个过程里 `LoginGate` **会重挂载**
 * （因为 `signedOut` 变了，父组件重新渲染）。
 * 内存里的 `useState` 会丢，所以这个标记必须活过重挂载。
 *
 * ⚠️ **为什么用 sessionStorage 而不是 localStorage** ——
 * localStorage 会**永久**留着 —— 明天你打开管理台，
 * 还会看到"请用最新密码登录"，那是错的。
 * sessionStorage 只活到关标签页，正好。
 */
const PASSWORD_CHANGED_KEY = 'fmh:password-changed'

function readPasswordChanged(): boolean {
  try {
    return sessionStorage.getItem(PASSWORD_CHANGED_KEY) === '1'
  } catch {
    // ⚠️ 隐私模式下 sessionStorage 可能抛错 —— 不影响主流程。
    return false
  }
}

function writePasswordChanged(v: boolean): void {
  try {
    if (v) {
      sessionStorage.setItem(PASSWORD_CHANGED_KEY, '1')
    } else {
      sessionStorage.removeItem(PASSWORD_CHANGED_KEY)
    }
  } catch {
    // 同上 —— 存不了就算了，提示少一次不影响能不能登录。
  }
}

/** 应用。 */
function App() {
  const [route, setRoute] = useState<RouteKey>(readRoute)
  const logout = useLogout()

  useEffect(() => {
    const onHash = () => setRoute(readRoute())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  function navigate(r: RouteKey) {
    window.location.hash = `/${r}`
    setRoute(r)
  }

  return (
    <Shell route={route} onNavigate={navigate} onLogout={() => logout.mutate()}>
      {route === 'quickstart' ? <QuickStart /> : null}
      {route === 'models' ? <Models /> : null}
      {route === 'usage' ? <Usage /> : null}
      {route === 'sources' ? <Sources /> : null}
      {route === 'settings' ? <Settings /> : null}
    </Shell>
  )
}

/**
 * 登录页 —— 未登录时显示。
 *
 * ⚠️ `signedOut` = **显式登出**（改完密码 / 点了退出登录）。
 *   这种情况**立刻**显示登录页，不等 `useStatus` 的网络往返。
 */
function LoginGate({
  children,
  signedOut,
}: {
  children: React.ReactNode
  signedOut?: boolean
}) {
  const { data, isLoading, error } = useStatus()
  const login = useLogin()
  const [password, setPassword] = useState('')

  // ⚠️ **刚改过密码** —— 登录页要提示"用最新的密码"。
  //
  //   用 `sessionStorage` 而不是内存 state：
  //   改密码 → 广播登出 → 回到登录页，这中间**组件会重挂载**，
  //   内存里的 state 就丢了。
  //
  //   sessionStorage 只活到关标签页 —— 正好覆盖
  //   "改完马上重登"这个场景，不会残留到下次。
  const [passwordChanged, setPasswordChanged] = useState(() =>
    readPasswordChanged(),
  )

  useEffect(() => {
    const onChanged = () => {
      // ⚠️ **只有"改密码"才提示** ——
      //   主动点退出登录不需要提示"用最新密码"。
      setPasswordChanged(true)
      writePasswordChanged(true)
    }
    window.addEventListener('fmh:password-changed', onChanged)
    return () => window.removeEventListener('fmh:password-changed', onChanged)
  }, [])

  // ⚠️ **登录成功** → 清掉标记（下次别再提示）。
  useEffect(() => {
    if (data && !signedOut) {
      setPasswordChanged(false)
      writePasswordChanged(false)
    }
  }, [data, signedOut])

  // ⚠️ **地址栏要跟着登录状态走** ——
  //
  //   登录页的 URL 是 `#/login`，登进去之后还留着它：
  //   刷新一下会又弹回登录页，看着像"没登上"。
  //
  //   所以：登录成功 → 清掉 `#/login`；登出 → 写回 `#/login`。
  useEffect(() => {
    const hash = window.location.hash
    if (signedOut) {
      if (hash !== '#/login') window.location.hash = '/login'
    } else if (data && hash === '#/login') {
      window.location.hash = '/quickstart'
    }
  }, [signedOut, data])

  // 已登录 → 显示应用。
  //
  // ⚠️ **`signedOut` 优先** —— 它是明确的"登出"信号，
  //   不用等查询往返。
  if (!signedOut && !isLoading && data) {
    return (
      <>
        {/* ⚠️ **还在用初始密码 → 强制改** ——
            这个弹窗**不能关** —— 必须改完才能用。
            改完之后后端会作废所有会话，所以会自动回到登录页。 */}
        {data.mustChangePassword ? <MustChangePassword /> : null}
        {children}
      </>
    )
  }

  return (
    <div className="flex min-h-full items-center justify-center bg-[var(--bg)] px-4">
      <div className="w-full max-w-sm">
        <div className="mb-5 flex items-center justify-center gap-2.5">
          <BrandMark size="md" />
          <div>
            <div className="text-[15px] font-semibold text-[var(--ink)]">
              Free Model House
            </div>
            <div className="text-[12px] text-[var(--ink-3)]">请输入管理密码</div>
          </div>
        </div>

        <Card>
          <CardBody className="space-y-3">
            {/* ⚠️ **"未登录"不是错误** ——
                那是"当然没登录"的状态，不是出了故障。
                显示成红框会让人以为系统坏了。

                只显示**真正的**错误（网络挂了、服务器 500）。 */}
            {error && !isUnauthorized(error) ? (
              <ErrorNote>{(error as Error).message}</ErrorNote>
            ) : null}

            <PasswordInput
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && password) {
                  login.mutate(password)
                }
              }}
              placeholder="管理密码"
              autoFocus
              autoComplete="current-password"
              aria-label="管理密码"
            />
            <Button
              variant="primary"
              className="w-full"
              disabled={!password || login.isPending}
              onClick={() => login.mutate(password)}
            >
              <KeyRound size={14} />
              {login.isPending ? '登录中…' : '登录'}
            </Button>

            {login.isError ? (
              <ErrorNote>{(login.error as Error).message}</ErrorNote>
            ) : null}
          </CardBody>
        </Card>

        <div className="mt-3">
          {/* ⚠️ **刚改过密码？** ——
              改完密码后所有会话都作废了，必须用**新密码**重新登录。
              而用户很容易下意识地再输一遍**旧密码** ——
              那样只会看到"密码不对"，完全不知道发生了什么。

              所以明确提示："用最新的密码登录"。

              ⚠️ **用 sessionStorage 而不是内存** ——
              改密码 → 回登录页是一次**重新挂载**，
              内存里的 state 会丢。sessionStorage 只活到关标签页，
              正好覆盖"改完马上重登"这个场景。 */}
          {passwordChanged ? (
            <Notice tone="accent">
              <div className="text-[12px]">
                密码已修改 —— 请用
                <b>最新的密码</b>
                登录。
              </div>
            </Notice>
          ) : (
            <Notice tone="accent">
              <div className="text-[12px]">
                首次部署？初始密码是 <b className="mono">admin</b>，
                登录后会要求你立刻改掉。
              </div>
            </Notice>
          )}
        </div>
      </div>
    </div>
  )
}

/**
 * 强制改密码的弹窗。
 *
 * ⚠️ **不能关** —— 用户还在用初始密码 `admin`，
 *   那是**公开的默认值**，谁都能登进来。
 */
function MustChangePassword() {
  const change = useChangePassword()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')

  const tooShort = next.length > 0 && next.length < MIN_PASSWORD_LEN
  const mismatch = confirm.length > 0 && next !== confirm
  const canSubmit =
    current.length > 0 && next.length >= MIN_PASSWORD_LEN && next === confirm && !change.isPending

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/45 px-4">
      <div className="w-full max-w-sm">
        <Card>
          <CardHeader title="设置新密码" />
          <CardBody className="space-y-3">
            <Notice tone="warn">
              <div className="text-[12px]">
                你还在用初始密码 <b className="mono">admin</b> ——
                那是公开的默认值，<b>必须改掉</b>才能继续用。
              </div>
            </Notice>

            {change.isError ? (
              <ErrorNote>{(change.error as Error).message}</ErrorNote>
            ) : null}

            <Field label="当前密码">
              <PasswordInput
                value={current}
                onChange={(e) => setCurrent(e.target.value)}
                autoComplete="current-password"
                aria-label="当前密码"
              />
            </Field>

            <Field
              label="新密码"
              hint={`至少 ${MIN_PASSWORD_LEN} 位`}
              error={tooShort ? `还差 ${MIN_PASSWORD_LEN - next.length} 位` : ''}
            >
              <PasswordInput
                value={next}
                onChange={(e) => setNext(e.target.value)}
                autoComplete="new-password"
                aria-label="新密码"
              />
            </Field>

            <Field label="确认新密码" error={mismatch ? '两次输入不一致' : ''}>
              <PasswordInput
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && canSubmit) {
                    change.mutate({ current, new: next, confirm })
                  }
                }}
                autoComplete="new-password"
                aria-label="确认新密码"
              />
            </Field>

            <Button
              variant="primary"
              className="w-full"
              disabled={!canSubmit}
              onClick={() => change.mutate({ current, new: next, confirm })}
            >
              <Lock size={14} />
              {change.isPending ? '提交中…' : '修改密码'}
            </Button>

            <div className="text-[11.5px] leading-[18px] text-[var(--ink-3)]">
              改完会**退出所有管理会话** —— 请用新密码重新登录。
            </div>
          </CardBody>
        </Card>
      </div>
    </div>
  )
}

/**
 * 根组件。
 *
 * ⚠️ **`signedOut` 是"显式登出"标记** ——
 *
 *   改完密码后后端会作废所有会话，前端要**立刻**回登录页。
 *
 *   之前是"等 `useStatus` 重新请求、拿到 401、再判断没登录" ——
 *   那个过程要走**一个网络往返**，中间还会：
 *     - 闪一下旧界面
 *     - 把 401 当成错误显示出来
 *     - 因为 React Query 的重试机制再等几秒
 *
 *   症状就是"改完密码后卡住好几秒，还很诡异"。
 *
 *   现在：**改完密码立刻置 `signedOut`**，不等任何网络请求。
 */
export default function Root() {
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            // ⚠️ 管理面的请求**不消耗上游额度**，
            // 所以重试是安全的。
            //
            // ⚠️ 但 **401 不能重试** —— 会话没了就是没了，
            //   重试只会让用户白等（详见 `api/client.ts` 的 `isRetryable`）。
            retry: (failureCount, error) => {
              if (error && typeof error === 'object' && 'status' in error) {
                const s = (error as { status: number }).status
                if (s === 401 || s === 403) return false
                if (s >= 400 && s < 500) return false
              }
              return failureCount < 1
            },
            refetchOnWindowFocus: false,
            staleTime: 5_000,
          },
        },
      }),
  )

  // ⚠️ **显式登出标记** —— 见上面的说明。
  const [signedOut, setSignedOut] = useState(false)

  // ⚠️ 监听"登出 / 登录"广播 ——
  //   改完密码或点了退出登录时，**立刻**切换界面，不等网络往返。
  useEffect(() => {
    const onLogout = () => setSignedOut(true)
    const onLogin = () => setSignedOut(false)
    window.addEventListener('fmh:logout', onLogout)
    window.addEventListener('fmh:login', onLogin)
    return () => {
      window.removeEventListener('fmh:logout', onLogout)
      window.removeEventListener('fmh:login', onLogin)
    }
  }, [])

  return (
    <QueryClientProvider client={client}>
      <ToastProvider>
        <LoginGate signedOut={signedOut}>
          <App />
        </LoginGate>
      </ToastProvider>
    </QueryClientProvider>
  )
}
