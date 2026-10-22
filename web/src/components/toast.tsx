import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { AlertTriangle, Check, Info } from 'lucide-react'

/**
 * 全局提示（Toast）。
 *
 * ⚠️ **为什么需要它** ——
 *
 * 之前的反馈全靠"图标变一下"（复制 → 打勾）。
 * 但那个反馈太弱：
 *   - 按钮小，图标变化几乎看不见；
 *   - 用户点了之后**不知道有没有生效**；
 *   - 探测这种**要等好几秒**的操作，界面上完全没反应。
 *
 * 所以要有 toast：
 *   - 短操作（复制）→ 弹一下"已复制"；
 *   - 长操作（探测）→ 弹"正在探测…"→ 弹"探测完成"。
 *
 * ⚠️ **位置固定在右下角** —— 不遮挡用户正在看的内容。
 */
export type ToastTone = 'ok' | 'info' | 'bad'

export interface Toast {
  id: number
  tone: ToastTone
  text: string
  /** 0 = 不自动消失。 */
  duration: number
}

interface ToastApi {
  /** 弹一个提示。 */
  toast: (text: string, tone?: ToastTone, duration?: number) => void
  /** 便捷方法。 */
  ok: (text: string) => void
  info: (text: string) => void
  bad: (text: string) => void
}

const ToastCtx = createContext<ToastApi | null>(null)

// useToast 拿提示 API。
//
// ⚠️ 没有 Provider 时**不报错** —— 返回一个空实现，
//   这样组件在测试里单独渲染也不会炸。
export function useToast(): ToastApi {
  const api = useContext(ToastCtx)
  const noop = useMemo<ToastApi>(
    () => ({
      toast: () => {},
      ok: () => {},
      info: () => {},
      bad: () => {},
    }),
    [],
  )
  return api ?? noop
}

/** ToastProvider 提供全局提示。 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([])
  const seq = useRef(0)

  const dismiss = useCallback((id: number) => {
    setItems((prev) => prev.filter((t) => t.id !== id))
  }, [])

  const toast = useCallback(
    (text: string, tone: ToastTone = 'info', duration = 2600) => {
      seq.current += 1
      const id = seq.current
      setItems((prev) => {
        // ⚠️ 最多留 4 个 —— 多了会糊住界面。
        const next = [...prev, { id, tone, text, duration }]
        return next.slice(-4)
      })
      if (duration > 0) {
        setTimeout(() => dismiss(id), duration)
      }
      return id
    },
    [dismiss],
  )

  const api = useMemo<ToastApi>(
    () => ({
      toast,
      ok: (text: string) => toast(text, 'ok'),
      info: (text: string) => toast(text, 'info'),
      bad: (text: string) => toast(text, 'bad', 4000),
    }),
    [toast],
  )

  return (
    <ToastCtx.Provider value={api}>
      {children}
      <ToastViewport items={items} onDismiss={dismiss} />
    </ToastCtx.Provider>
  )
}

/** ToastViewport 渲染提示列表。 */
function ToastViewport({
  items,
  onDismiss,
}: {
  items: Toast[]
  onDismiss: (id: number) => void
}) {
  return (
    <div
      className="pointer-events-none fixed bottom-4 right-4 z-[100] flex flex-col items-end gap-2"
      // ⚠️ 安全区 —— 全面屏手机上底部有 Home 指示条。
      style={{ paddingBottom: 'var(--safe-bottom)', paddingRight: 'var(--safe-right)' }}
      role="status"
      aria-live="polite"
    >
      {items.map((t) => (
        <button
          key={t.id}
          onClick={() => onDismiss(t.id)}
          className={
            'pointer-events-auto inline-flex max-w-[min(90vw,360px)] items-center gap-2 ' +
            'rounded-[var(--r-md)] border px-3 py-2 text-[12.5px] shadow-[var(--shadow-2)] ' +
            'animate-[fmh-toast-in_180ms_ease-out] ' +
            (t.tone === 'ok'
              ? 'border-[var(--ok-line)] bg-[var(--surface)] text-[var(--ink)]'
              : t.tone === 'bad'
                ? 'border-[var(--bad-line)] bg-[var(--surface)] text-[var(--ink)]'
                : 'border-[var(--line)] bg-[var(--surface)] text-[var(--ink)]')
          }
        >
          <span
            className={
              'inline-flex h-4 w-4 shrink-0 items-center justify-center ' +
              (t.tone === 'ok'
                ? 'text-[var(--ok)]'
                : t.tone === 'bad'
                  ? 'text-[var(--bad)]'
                  : 'text-[var(--ink-3)]')
            }
          >
            {t.tone === 'ok' ? (
              <Check size={13} />
            ) : t.tone === 'bad' ? (
              <AlertTriangle size={13} />
            ) : (
              <Info size={13} />
            )}
          </span>
          <span className="text-left leading-[18px]">{t.text}</span>
        </button>
      ))}
    </div>
  )
}

/**
 * 用一个 hook 跟踪异步操作的进行中状态 + 结束提示。
 *
 * ⚠️ 探测这类操作要跑好几秒 —— 界面上必须有反馈，
 *   否则用户会以为没点到，然后连点好几次。
 */
export function useAsyncAction(
  run: () => Promise<unknown>,
  opts: {
    /** 进行中的提示文案。 */
    pending?: string
    /** 成功后的提示文案。 */
    done?: string
    /** 失败后的提示文案。 */
    failed?: string
  } = {},
) {
  const [pending, setPending] = useState(false)
  const { toast, ok, bad } = useToast()

  const trigger = useCallback(async () => {
    if (pending) return
    setPending(true)
    if (opts.pending) toast(opts.pending, 'info', 0)
    try {
      await run()
      if (opts.done) ok(opts.done)
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      bad(opts.failed ? `${opts.failed} —— ${msg}` : msg)
    } finally {
      setPending(false)
    }
  }, [pending, run, opts.pending, opts.done, opts.failed, toast, ok, bad])

  return { pending, trigger }
}

/** 挂载时清掉所有遗留提示（切页时用）。 */
export function useClearToasts(api: ToastApi) {
  useEffect(() => {
    return () => {
      api.toast('', 'info', 0)
    }
  }, [api])
}
