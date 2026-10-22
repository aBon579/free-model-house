import { useEffect, useState } from 'react'
import { KeyRound, Lock, RotateCw } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import {
  Button,
  Card,
  CardBody,
  CardHeader,
  Field,
  Input,
  Notice,
  PasswordInput,
  Switch,
  cn,
} from '../components/ui'
import { CopyField } from '../components/common'
import { useToast } from '../components/toast'
import { LAN_TIERS, type LanTier, type Settings } from '../api/types'
import {
  useChangePassword,
  useKeys,
  useRotateKey,
  useSaveSettings,
  useSettings,
} from '../api/client'

/**
 * 每档的默认限额。
 *
 * ⚠️ 必须与后端 `server.TierLimits` 一致 ——
 *   这是前端在"切档位时补默认值"用的。
 */
const TIER_DEFAULTS: Record<Exclude<LanTier, 'deep'>, { perMinute: number; maxConcurrent: number }> = {
  light: { perMinute: 10, maxConcurrent: 2 },
  standard: { perMinute: 60, maxConcurrent: 6 },
}

/** 设置页。 */
export function Settings() {
  const { data: settings } = useSettings()
  const { data: keys } = useKeys(false)
  const save = useSaveSettings()
  const rotate = useRotateKey()

  // ⚠️ **显示完整 Key** —— 默认只给指纹，明文要显式点开。
  const [revealed, setRevealed] = useState<{ lan: boolean }>({ lan: false })
  const { data: fullKeys } = useKeys(revealed.lan)

  // 档位列表 —— 优先用后端发的（文案以后端为准），兜底用前端的。
  const tiers = settings?.lanTiers?.length
    ? settings.lanTiers.map((t) => ({
        value: t.value as LanTier,
        label: t.label,
        hint: t.hint,
      }))
    : LAN_TIERS
  // ⚠️ 本地草稿 —— 后台轮询**不覆盖**用户正在编辑的东西。
  const [draft, setDraft] = useState(settings)
  const [rotated, setRotated] = useState<string | null>(null)

  useEffect(() => {
    setDraft(settings)
  }, [settings])

  if (!draft) {
    return (
      <>
        <PageTitle title="设置" />
        <Card>
          <CardBody className="py-8 text-center text-[12.5px] text-[var(--ink-3)]">
            读取中…
          </CardBody>
        </Card>
      </>
    )
  }

  async function doRotate(which: 'local' | 'lan') {
    const res = await rotate.mutateAsync(which)
    setRotated(res.value)
    setTimeout(() => setRotated(null), 6000)
  }

  return (
    <>
      <PageTitle title="设置" />

      <div className="space-y-4">
        {/* 接入 */}
        <Card>
          <CardHeader title="接入" />
          <CardBody className="divide-y divide-[var(--line-soft)]">
            <Field
              label="局域网推理"
              hint="决定局域网里的 Agent 能用多少。限额对所有推理请求一视同仁。"
            >
              <div className="space-y-2.5">
                {/* ⚠️ 三档（light / standard / deep）——
                    之前是 on/off，粒度太粗：
                    要么全开（谁都能用你的额度），要么全关。
                    现在按"用多少"分档。 */}
                <div className="flex flex-wrap gap-2">
                  {tiers.map((t) => {
                    const active = draft.lanTier === t.value
                    return (
                      <button
                        key={t.value}
                        onClick={() => {
                          // ⚠️ 换档位就**重置限额** ——
                          //   否则上一档的数值会带过来
                          //   （比如从 deep 切到 light，却还留着"不限量"）。
                          const patch: Partial<Settings> = { lanTier: t.value }
                          const next: Settings = { ...draft, lanTier: t.value }
                          if (t.value !== 'deep') {
                            const def = TIER_DEFAULTS[t.value]
                            if (draft.lanPerMinute <= 0) {
                              patch.lanPerMinute = def.perMinute
                              next.lanPerMinute = def.perMinute
                            }
                            if (draft.lanMaxConcurrent <= 0) {
                              patch.lanMaxConcurrent = def.maxConcurrent
                              next.lanMaxConcurrent = def.maxConcurrent
                            }
                          }
                          setDraft(next)
                          save.mutate(patch)
                        }}
                        aria-pressed={active}
                        title={t.hint}
                        className={cn(
                          'fmh-touchable fmh-hoverable flex flex-col items-start gap-0.5 rounded-[var(--r-sm)] border px-3 py-2 text-left transition-colors duration-150',
                          active
                            ? 'border-[var(--accent)] bg-[var(--accent-soft)] text-[var(--ink)]'
                            : 'border-[var(--line)] text-[var(--ink-2)] hover:bg-[var(--surface-hover)]',
                        )}
                      >
                        <span className="text-[13px] font-medium">{t.label}</span>
                        <span className="text-[11.5px] text-[var(--ink-3)]">{t.hint}</span>
                      </button>
                    )
                  })}
                </div>

                {draft.lanTier !== 'deep' ? (
                  <div
                    className="flex flex-wrap items-center gap-4 rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface-2)] px-3 py-2.5"
                    data-fmh="lan-limits"
                  >
                    <div>
                      <div className="mb-1 text-[12px] font-medium text-[var(--ink-2)]">
                        每分钟请求上限
                      </div>
                      <div className="flex items-center gap-1.5">
                        <Input
                          type="number"
                          min={0}
                          value={draft.lanPerMinute}
                          onChange={(e) => {
                            const v = Math.max(0, Number(e.target.value) || 0)
                            setDraft({ ...draft, lanPerMinute: v })
                          }}
                          onBlur={() => save.mutate({ lanPerMinute: draft.lanPerMinute })}
                          className="w-20"
                        />
                        <span className="text-[12px] text-[var(--ink-3)]">
                          次/分钟（0 = 不限）
                        </span>
                      </div>
                    </div>
                    <div>
                      <div className="mb-1 text-[12px] font-medium text-[var(--ink-2)]">
                        并发上限
                      </div>
                      <div className="flex items-center gap-1.5">
                        <Input
                          type="number"
                          min={0}
                          value={draft.lanMaxConcurrent}
                          onChange={(e) => {
                            const v = Math.max(0, Number(e.target.value) || 0)
                            setDraft({ ...draft, lanMaxConcurrent: v })
                          }}
                          onBlur={() => save.mutate({ lanMaxConcurrent: draft.lanMaxConcurrent })}
                          className="w-20"
                        />
                        <span className="text-[12px] text-[var(--ink-3)]">
                          个（0 = 不限）
                        </span>
                      </div>
                    </div>
                  </div>
                ) : null}

                <div className="text-[11.5px] leading-[17px] text-[var(--ink-3)]">
                  限额是对整个网关生效的，不是对单个 Key。
                </div>
              </div>
            </Field>

            <Field label="局域网 Key" hint="局域网内 Agent 调用 API Key">
              <div className="flex flex-wrap items-center gap-2">
                {keys ? (
                  <CopyField
                    value={
                      revealed.lan ? (fullKeys?.lan.value ?? '') : keys.lan.hint
                    }
                    masked={false}
                    allowReveal={false}
                    className="max-w-xs"
                  />
                ) : null}
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setRevealed((r) => ({ ...r, lan: !r.lan }))}
                >
                  {revealed.lan ? '隐藏' : '显示完整'}
                </Button>
                <Button
                  size="sm"
                  onClick={() => doRotate('lan')}
                  disabled={rotate.isPending}
                >
                  <RotateCw size={13} />
                  轮换
                </Button>
              </div>
            </Field>

            {rotated ? (
              <div className="pt-3">
                <Notice tone="warn">
                  <div className="mb-1.5 font-medium">新的 Key（只显示这一次）</div>
                  <CopyField value={rotated} masked={false} allowReveal={false} />
                </Notice>
              </div>
            ) : null}
          </CardBody>
        </Card>

        {/* ⚠️ **自动跨模型调用 + 管理密码并排** ——
            这两张卡都是"小配置"，各占一整行太浪费纵向空间。
            并排放，高度对齐，扫一眼就知道有两个开关。 */}
        <div className="grid gap-4 md:grid-cols-2 md:items-stretch">
          {/* 自动跨模型调用 */}
          <Card className="h-full">
            {/* ⚠️ **Toggle 放在标题右边** ——
                之前开关在 Field 下面（要往下扫才看到），
                现在跟标题并排 —— 一眼就知道这张卡在控制什么。 */}
            <CardHeader
              title="自动跨模型调用"
              actions={
                <Switch
                  checked={draft.autoModelSwitch}
                  onChange={(v) => {
                    setDraft({ ...draft, autoModelSwitch: v })
                    save.mutate({ autoModelSwitch: v })
                  }}
                  label="自动跨模型调用"
                />
              }
            />
            <CardBody>
              <div
                className={cn(
                  'text-[12.5px] leading-[19px]',
                  draft.autoModelSwitch
                    ? 'text-[var(--ink-2)]'
                    : 'text-[var(--ink-3)]',
                )}
              >
                <p>
                  {draft.autoModelSwitch
                    ? '当前模型限流或不可用时，自动改用其他可用模型。'
                    : '只重试当前模型。重试仍不行就直接报错，不换模型。'}
                </p>
                <p className="mt-1">
                  开启后的切换顺序：Kilo ↔ Zen（免密，不花你的额度），
                  两条都不行才用 OpenRouter。
                </p>
                {draft.autoModelSwitch ? (
                  <p className="mt-1 text-[var(--warn)]">
                    切到 OpenRouter 的那部分用你的 OpenRouter 额度。
                  </p>
                ) : null}
              </div>
            </CardBody>
          </Card>

          <ChangePasswordCard />
        </div>

        {/* 探测 */}
        <Card>
          <CardHeader title="探测" />
          <CardBody className="divide-y divide-[var(--line-soft)]">
            <Field
              label="自动探测"
              hint="定期发一个最小请求，确认模型是不是真的可用。⚠ 探测会消耗上游额度 —— 每探一个模型就是一次真实推理。"
            >
              <Switch
                checked={draft.probeEnabled}
                onChange={(v) => {
                  setDraft({ ...draft, probeEnabled: v })
                  save.mutate({ probeEnabled: v })
                }}
                label="自动探测"
              />
            </Field>

            <Field
              label="探测间隔（分钟）"
              hint="太短会白白消耗额度，太长则状态不准。默认 10 分钟比较合适。"
            >
              <Input
                type="number"
                min={1}
                max={1440}
                value={draft.probeIntervalMin}
                onChange={(e) => {
                  const v = Number(e.target.value) || 10
                  setDraft({ ...draft, probeIntervalMin: v })
                }}
                onBlur={() => save.mutate({ probeIntervalMin: draft.probeIntervalMin })}
                className="w-32"
              />
            </Field>

            {/* ⚠️ **没有"每轮探测几个模型"** ——
                探测是**全量**的，每 15 分钟把每个免费模型都探一遍。
                批量这个概念已经不存在了 ——
                留着它会让人以为还在"少量多次"。 */}

            {/* ⚠️ **自适应批量** ——
                上游越拥挤，探得越少。
                高峰期继续按正常节奏探测就是拿额度去撞限流墙。 */}
            <Field
              label="探测批量自适应"
              hint="上游限流变多时自动少探几个，恢复后再调回去。省额度，也避免在限流时拿到的全是无效信息。"
            >
              <Switch
                checked={draft.adaptiveProbe}
                onChange={(v) => {
                  setDraft({ ...draft, adaptiveProbe: v })
                  save.mutate({ adaptiveProbe: v })
                }}
                label="探测批量自适应"
              />
            </Field>
          </CardBody>
        </Card>

        {/* ⚠️ **删掉「关于」卡** ——
            它原本写的是"所有推理 Key 都是 fmh- 前缀 + 40 位 hex"
            和"凭据加密密钥来自环境变量 FMH_SECRET_KEY"。

            那是**实现细节** —— 用户不需要知道 Key 的内部格式，
            也不需要知道加密密钥怎么来的。
            真要查，命令行 `fmh key` 会直接给他。

            换成**用户真正要问的问题**：数据存在哪、怎么备份。 */}
        <Card>
          <CardHeader title="数据" />
          <CardBody className="space-y-2 text-[12.5px] leading-[19px] text-[var(--ink-2)]">
            <div className="flex items-center gap-2">
              <KeyRound size={13} className="text-[var(--ink-3)]" />
              <span>所有设置、密钥和用量记录都存在数据目录里的一个文件里。</span>
            </div>
            <div className="text-[var(--ink-3)]">
              备份时连数据目录一起复制 —— 换 NAS 时不会有任何东西丢。
            </div>
          </CardBody>
        </Card>
      </div>
    </>
  )
}

/** 密码最少几位（必须与后端 `auth.MinPasswordLen` 一致）。 */
const MIN_PASSWORD_LEN = 8

/**
 * 「修改密码」卡片。
 *
 * ⚠️ **卡片里不放输入框** ——
 *
 *   之前三个密码框直接摊在卡片里，把整张卡撑得很高，
 *   跟左边的「路由」卡**高度对不上**（右边多出一大截空白）。
 *
 *   现在：卡片只放**说明** + 右下角一个「修改密码」按钮，
 *   点击**弹窗**出来填。
 *
 *   好处：
 *     ① 两张卡**高度一致**，并排看着齐整；
 *     ② 密码框**不占空间**（不用的时候看不见）；
 *     ③ 改密码是**低频操作** —— 不该在主界面上摆三个输入框。
 */
function ChangePasswordCard() {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Card className="h-full">
        <CardHeader title="修改密码" />
        <CardBody className="flex flex-col">
          <div className="text-[12.5px] leading-[19px] text-[var(--ink-2)]">
            <div className="font-medium text-[var(--ink)]">管理台密码</div>
            <p className="mt-1">
              管理台用密码登录。局域网里的 Agent 用的是另一把 Key，两者是分开的。
            </p>
            <p className="mt-1 text-[var(--ink-3)]">
              改完密码会退出所有管理会话 —— 请用新密码重新登录。
            </p>
          </div>
          {/* ⚠️ **按钮靠右下角** —— 跟左边的卡视觉重心一致。 */}
          <div className="mt-auto flex justify-end pt-3">
            <Button size="sm" onClick={() => setOpen(true)}>
              <Lock size={13} />
              修改密码
            </Button>
          </div>
        </CardBody>
      </Card>

      {open ? <ChangePasswordDialog onClose={() => setOpen(false)} /> : null}
    </>
  )
}

/**
 * 修改密码的弹窗。
 *
 * ⚠️ 改完密码**所有管理会话都会失效**（后端行为）——
 *   所以要明确告诉用户"改完要重新登录"。
 */
function ChangePasswordDialog({ onClose }: { onClose: () => void }) {
  const change = useChangePassword()
  const toast = useToast()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')

  const tooShort = next.length > 0 && next.length < MIN_PASSWORD_LEN
  const mismatch = confirm.length > 0 && next !== confirm
  const canSubmit =
    current.length > 0 && next.length >= MIN_PASSWORD_LEN && next === confirm && !change.isPending

  async function submit() {
    try {
      await change.mutateAsync({ current, new: next, confirm })
      toast.ok('密码已修改 —— 请用新密码重新登录')
      onClose()
    } catch (e) {
      toast.bad((e as Error).message)
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/45 px-4"
      onClick={(e) => {
        // ⚠️ **点遮罩关闭** —— 跟常见的弹窗行为一致。
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="w-full max-w-sm">
        <Card>
          <CardHeader title="修改密码" />
          <CardBody className="space-y-3">
            {change.isError ? (
              <Notice tone="bad">
                <div className="text-[12px]">{(change.error as Error).message}</div>
              </Notice>
            ) : null}

            <Field label="当前密码">
              <PasswordInput
                value={current}
                onChange={(e) => setCurrent(e.target.value)}
                autoComplete="current-password"
                aria-label="当前密码"
                autoFocus
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
                  if (e.key === 'Enter' && canSubmit) submit()
                }}
                autoComplete="new-password"
                aria-label="确认新密码"
              />
            </Field>

            <div className="flex items-center justify-end gap-2 pt-1">
              <Button variant="secondary" onClick={onClose}>
                取消
              </Button>
              <Button disabled={!canSubmit} onClick={submit}>
                <Lock size={13} />
                {change.isPending ? '提交中…' : '修改密码'}
              </Button>
            </div>
          </CardBody>
        </Card>
      </div>
    </div>
  )
}
