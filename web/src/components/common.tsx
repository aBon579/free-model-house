import { useState } from 'react'
import { Check, Copy, Eye, EyeOff, X } from 'lucide-react'
import { Button, cn } from './ui'
import { copyToClipboard } from '../lib/clipboard'
import { useToast } from './toast'
import type { LaneID, ModelState } from '../api/types'

/**
 * 格式化美元金额。
 *
 * ⚠️ **不能固定两位小数** —— 一次免费调用可能是 $0.0000043，
 * 显示成 "$0.00" 会让人以为"完全没花钱"，而额度在**悄悄消耗**。
 * 但也不能太长，所以按量级递减。
 */
export function formatCost(usd: number): string {
  if (usd === 0) return '$0'
  if (usd < 0.0001) return `$${usd.toFixed(7)}`
  if (usd < 0.01) return `$${usd.toFixed(5)}`
  if (usd < 1) return `$${usd.toFixed(4)}`
  return `$${usd.toFixed(2)}`
}

/**
 * 车道徽章。
 *
 * ⚠️ 这是 FMH 独有的概念 —— 三条上游源性质完全不同，
 * 用户必须一眼看出区别：
 *
 *   zen（免密）—— 不要凭据，高峰期可能被限流。
 *   Kilo —— 也不要凭据，标准 OpenAI 兼容。
 *   OpenRouter —— 用你自己的 Key，额度绑定账户、**按天算**。
 */
export function LaneBadge({ lane }: { lane: LaneID; full?: boolean }) {
  const style = laneStyle(lane)
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1 rounded-full border px-2 py-[1px]',
        'text-[11.5px] font-medium leading-[17px] whitespace-nowrap',
        style.badge,
      )}
      title={style.title}
    >
      <span className={cn('h-1.5 w-1.5 rounded-full', style.dot)} />
      {style.label}
    </span>
  )
}

/** laneStyle 给一条车道返回配色和文案。 */
function laneStyle(lane: LaneID): { badge: string; dot: string; label: string; title: string } {
  switch (lane) {
    case 'kilo':
      return {
        badge: 'border-[var(--lane-kilo-line)] bg-[var(--lane-kilo-soft)] text-[var(--lane-kilo)]',
        dot: 'bg-[var(--lane-kilo)]',
        label: 'Kilo',
        title: 'Kilo 免 key 池：不需要任何凭据，标准 OpenAI 兼容',
      }
    case 'openrouter':
      return {
        badge:
          'border-[var(--lane-openrouter-line)] bg-[var(--lane-openrouter-soft)] text-[var(--lane-openrouter)]',
        dot: 'bg-[var(--lane-openrouter)]',
        label: 'OR',
        title: 'OpenRouter 免费模型：用你自己的 Key（免费额度按天算）',
      }
    default:
      return {
        badge: 'border-[var(--lane-zen-line)] bg-[var(--lane-zen-soft)] text-[var(--lane-zen)]',
        dot: 'bg-[var(--lane-zen)]',
        // ⚠️ **用 `zen` 而不是"免密"** ——
        //   模型清单里三个标签并排（zen / Kilo / OR），
        //   "免密"两个字占的宽度是 "zen" 的三倍，
        //   三个标签的**宽度就不齐**了，看着很乱。
        //
        //   而且"zen"是**官方叫法**（OpenCode Zen），用户搜得到。
        label: 'zen',
        title: 'OpenCode Zen 免密车道：不需要凭据',
      }
  }
}

// 状态点
//
// ⚠️ **文案原则** —— 好处是"状态 = 该做什么"：
//
//	可用            → 直接用
//	未探测          → 探一下才知道
//	暂时限流·可重试 → 等一会就好（**不是**"限流"，那个词会让人以为坏了）
//	地区受限        → 换出口
//	出口被风控      → 换出口（比"不可用"准确 —— 它是**暂时**的）
//	不可用          → 别试了
//
// ⚠️ **排序也按这个思路**：
//   可用 < 未探测 < 限流 < 地区受限 < 不可用
//   —— 限流在"地区受限"**前面**，因为限流是**暂时**的，
//   而地区受限在这次会话里基本没救。
//
// ⚠️ 状态值必须与后端**逐字一致**（连字符，不是下划线）。
const stateMeta: Record<
  ModelState,
  { label: string; color: string; bg: string; pulse: boolean; hint: string }
> = {
  available: {
    label: '可用',
    color: 'var(--ok)',
    bg: 'var(--ok-soft)',
    pulse: false,
    hint: '探测通过，可以用',
  },
  unknown: {
    label: '未探测',
    color: 'var(--idle)',
    bg: 'var(--idle-soft)',
    pulse: false,
    hint: '还没探过，或结论已过期',
  },
  throttled: {
    label: '暂时限流·可重试',
    color: 'var(--warn)',
    bg: 'var(--warn-soft)',
    pulse: false,
    hint: '被限流了 —— 等一会就好，路由会自动换模型',
  },
  'region-blocked': {
    label: '地区受限',
    color: 'var(--warn)',
    bg: 'var(--warn-soft)',
    pulse: false,
    hint: '这个出口所在地区被上游屏蔽 —— 换出口才有用',
  },
  'abuse-blocked': {
    label: '出口被风控',
    color: 'var(--bad)',
    bg: 'var(--bad-soft)',
    pulse: false,
    hint: '上游认为这个出口 IP 有滥用行为 —— 整条车道都不可用，换出口才有用',
  },
  unavailable: {
    label: '不可用',
    color: 'var(--bad)',
    bg: 'var(--bad-soft)',
    pulse: false,
    hint: '上游明确拒绝（模型不存在 / 凭据不适用）—— 别试了',
  },
}

/** 状态排序 —— 按 STATE_ORDER。 */
const stateOrder: Record<ModelState, number> = {
  available: 0,
  unknown: 1,
  throttled: 2,
  'region-blocked': 3,
  'abuse-blocked': 4,
  unavailable: 5,
}

/** 状态排序键。 */
export function stateRank(state: ModelState): number {
  return stateOrder[state] ?? 1
}

/**
 * 状态文字（不带底色，用于表格内）。
 */
export function StateText({
  state,
  detail,
}: {
  state: ModelState
  detail?: string
}) {
  const meta = stateMeta[state] ?? stateMeta.unknown
  return (
    <span
      className="inline-flex items-center gap-1.5 text-[12.5px] whitespace-nowrap"
      style={{ color: meta.color }}
      title={detail ? `${meta.label} —— ${detail}` : meta.hint}
    >
      <span className="h-1.5 w-1.5 rounded-full" style={{ background: meta.color }} />
      {meta.label}
    </span>
  )
}

/**
 * 状态徽章（带底色的药丸）—— 卡片式布局用。
 *
 * ⚠️ 文案原则：状态要**能指导下一步动作**。
 *   "暂时限流·可重试" 比 "限流" 有用得多。
 */
export function StateBadge({
  state,
  detail,
  className,
}: {
  state: ModelState
  detail?: string
  className?: string
}) {
  const meta = stateMeta[state] ?? stateMeta.unknown
  return (
    <span
      className={cn(
        'inline-flex shrink-0 items-center rounded-full px-2 py-[2px] text-[11.5px] whitespace-nowrap',
        className,
      )}
      style={{ color: meta.color, background: meta.bg }}
      title={detail ? `${meta.label} —— ${detail}` : meta.hint}
    >
      {meta.label}
    </span>
  )
}

// CopyField
/**
 * 带复制（可选打码）的字段。
 *
 * ⚠️ **默认打码** —— 明文只在用户显式点"显示"时出现。
 * 理由：管理台会被投屏、截图、录屏。
 *
 * ⚠️ **`revealed` 是受控属性** ——
 *   组件内部也有一个 `revealed` state（点眼睛按钮切换），
 *   但如果外部传了 `revealed`，就**以外部为准**。
 *
 *   ⚠️ 为什么需要这个：
 *   「快速接入」页的 Key 来自 `useKeys(revealed)` ——
 *   要显示完整 Key 就得**重新请求**后端（`?reveal=1`），
 *   所以显示状态必须由页面持有，不能只存在组件内部。
 *
 *   没有受控模式时，页面拿到的完整 Key 会被组件的 `masked` 打回指纹 ——
 *   表现就是「点了显示，界面没变」。
 */
export function CopyField({
  value,
  masked = true,
  allowReveal = true,
  mono = true,
  className,
  revealed: revealedProp,
  getFullValue,
}: {
  value: string
  /** 初始是否打码。 */
  masked?: boolean
  /** 是否允许"显示"按钮。 */
  allowReveal?: boolean
  mono?: boolean
  className?: string
  /** 受控的显示状态 —— 传了就以外部为准。 */
  revealed?: boolean
  /**
   * 取**完整值**的异步函数 —— 打码状态下点"复制"时调用。
   *
   * ⚠️ **为什么需要它** ——
   *
   * `value` 在打码时是**指纹**（`fmh-3f9a…abcd`），
   * 直接复制会把指纹抄进剪贴板 —— 那不是 Key，调用方拿去用必然失败。
   *
   * 传了 `getFullValue`，复制按钮会**先取完整值再复制**：
   *   - 界面上仍然只有指纹（防偷窥）
   *   - 剪贴板里是完整的 Key（能用）
   *
   * ⚠️ **与 `revealed` 的判定解耦** ——
   *   之前靠"当前是否显示完整"来决定复制什么，
   *   但受控模式下 `value === display` 恒等（页面传什么显示什么），
   *   打码时也判成"在显示完整值" → 复制出去就是指纹。
   *   现在只看 `revealed`（外部明确告知）+ `masked`。
   *
   * ⚠️ 没传时退化为复制 `value` 本身（对非敏感字段——
   *   base URL、模型名—— 这就是对的）。
   */
  getFullValue?: () => Promise<string>
}) {
  const [revealedLocal, setRevealedLocal] = useState(!masked)
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)
  const [fetching, setFetching] = useState(false)
  const toast = useToast()

  // ⚠️ 受控优先 —— `revealedProp` 是 `false` 时也要用它的值，
  //   所以用 `??`（`false ?? x` = `false`，不是 x）。
  const controlled = revealedProp !== undefined
  const revealed = revealedProp ?? revealedLocal

  // ⚠️ 受控时组件**不切**自己的状态 —— 由外部管。
  //   （受控模式下眼睛按钮也不显示，所以没有点击路径。）
  function toggleRevealed() {
    if (!controlled) setRevealedLocal((v) => !v)
  }

  // ⚠️ **关键**：受控时**不能再打码** ——
  //   外部传进来的 value 要么是指纹（本来就该打码），
  //   要么是明文（打回去就白干了）。
  //   受控模式下由调用方决定传什么，组件不再改它。
  const display = controlled ? value : revealed || !masked ? value : maskMiddle(value)

  async function copy() {
    // ⚠️ 用 copyToClipboard 而不是 navigator.clipboard ——
    //   后者在 HTTP（局域网访问 NAS）下不可用，点了没反应。
    //
    // ⚠️ **当前显示的不是完整值、且调用方给了取完整值的路** ——
    //   就现取完整值再复制。
    //
    //   判定**只看 revealed + masked**（是否处于打码显示），
    //   不看 value 的内容 —— 内容比对在受控模式下永远失真。
    const showingFull = revealed || !masked
    let full = value
    if (!showingFull && getFullValue) {
      setFetching(true)
      try {
        full = await getFullValue()
      } catch {
        toast.bad('取完整值失败 —— 请先点"显示"再复制')
        setFetching(false)
        return
      }
      setFetching(false)
    }
    const ok = await copyToClipboard(full)
    if (!ok) {
      setFailed(true)
      toast.bad('复制失败 —— 请手动选中后按 Ctrl+C')
      setTimeout(() => setFailed(false), 2400)
      return
    }
    setCopied(true)
    // ⚠️ **弹窗提示** —— 之前只有图标变一下，几乎看不见。
    //   用户点了之后必须**明确知道**"复制成功了"。
    toast.ok(showingFull ? '已复制到剪贴板' : '已复制完整值（界面仍显示指纹）')
    setTimeout(() => setCopied(false), 1400)
  }

  return (
    <div className={cn('flex items-center gap-1.5', className)}>
      <code
        className={cn(
          'min-w-0 flex-1 truncate rounded-[var(--r-sm)] border border-[var(--line)]',
          'bg-[var(--surface-2)] px-2.5 py-1.5 text-[12.5px] text-[var(--ink)]',
          mono && 'font-[family-name:var(--mono)]',
        )}
        title={controlled || revealed || !masked ? value : undefined}
      >
        {display || '—'}
      </code>

      {allowReveal && masked && !controlled ? (
        <Button
          size="sm"
          variant="ghost"
          onClick={toggleRevealed}
          aria-label={revealed ? '隐藏' : '显示'}
          title={revealed ? '隐藏' : '显示'}
        >
          {revealed ? <EyeOff size={13} /> : <Eye size={13} />}
        </Button>
      ) : null}

      <Button
        size="sm"
        variant="ghost"
        onClick={copy}
        aria-label="复制"
        title={copied ? '已复制' : failed ? '复制失败 —— 请手动选中复制' : fetching ? '正在取完整值…' : '复制'}
        disabled={!value || fetching}
      >
        {copied ? (
          <Check size={13} className="text-[var(--ok)]" />
        ) : failed ? (
          <X size={13} className="text-[var(--bad)]" />
        ) : fetching ? (
          <span className="inline-block h-[13px] w-[13px] animate-spin rounded-full border-2 border-[var(--line)] border-t-[var(--accent)]" />
        ) : (
          <Copy size={13} />
        )}
      </Button>
    </div>
  )
}

/**
 * 把中间的字符换成省略号。
 *
 * ⚠️ 保留头尾 —— 让人能确认"是不是同一把"。
 */
export function maskMiddle(value: string): string {
  if (!value) return ''
  if (value.length <= 12) return value
  const head = value.slice(0, 8)
  const tail = value.slice(-4)
  return `${head}…${tail}`
}
