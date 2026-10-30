import { useState } from 'react'
import { KeyRound, Lock, Plus, RotateCw, Trash2 } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import {
  Button,
  Card,
  CardBody,
  CardHeader,
  Field,
  Input,
  Switch,
  cn,
} from '../components/ui'
import { CopyField } from '../components/common'
import { copyToClipboard } from '../lib/clipboard'
import { useToast } from '../components/toast'
import {
  useCreateKey,
  useDeleteKey,
  useGatewayTier,
  useKeys,
  useRotateKeyById,
  useUpdateKey,
} from '../api/client'
import type { GatewayTier, LanTier } from '../api/types'

/**
 * 网关档位的说明。
 *
 * ⚠️ 与后端 `gatewayTierLabels()` 的文案一致 ——
 *   这里是兜底，正常以后端发的为准。
 */
const GATEWAY_CARDS: Array<{ value: GatewayTier; label: string; hint: string }> = [
  {
    value: 'off',
    label: 'off · 单 Key 分治',
    hint: '网关不限制 —— 每把 Key 按自己下面选的档位限流',
  },
  {
    value: 'light',
    label: 'light · 轻量',
    hint: '全部 Key 强制 10 次/分钟、2 并发',
  },
  {
    value: 'standard',
    label: 'standard · 标准',
    hint: '全部 Key 强制 60 次/分钟、6 并发',
  },
  {
    value: 'deep',
    label: 'deep · 不限',
    hint: '全部 Key 不限量',
  },
]

/** 单 Key 的三档（没有 off）。 */
const KEY_TIERS: Array<{ value: LanTier; label: string }> = [
  { value: 'light', label: 'light' },
  { value: 'standard', label: 'standard' },
  { value: 'deep', label: 'deep' },
]

/** 标签最大长度 —— 与后端 MaxKeyLabelChars 一致（兜底）。 */
const MAX_LABEL = 6

/** 标签输入的校验（字符数，不是字节数 —— 中文一个字算一个）。 */
function labelError(label: string): string {
  if (!label) return ''
  const n = [...label].length
  if (n > MAX_LABEL) return `最多 ${MAX_LABEL} 个字（现在 ${n} 个）`
  return ''
}

/** Key Hub 页 —— 网关级限流 + 多把 Key。 */
export function Access() {
  const { data, isLoading } = useKeys(false)
  const [revealedId, setRevealedId] = useState<string | null>(null)
  const { data: fullKeys } = useKeys(revealedId !== null)
  const toast = useToast()

  const create = useCreateKey()
  const update = useUpdateKey()
  const remove = useDeleteKey()
  const rotate = useRotateKeyById()
  const setGatewayTier = useGatewayTier()

  // 新建表单。
  const [newLabel, setNewLabel] = useState('')
  const [newTier, setNewTier] = useState<LanTier>('standard')
  const [creating, setCreating] = useState(false)

  if (isLoading || !data) {
    return (
      <>
        <PageTitle title="Key Hub" />
        <Card>
          <CardBody className="py-8 text-center text-[12.5px] text-[var(--ink-3)]">
            读取中…
          </CardBody>
        </Card>
      </>
    )
  }

  const keys = data.keys ?? []
  const maxLabel = data.maxLabel ?? MAX_LABEL
  const gatewayTier = data.gatewayTier ?? 'off'
  const locked = gatewayTier !== 'off'
  const err = labelError(newLabel)

  async function doCreate() {
    try {
      const res = await create.mutateAsync({ label: newLabel, tier: newTier })
      setNewLabel('')
      setCreating(false)
      // ⚠️ **新 Key 直接进剪贴板** —— 不再弹"只显示这一次"的提示块。
      //   创建后那把 Key 已经在列表里（打码状态），
      //   需要完整值随时点它的"复制"（会现取完整值）或"显示完整"。
      const ok = await copyToClipboard(res.key.value ?? '')
      toast.ok(ok ? 'Key 已创建 —— 完整值已复制到剪贴板' : 'Key 已创建')
    } catch (e) {
      toast.bad((e as Error).message)
    }
  }

  return (
    <>
      <PageTitle
        title="Key Hub"
        description="多把 Key 分给不同设备 —— 网关级限制优先级最高，网关不管时各 Key 按自己的档位。"
      />

      <div className="space-y-4">
        {/* ── 网关级限制 —— 卡片式档位（原设置页样式） ── */}
        <Card>
          <CardHeader
            title="网关级限制"
            description="优先级最高 —— 非 off 时所有 Key 强制跟随，单 Key 档位被锁定；回 off 后自动恢复各 Key 的设置。"
          />
          <CardBody>
            <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
              {GATEWAY_CARDS.map((t) => {
                const active = gatewayTier === t.value
                return (
                  <button
                    key={t.value}
                    onClick={() => {
                      setGatewayTier.mutate(t.value, {
                        onError: (e) => toast.bad((e as Error).message),
                      })
                    }}
                    aria-pressed={active}
                    title={t.hint}
                    className={cn(
                      'fmh-touchable fmh-hoverable flex flex-col items-start gap-0.5 rounded-[var(--r-sm)] border px-3 py-2.5 text-left transition-colors duration-150',
                      active
                        ? 'border-[var(--accent)] bg-[var(--accent-soft)] text-[var(--ink)]'
                        : 'border-[var(--line)] text-[var(--ink-2)] hover:bg-[var(--surface-hover)]',
                    )}
                  >
                    <span className="text-[13px] font-medium">{t.label}</span>
                    <span className="text-[11.5px] leading-[16px] text-[var(--ink-3)]">
                      {t.hint}
                    </span>
                    {active ? (
                      <span className="mt-0.5 text-[11px] font-medium text-[var(--accent)]">
                        当前生效
                      </span>
                    ) : null}
                  </button>
                )
              })}
            </div>
            {locked ? (
              <div className="mt-3 flex items-start gap-2 rounded-[var(--r-sm)] border border-[var(--warn-line)] bg-[var(--warn-soft)] px-3 py-2 text-[12px] text-[var(--warn)]">
                <Lock size={13} className="mt-0.5 shrink-0" />
                <span>
                  网关级限制生效中 —— 下面每把 Key 的档位按钮已锁定。各 Key 的档位设置**保留**，
                  切回 off 后自动恢复生效。
                </span>
              </div>
            ) : null}
          </CardBody>
        </Card>

        {/* ── 单 Key 列表 ── */}
        <Card>
          <CardHeader
            title="调用 Key"
            description="每台设备一把。标签认设备，档位管限流，停用收权限。"
            actions={
              <Button size="sm" onClick={() => setCreating((v) => !v)}>
                <Plus size={13} />
                新建 Key
              </Button>
            }
          />
          <CardBody className="space-y-3">
            {creating ? (
              <div className="space-y-3 rounded-[var(--r-md)] border border-[var(--line)] bg-[var(--surface-2)] p-3.5">
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field
                    label="标签"
                    hint={`认出这是哪台设备（最多 ${maxLabel} 个字）`}
                    error={err}
                  >
                    <Input
                      value={newLabel}
                      onChange={(e) => setNewLabel(e.target.value)}
                      placeholder="如：笔记本 / 手机 / 同事A"
                      maxLength={24}
                    />
                  </Field>
                  <Field label="档位" hint="这把 Key 的限流档位（网关级为 off 时生效）">
                    <div className="flex gap-1.5">
                      {KEY_TIERS.map((t) => (
                        <button
                          key={t.value}
                          onClick={() => setNewTier(t.value)}
                          aria-pressed={newTier === t.value}
                          className={cn(
                            'fmh-touchable fmh-hoverable flex-1 rounded-[var(--r-sm)] border px-2 py-1.5 text-[12.5px] transition-colors duration-150',
                            newTier === t.value
                              ? 'border-[var(--accent)] bg-[var(--accent-soft)] font-medium text-[var(--accent)]'
                              : 'border-[var(--line)] text-[var(--ink-2)] hover:bg-[var(--surface-hover)]',
                          )}
                        >
                          {t.label}
                        </button>
                      ))}
                    </div>
                  </Field>
                </div>
                <div className="flex justify-end gap-2">
                  <Button size="sm" variant="secondary" onClick={() => setCreating(false)}>
                    取消
                  </Button>
                  <Button size="sm" onClick={doCreate} disabled={!!err || create.isPending}>
                    <KeyRound size={13} />
                    {create.isPending ? '创建中…' : '创建'}
                  </Button>
                </div>
              </div>
            ) : null}

            {keys.length === 0 ? (
              <div className="py-6 text-center text-[12.5px] text-[var(--ink-3)]">
                还没有 Key —— 新建第一把。
              </div>
            ) : (
              keys.map((k) => {
                const revealed = revealedId === k.id
                const fullValue = revealed
                  ? (fullKeys?.keys.find((x) => x.id === k.id)?.value ?? '')
                  : ''
                return (
                  <div
                    key={k.id}
                    className="space-y-2.5 rounded-[var(--r-md)] border border-[var(--line)] bg-[var(--surface)] p-3.5"
                  >
                    {/* 行 1：开关 + 标签 + 操作 */}
                    <div className="flex flex-wrap items-center gap-2.5">
                      <Switch
                        checked={k.enabled}
                        onChange={(v) => {
                          update.mutate({ id: k.id, enabled: v }, {
                            onError: (e) => toast.bad((e as Error).message),
                          })
                        }}
                        label={k.enabled ? '启用' : '停用'}
                      />
                      <div className="min-w-0">
                        <div className="truncate text-[13px] font-medium text-[var(--ink)]">
                          {k.label || '(无标签)'}
                        </div>
                        <div className="text-[11px] text-[var(--ink-3)]">
                          {new Date(k.createdAt).toLocaleDateString('zh-CN')} 创建
                        </div>
                      </div>
                      <div className="ml-auto flex items-center gap-1.5">
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => setRevealedId(revealed ? null : k.id)}
                        >
                          {revealed ? '隐藏' : '显示完整'}
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          disabled={rotate.isPending}
                          title="旧 Key 立即失效 —— 那台设备要换新的"
                          onClick={() => {
                            if (
                              confirm(
                                `轮换「${k.label || '这把 Key'}」？旧 Key 立即失效，那台设备要换新的。`,
                              )
                            ) {
                              rotate.mutate(k.id, {
                                onSuccess: async (res) => {
                                  // ⚠️ 新 Key 直接进剪贴板 —— 不弹提示块。
                                  const ok = await copyToClipboard(res.value)
                                  toast.ok(ok ? '已轮换 —— 新 Key 已复制到剪贴板' : '已轮换')
                                },
                                onError: (e) => toast.bad((e as Error).message),
                              })
                            }
                          }}
                        >
                          <RotateCw size={13} />
                          轮换
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          title="删除后那台设备立刻调不了"
                          onClick={() => {
                            if (
                              confirm(
                                `删除「${k.label || '这把 Key'}」？那台设备会立刻失去调用权。`,
                              )
                            ) {
                              remove.mutate(k.id, {
                                onError: (e) => toast.bad((e as Error).message),
                              })
                            }
                          }}
                        >
                          <Trash2 size={13} />
                        </Button>
                      </div>
                    </div>

                    {/* 行 2：三个档位按钮（网关级锁定时禁用） */}
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-[11.5px] text-[var(--ink-3)]">档位</span>
                      <div className="flex gap-1.5">
                        {KEY_TIERS.map((t) => {
                          const active = k.tier === t.value
                          return (
                            <button
                              key={t.value}
                              onClick={() => {
                                if (locked) {
                                  toast.bad(
                                    '网关级限制生效中 —— 单 Key 档位被强制跟随，先到上面切回 off',
                                  )
                                  return
                                }
                                update.mutate(
                                  { id: k.id, tier: t.value },
                                  { onError: (e) => toast.bad((e as Error).message) },
                                )
                              }}
                              aria-pressed={active}
                              disabled={locked}
                              title={
                                locked
                                  ? '网关级限制生效中 —— 被锁定'
                                  : t.value === 'light'
                                    ? '10 次/分钟、2 并发'
                                    : t.value === 'standard'
                                      ? '60 次/分钟、6 并发'
                                      : '不限量'
                              }
                              className={cn(
                                'fmh-touchable fmh-hoverable rounded-[var(--r-sm)] border px-3 py-1 text-[12px] transition-colors duration-150',
                                locked
                                  ? 'cursor-not-allowed opacity-50'
                                  : '',
                                active
                                  ? 'border-[var(--accent)] bg-[var(--accent-soft)] font-medium text-[var(--accent)]'
                                  : 'border-[var(--line)] text-[var(--ink-2)] hover:bg-[var(--surface-hover)]',
                              )}
                            >
                              {t.label}
                            </button>
                          )
                        })}
                      </div>
                      {locked ? (
                        <span className="inline-flex items-center gap-1 text-[11px] text-[var(--ink-3)]">
                          <Lock size={11} />
                          强制跟随网关（{gatewayTier}）
                        </span>
                      ) : null}
                    </div>

                    {/* 行 3：Key（指纹/明文 + 复制） */}
                    <CopyField
                      value={revealed ? fullValue : k.hint}
                      masked
                      allowReveal={false}
                      // ⚠️ **受控 revealed** —— 组件靠它判断
                      //   "现在显示的是指纹还是完整值"，
                      //   打码时点复制才会走"现取完整值"的路。
                      revealed={revealed}
                      getFullValue={
                        async () => {
                          // ⚠️ 打码时点复制 —— 现取完整值，不进 DOM。
                          const res = await fetch('/api/keys?reveal=1', {
                            credentials: 'include',
                          })
                          const body = await res.json()
                          const found = (body.keys ?? []).find(
                            (x: { id: string }) => x.id === k.id,
                          )
                          if (!res.ok || !found?.value) {
                            throw new Error('取完整值失败')
                          }
                          return found.value as string
                        }
                      }
                    />
                  </div>
                )
              })
            )}
          </CardBody>
        </Card>

        <Card>
          <CardHeader title="怎么配到设备上" />
          <CardBody className="space-y-2 text-[12.5px] leading-[19px] text-[var(--ink-2)]">
            <p>1. 新建一把 Key，标签写设备名。</p>
            <p>2. 复制完整 Key，贴到那台设备的 <code className="mono">apiKey</code> 配置里。</p>
            <p>3. 档位管的是这台设备的调用频率 —— 网关级切到非 off 时全部锁定跟随。</p>
            <p className="text-[var(--ink-3)]">
              停用 = 那台设备立刻 401，其他设备不受影响；轮换 = 旧 Key 失效并给新的。
            </p>
          </CardBody>
        </Card>
      </div>
    </>
  )
}
