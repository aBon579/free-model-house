import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'
import { Eye, EyeOff } from 'lucide-react'
import { useState, type ReactNode } from 'react'

/** 合并 class 名。 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

// Button

type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'
type ButtonSize = 'sm' | 'md'

const buttonVariants: Record<ButtonVariant, string> = {
  primary:
    'bg-[var(--accent)] text-[var(--accent-ink)] hover:bg-[var(--accent-2)] ' +
    'shadow-[var(--shadow-1)] disabled:opacity-45 disabled:shadow-none',
  secondary:
    'bg-[var(--surface)] text-[var(--ink)] border border-[var(--line)] ' +
    // ⚠️ **悬停底色用 --surface-hover** —— 比 --surface-2 再深一档。
    //   实测 surface→surface-2 只有 1.13:1，看不出来。
    'hover:bg-[var(--surface-hover)] shadow-[var(--shadow-1)] disabled:opacity-45',
  ghost:
    'text-[var(--ink-2)] hover:bg-[var(--surface-hover)] hover:text-[var(--ink)] ' +
    'disabled:opacity-45',
  danger:
    'bg-[var(--bad)] text-white hover:opacity-90 shadow-[var(--shadow-1)] disabled:opacity-45',
}

const buttonSizes: Record<ButtonSize, string> = {
  sm: 'h-7 px-2.5 text-[12.5px] gap-1.5',
  md: 'h-9 px-3.5 text-[13.5px] gap-2',
}

/** 按钮。 */
export function Button({
  variant = 'secondary',
  size = 'md',
  className,
  children,
  ...rest
}: {
  variant?: ButtonVariant
  size?: ButtonSize
} & React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button
      type="button"
      className={cn(
        'inline-flex items-center justify-center rounded-[var(--r-sm)] font-medium',
        'transition-[background-color,border-color,color,opacity] duration-150',
        'disabled:cursor-not-allowed',
        buttonVariants[variant],
        buttonSizes[size],
        // ⚠️ 触屏上小按钮要放大 —— 28px 点不准。
        'fmh-touchable',
        // ⚠️ **按压反馈** ——
        //   之前点按钮完全没有物理反馈，手感上像"没点到"。
        //   按下时缩一点点 + 变暗，一眼就看出点到了。
        'fmh-pressable',
        // ⚠️ **悬停聚焦** ——
        //   实测原来只改背景色，差异只有 1.13:1 —— 几乎看不出来。
        //   现在悬停时给**边框 + 外发光 + 上浮**三个信号。
        'fmh-hoverable',
        className,
      )}
      {...rest}
    >
      {children}
    </button>
  )
}

// Card

/**
 * 加载指示器。
 *
 * ⚠️ **每个耗时操作都要有它** ——
 * 探测要跑好几秒，没有旋转图标用户会以为没点到，
 * 然后连点好几次（每次都花上游额度）。
 */
export function Spinner({ size = 13, className }: { size?: number; className?: string }) {
  return (
    <svg
      className={cn('fmh-spin shrink-0', className)}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      aria-hidden="true"
    >
      <circle
        cx="12"
        cy="12"
        r="9"
        stroke="currentColor"
        strokeWidth="3"
        strokeOpacity="0.25"
      />
      <path
        d="M21 12a9 9 0 0 0-9-9"
        stroke="currentColor"
        strokeWidth="3"
        strokeLinecap="round"
      />
    </svg>
  )
}

/** 卡片。 */
export function Card({
  className,
  children,
  ...rest
}: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn(
        'rounded-[var(--r-md)] border border-[var(--line)] bg-[var(--surface)]',
        'shadow-[var(--shadow-1)]',
        // ⚠️ **flex 列布局** —— 这样并排时卡片能**等高**：
        //   父容器 `items-stretch` 会把两张卡拉到同高，
        //   而内容区 `flex-1` 会撑满剩余空间。
        'flex flex-col',
        className,
      )}
      {...rest}
    >
      {children}
    </div>
  )
}

/** 卡片标题。 */
export function CardHeader({
  title,
  description,
  actions,
  className,
}: {
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        'flex items-start justify-between gap-4 px-4 py-3',
        'border-b border-[var(--line)]',
        className,
      )}
    >
      <div className="min-w-0">
        <div className="text-[13.5px] font-semibold text-[var(--ink)]">{title}</div>
        {description ? (
          <div className="mt-0.5 text-[12.5px] text-[var(--ink-3)]">{description}</div>
        ) : null}
      </div>
      {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </div>
  )
}

/** 卡片内容。 */
export function CardBody({ className, children, ...rest }: React.HTMLAttributes<HTMLDivElement>) {
  return (
    // ⚠️ **`flex-1`** —— 撑满卡片剩余高度，让并排的卡片内容区一样高。
    <div className={cn('flex-1 px-4 py-3.5', className)} {...rest}>
      {children}
    </div>
  )
}

// Badge

type BadgeTone = 'ok' | 'warn' | 'bad' | 'idle' | 'accent' | 'zen' | 'openrouter'

const badgeTones: Record<BadgeTone, string> = {
  ok: 'bg-[var(--ok-soft)] text-[var(--ok)] border-[var(--ok-line)]',
  warn: 'bg-[var(--warn-soft)] text-[var(--warn)] border-[var(--warn-line)]',
  bad: 'bg-[var(--bad-soft)] text-[var(--bad)] border-[var(--bad-line)]',
  idle: 'bg-[var(--idle-soft)] text-[var(--idle)] border-[var(--idle-line)]',
  accent: 'bg-[var(--accent-soft)] text-[var(--accent)] border-[var(--accent-line)]',
  zen: 'bg-[var(--lane-zen-soft)] text-[var(--lane-zen)] border-[var(--lane-zen-line)]',
  openrouter:
    'bg-[var(--lane-openrouter-soft)] text-[var(--lane-openrouter)] border-[var(--lane-openrouter-line)]',
}

/** 徽章。 */
export function Badge({
  tone = 'idle',
  className,
  children,
  ...rest
}: {
  tone?: BadgeTone
} & React.HTMLAttributes<HTMLSpanElement>) {
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1 rounded-full border px-2 py-[1px]',
        'text-[11.5px] font-medium leading-[17px] whitespace-nowrap',
        badgeTones[tone],
        className,
      )}
      {...rest}
    >
      {children}
    </span>
  )
}

// Field

/** 表单行。 */
export function Field({
  label,
  hint,
  error,
  danger,
  children,
  className,
}: {
  label: ReactNode
  hint?: ReactNode
  /**
   * 错误提示。
   *
   * ⚠️ **比 hint 优先** —— 错误是**当下**的反馈，
   *   提示是静态说明。两者同时出现时，错误更重要。
   */
  error?: ReactNode
  /** 危险项 —— 提示用红色。 */
  danger?: boolean
  children: ReactNode
  className?: string
}) {
  return (
    <div className={cn('py-2.5', className)}>
      <div className="text-[13px] font-medium text-[var(--ink)]">{label}</div>
      {error ? (
        <div className="mt-0.5 text-[12.5px] leading-[18px] text-[var(--bad)]">{error}</div>
      ) : hint ? (
        <div
          className={cn(
            'mt-0.5 text-[12.5px] leading-[18px]',
            danger ? 'text-[var(--bad)]' : 'text-[var(--ink-3)]',
          )}
        >
          {hint}
        </div>
      ) : null}
      <div className="mt-2">{children}</div>
    </div>
  )
}

/** 输入框。 */
export function Input({
  className,
  ...rest
}: React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cn(
        'h-9 w-full rounded-[var(--r-sm)] border border-[var(--line)]',
        'bg-[var(--surface)] px-3 text-[13.5px] text-[var(--ink)]',
        'placeholder:text-[var(--ink-3)]',
        'transition-[border-color,box-shadow] duration-150',
        // ⚠️ **悬停时边框变亮** —— 输入框也是能点的东西。
        'hover:border-[var(--accent-line)]',
        'focus:border-[var(--accent)] focus:outline-none',
        'focus:shadow-[0_0_0_3px_var(--accent-soft)]',
        'disabled:opacity-55',
        className,
      )}
      {...rest}
    />
  )
}

/** 下拉框。 */
export function Select({
  className,
  ...rest
}: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      className={cn(
        'h-9 rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface)]',
        'px-2.5 text-[13px] text-[var(--ink)]',
        'transition-[border-color,box-shadow] duration-150',
        // ⚠️ **悬停也要有反馈** —— 下拉框也是能按的东西。
        'hover:border-[var(--accent-line)] hover:shadow-[0_0_0_1px_var(--accent-line)]',
        'focus:border-[var(--accent)] focus:outline-none focus:shadow-[0_0_0_1px_var(--accent)]',
        className,
      )}
      {...rest}
    />
  )
}

/** 密码输入框（带显示/隐藏的眼睛）。 */
export function PasswordInput({
  className,
  ...rest
}: React.InputHTMLAttributes<HTMLInputElement>) {
  const [shown, setShown] = useState(false)
  return (
    <div className="relative">
      <input
        type={shown ? 'text' : 'password'}
        className={cn(
          'h-9 w-full rounded-[var(--r-sm)] border border-[var(--line)]',
          'bg-[var(--surface)] px-3 pr-9 text-[13.5px] text-[var(--ink)]',
          'placeholder:text-[var(--ink-3)]',
          'transition-[border-color,box-shadow] duration-150',
          'hover:border-[var(--accent-line)]',
          'focus:border-[var(--accent)] focus:outline-none',
          'focus:shadow-[0_0_0_3px_var(--accent-soft)]',
          'disabled:opacity-55',
          className,
        )}
        {...rest}
      />
      {/* ⚠️ **眼睛按钮** ——
          输错密码时看不到自己错在哪一位，
          只能删了重打 —— 眼睛按钮是刚需，不是装饰。 */}
      <button
        type="button"
        onClick={() => setShown((s) => !s)}
        aria-label={shown ? '隐藏密码' : '显示密码'}
        title={shown ? '隐藏密码' : '显示密码'}
        className={cn(
          'absolute right-1 top-1/2 flex h-7 w-7 -translate-y-1/2 items-center justify-center',
          'rounded-[var(--r-xs)] text-[var(--ink-3)]',
          'transition-colors duration-150',
          'hover:bg-[var(--surface-hover)] hover:text-[var(--ink-2)]',
          'fmh-hoverable',
        )}
      >
        {shown ? <EyeOff size={14} /> : <Eye size={14} />}
      </button>
    </div>
  )
}

/** 开关。 */
export function Switch({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        'relative h-[22px] w-[38px] shrink-0 rounded-full',
        'transition-[background-color,box-shadow] duration-200 disabled:opacity-50',
        // ⚠️ 触屏上开关只有 22px 高，**点不准**。
        //   用伪元素把可点区域撑到 40px，视觉不变。
        'fmh-switch',
        // ⚠️ **开启时是绿色** —— 绿色 = "开着"，跟直觉一致
        //   （蓝色是"选中/强调"，用来做按钮和选中态更合适）。
        //
        //   ⚠️ **用 `--switch-on` 而不是 `--ok`** ——
        //   亮色主题的 `--ok` 是墨绿（#047857），暗色是亮绿（#34d399），
        //   同一个开关在两个主题下颜色不一样，看着像两回事。
        //   `--switch-on` 两个主题都是 #34d399。
        checked ? 'bg-[var(--switch-on)]' : 'bg-[var(--idle-line)]',
        // ⚠️ **悬停时光晕** —— 开关很小，光晕能明确告诉用户"指针在这"。
        'hover:shadow-[0_0_0_2px_var(--accent-soft),0_0_10px_1px_var(--accent-line)]',
        'focus-visible:shadow-[0_0_0_2px_var(--accent),0_0_0_4px_var(--accent-soft)]',
      )}
    >
      {/*
        ⚠️ **滑块必须显式给 `left-0`** ——
        之前只写了 `absolute top-[2px]`，没给 `left`：
        绝对定位元素没有 `left` 时会退回**静态位置**，
        而这里静态位置是按钮内容流的末尾 —— 于是滑块跑到轨道外，
        `translate-x-[18px]` 只是把它推得更远。

        尺寸：轨道 38px、滑块 18px → 关闭时 left=2，开启时 left=18（38-18-2）。
      */}
      <span
        className={cn(
          'absolute left-0 top-[2px] h-[18px] w-[18px] rounded-full bg-white',
          'shadow-[var(--shadow-1)] transition-transform duration-200',
          checked ? 'translate-x-[18px]' : 'translate-x-[2px]',
        )}
      />
    </button>
  )
}

// 杂项

/** 空状态。 */
export function Empty({
  icon,
  title,
  description,
  action,
}: {
  icon?: ReactNode
  title: string
  description?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center justify-center px-6 py-14 text-center">
      {icon ? (
        <div className="mb-3 text-[var(--ink-3)] opacity-60">{icon}</div>
      ) : null}
      <div className="text-[13.5px] font-medium text-[var(--ink-2)]">{title}</div>
      {description ? (
        <div className="mt-1 max-w-md text-[12.5px] leading-[19px] text-[var(--ink-3)]">
          {description}
        </div>
      ) : null}
      {action ? <div className="mt-4">{action}</div> : null}
    </div>
  )
}

/** 加载态。 */
export function Loading({ label = '加载中' }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-2 px-6 py-12 text-[13px] text-[var(--ink-3)]">
      <span className="status-dot h-1.5 w-1.5 rounded-full bg-[var(--accent)]" />
      {label}
    </div>
  )
}

/** 错误态。 */
export function ErrorNote({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-[var(--r-sm)] border border-[var(--bad-line)] bg-[var(--bad-soft)] px-3 py-2 text-[12.5px] text-[var(--bad)]">
      {children}
    </div>
  )
}

/** 提示条。 */
export function Notice({
  tone = 'accent',
  children,
}: {
  tone?: 'accent' | 'warn' | 'bad'
  children: ReactNode
}) {
  const tones = {
    accent: 'border-[var(--accent-line)] bg-[var(--accent-soft)] text-[var(--accent)]',
    warn: 'border-[var(--warn-line)] bg-[var(--warn-soft)] text-[var(--warn)]',
    bad: 'border-[var(--bad-line)] bg-[var(--bad-soft)] text-[var(--bad)]',
  }
  return (
    <div
      className={cn(
        'rounded-[var(--r-sm)] border px-3 py-2 text-[12.5px] leading-[19px]',
        tones[tone],
      )}
    >
      {children}
    </div>
  )
}

/** 统计块。 */
export function Stat({
  label,
  value,
  hint,
  tone,
}: {
  label: ReactNode
  value: ReactNode
  hint?: ReactNode
  tone?: 'ok' | 'warn' | 'bad'
}) {
  const valueColor = tone
    ? {
        ok: 'text-[var(--ok)]',
        warn: 'text-[var(--warn)]',
        bad: 'text-[var(--bad)]',
      }[tone]
    : 'text-[var(--ink)]'
  return (
    <div className="min-w-0">
      <div className="text-[11.5px] text-[var(--ink-3)]">{label}</div>
      <div className={cn('tnum mt-0.5 text-[19px] font-semibold leading-7', valueColor)}>
        {value}
      </div>
      {hint ? <div className="mt-0.5 text-[11.5px] text-[var(--ink-3)]">{hint}</div> : null}
    </div>
  )
}
