import { useState } from 'react'
import { Check, KeyRound, Lock, Pencil, Plus, RotateCw, Trash2, X } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import {
  Button,
  Card,
  CardBody,
  CardHeader,
  Field,
  Input,
  PasswordInput,
  Switch,
  cn,
} from '../components/ui'
import { CopyField } from '../components/common'
import { copyToClipboard } from '../lib/clipboard'
import { useToast } from '../components/toast'
import {
  useCreateCustomKey,
  useCreateKey,
  useCustomKeys,
  useDeleteCustomKey,
  useDeleteKey,
  useDiscoverCustomModels,
  useGatewayTier,
  useKeys,
  useRotateKeyById,
  useUpdateCustomKey,
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

/**
 * 标签的归一化形式 —— 与后端一致：去首尾空白 + 忽略大小写。
 *
 * ⚠️ 空标签归一为 `''`，不参与重名判断。
 */
function normLabel(label: string): string {
  return label.trim().toLowerCase()
}

/** 标签是否已被列表里**其他** Key 占用（空标签不算）。 */
function labelConflict(
  label: string,
  keys: Array<{ id: string; label: string }>,
  exceptId = '',
): string {
  const n = normLabel(label)
  if (!n) return ''
  if (keys.some((k) => k.id !== exceptId && normLabel(k.label) === n)) {
    return `标签「${label.trim()}」已被其他 Key 使用 —— 换个名字`
  }
  return ''
}

/** 找出列表里已经重复的标签（归一化后的值）。 */
function duplicateLabels(keys: Array<{ label: string }>): Set<string> {
  const seen = new Set<string>()
  const dup = new Set<string>()
  for (const k of keys) {
    const n = normLabel(k.label)
    if (!n) continue
    if (seen.has(n)) dup.add(n)
    else seen.add(n)
  }
  return dup
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
  // 正在改名的 Key（空 = 没有）与其新标签。
  const [editingKeyId, setEditingKeyId] = useState('')
  const [editingKeyLabel, setEditingKeyLabel] = useState('')

  // ── 自定义 OpenAI 兼容 Key（上游端点） ──
  // ⚠️ 与上面「调用 Key」完全不同：这些是**上游凭据**，
  //   不是发给客户端用的 Key。客户端认证**仍然**用上面的 FMH Key。
  const { data: customKeysData } = useCustomKeys()
  const createCustom = useCreateCustomKey()
  const updateCustom = useUpdateCustomKey()
  const deleteCustom = useDeleteCustomKey()
  const discoverCustom = useDiscoverCustomModels()
  const [showCustomForm, setShowCustomForm] = useState(false)
  // 非空 = 正在编辑这条端点（而不是新建）。
  const [editingCustomId, setEditingCustomId] = useState('')
  // 用户填写的上下文长度（空串 = 不填）。
  const [ckContext, setCkContext] = useState('')
  // 拉取到每个模型的上下文上限（用于校验"不能超过模型自身能力"）。
  const [ckContexts, setCkContexts] = useState<Record<string, number>>({})
  const [ckName, setCkName] = useState('')
  const [ckBase, setCkBase] = useState('')
  const [ckKey, setCkKey] = useState('')
  // 手动兜底（列表拉不到时用）。
  const [ckModels, setCkModels] = useState('')
  // 拉取到的模型清单 + 已勾选的子集。
  const [ckModelList, setCkModelList] = useState<string[]>([])
  const [ckPicked, setCkPicked] = useState<string[]>([])
  const [ckQuery, setCkQuery] = useState('')
  const [ckShowManual, setCkShowManual] = useState(false)
  const [ckDiscoverError, setCkDiscoverError] = useState('')

  /** 清空自定义端点表单的临时状态。 */
  function resetCustomForm() {
    setEditingCustomId('')
    setCkName('')
    setCkBase('')
    setCkKey('')
    setCkContext('')
    setCkModels('')
    setCkModelList([])
    setCkPicked([])
    setCkContexts({})
    setCkQuery('')
    setCkShowManual(false)
    setCkDiscoverError('')
  }

  /** 进入编辑：把已保存的端点回填到表单（Key 留空 = 保持原值）。 */
  function startEditCustom(k: {
    id: string
    name: string
    baseURL: string
    models: string[]
    contextLength?: number
  }) {
    setEditingCustomId(k.id)
    setCkName(k.name)
    setCkBase(k.baseURL)
    setCkKey('')
    setCkContext(k.contextLength ? String(k.contextLength) : '')
    setCkModels('')
    setCkModelList(k.models)
    setCkPicked(k.models)
    setCkContexts({})
    setCkQuery('')
    setCkShowManual(false)
    setCkDiscoverError('')
    setShowCustomForm(true)
  }

  /** 从 Base URL 拉取模型清单（不落库，仅用于勾选）。 */
  async function doDiscover() {
    const baseURL = ckBase.trim()
    if (!baseURL) {
      toast.bad('先填写 Base URL')
      return
    }
    setCkDiscoverError('')
    try {
      // ⚠️ 编辑时 Key 留空 —— 带 id 让后端用已保存的 Key 去拉清单。
      const res = await discoverCustom.mutateAsync({
        baseURL,
        apiKey: ckKey.trim(),
        id: editingCustomId || undefined,
      })
      setCkModelList(res.models)
      setCkContexts(res.contexts ?? {})
      // 默认全选 —— 用户拉清单通常就是想接入；一键「清空」可反选。
      setCkPicked(res.models)
      setCkQuery('')
      if (res.models.length === 0) {
        setCkShowManual(true)
        toast.bad('该端点没有返回模型，请手动填写模型名')
      } else {
        toast.ok(`拉到 ${res.models.length} 个模型，请勾选要接入的`)
      }
    } catch (e) {
      setCkModelList([])
      setCkPicked([])
      setCkContexts({})
      setCkShowManual(true)
      setCkDiscoverError((e as Error).message)
    }
  }

  // ⚠️ 勾选模型的上下文上限：只取探测到的那些模型的最小值（这是真正的约束）。
  const ckDetectedLimit = (() => {
    const picked = ckPicked.length > 0 ? ckPicked : ckModelList
    let min = 0
    for (const m of picked) {
      const n = ckContexts[m] ?? 0
      if (n > 0 && (min === 0 || n < min)) min = n
    }
    return min
  })()
  const ckContextNum = ckContext.trim() === '' ? 0 : Number(ckContext)
  const ckContextInvalid = !Number.isFinite(ckContextNum) || ckContextNum < 0
  const ckContextOverflow = ckDetectedLimit > 0 && ckContextNum > ckDetectedLimit

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
  // ⚠️ 后端才是唯一性的最终裁决 —— 这里只是**就地先提示**，
  //   避免用户填完点创建才吃一个 400。
  //   标签必填：空/全空白直接拦住（历史上已有的空标签不在此列）。
  const err =
    (newLabel.trim() ? '' : '标签必填 —— 用量和日志里靠它区分 Key') ||
    labelError(newLabel) ||
    labelConflict(newLabel, keys)
  const dupKeyLabels = duplicateLabels(keys)
  const renameErr = editingKeyId
    ? (editingKeyLabel.trim() ? '' : '标签不能为空') ||
      labelError(editingKeyLabel) ||
      labelConflict(editingKeyLabel, keys, editingKeyId)
    : ''

  async function doRename(id: string) {
    if (renameErr) return
    const label = editingKeyLabel.trim()
    if (!label) {
      toast.bad('标签不能为空')
      return
    }
    try {
      await update.mutateAsync({ id, label })
      setEditingKeyId('')
      setEditingKeyLabel('')
      toast.ok('标签已更新')
    } catch (e) {
      toast.bad((e as Error).message)
    }
  }

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

  async function doSaveCustom() {
    const name = ckName.trim()
    const baseURL = ckBase.trim()
    if (!name || !baseURL) {
      toast.bad('名称、Base URL 都要填')
      return
    }
    if (!editingCustomId && !ckKey.trim()) {
      toast.bad('新建端点时 API Key 必填')
      return
    }
    if (ckContextInvalid) {
      toast.bad('上下文长度必须是非负数字')
      return
    }
    if (ckContextOverflow) {
      toast.bad(`上下文长度不能超过模型的 ${ckDetectedLimit}（探测到的上限）`)
      return
    }
    // 勾选的优先；手动填写的作为补充（去重后合并）。
    const manual = ckModels
      .split(/[,，\n]/)
      .map((s) => s.trim())
      .filter(Boolean)
    const models = [...new Set([...ckPicked, ...manual])]
    if (models.length === 0) {
      toast.bad('至少选择一个模型（或手动填写）')
      return
    }
    const contextLength = ckContextNum > 0 ? ckContextNum : undefined
    try {
      if (editingCustomId) {
        const res = await updateCustom.mutateAsync({
          id: editingCustomId,
          name,
          baseURL,
          apiKey: ckKey.trim(),
          models,
          contextLength: contextLength ?? 0,
        })
        toast.ok(`已更新「${name}」—— ${res.models.length} 个模型已同步到模型池`)
      } else {
        const res = await createCustom.mutateAsync({
          name,
          baseURL,
          apiKey: ckKey.trim(),
          models,
          contextLength,
        })
        toast.ok(
          `已添加「${name}」—— ${res.models.length} 个模型已进入模型池（仅手动探测）`,
        )
      }
      resetCustomForm()
      setShowCustomForm(false)
    } catch (e) {
      toast.bad((e as Error).message)
    }
  }

  const ckFiltered = ckModelList.filter((m) => {
    const q = ckQuery.trim().toLowerCase()
    return q === '' || m.toLowerCase().includes(q)
  })

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
                        <div className="flex flex-wrap items-center gap-1.5">
                          <span className="truncate text-[13px] font-medium text-[var(--ink)]">
                            {k.label || '（未命名）'}
                          </span>
                          {/* ⚠️ 历史数据里的空标签**不自动改** —— 只提示补填；
                              补填走下面的铅笔按钮，同样受必填与唯一性校验。 */}
                          {!normLabel(k.label) ? (
                            <span
                              className="shrink-0 rounded-full border border-[var(--warn-line)] bg-[var(--warn-soft)] px-1.5 py-0.5 text-[10px] text-[var(--warn)]"
                              title="这把 Key 还没有标签 —— 用量与日志里会分不清；请点铅笔补一个"
                            >
                              缺少标签，请补填
                            </span>
                          ) : null}
                          {/* ⚠️ 历史数据可能已经有重名 —— **不改用户数据、不自动合并**，
                              只标出来提醒，改名走下面的铅笔按钮（同样受唯一性校验）。 */}
                          {dupKeyLabels.has(normLabel(k.label)) ? (
                            <span
                              className="shrink-0 rounded-full border border-[var(--warn-line)] bg-[var(--warn-soft)] px-1.5 py-0.5 text-[10px] text-[var(--warn)]"
                              title="这些 Key 的标签相同 —— 用量与日志里会分不清；建议改成不同的名字"
                            >
                              标签重复，建议改名
                            </span>
                          ) : null}
                        </div>
                        <div className="text-[11px] text-[var(--ink-3)]">
                          {new Date(k.createdAt).toLocaleDateString('zh-CN')} 创建
                        </div>
                      </div>
                      <div className="ml-auto flex items-center gap-1.5">
                        <Button
                          size="sm"
                          variant="ghost"
                          disabled={update.isPending}
                          title="改标签 —— 不能和其他 Key 重名"
                          onClick={() => {
                            setEditingKeyId(editingKeyId === k.id ? '' : k.id)
                            setEditingKeyLabel(k.label)
                          }}
                        >
                          <Pencil size={13} />
                        </Button>
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

                    {/* 改名（就地编辑）—— 后端同样校验唯一性。 */}
                    {editingKeyId === k.id ? (
                      <div className="flex flex-wrap items-center gap-2 rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface-2)] p-2">
                        <div className="min-w-0 flex-1">
                          <Input
                            value={editingKeyLabel}
                            onChange={(e) => setEditingKeyLabel(e.target.value)}
                            placeholder={`新标签（最多 ${maxLabel} 个字）`}
                            maxLength={24}
                            autoFocus
                          />
                          {renameErr ? (
                            <div className="mt-1 text-[11px] text-[var(--bad)]">{renameErr}</div>
                          ) : null}
                        </div>
                        <Button
                          size="sm"
                          onClick={() => void doRename(k.id)}
                          disabled={!!renameErr || update.isPending}
                        >
                          保存
                        </Button>
                        <Button
                          size="sm"
                          variant="secondary"
                          onClick={() => {
                            setEditingKeyId('')
                            setEditingKeyLabel('')
                          }}
                        >
                          取消
                        </Button>
                      </div>
                    ) : null}

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

        {/* ── 自定义 API Key（上游 OpenAI 兼容端点） ──
            ⚠️ 这是**上游凭据**，不是调用 Key。
            客户端的认证仍然用上面那一条 FMH Key —— 这里加的 Key
            只决定"请求可以发到哪家上游、花谁的额度"。 */}
        <Card>
          <CardHeader
            title="自定义 API Key"
            description="你自己的 OpenAI 兼容端点（Base URL + Key）。模型只进模型池，只支持手动探测。"
            actions={
              <Button size="sm" onClick={() => { resetCustomForm(); setShowCustomForm((v) => !v) }}>
                <Plus size={13} />
                添加端点
              </Button>
            }
          />
          <CardBody className="space-y-3">
            <div className="rounded-[var(--r-sm)] border border-[var(--warn-line)] bg-[var(--warn-soft)] px-3 py-2 text-[12px] leading-[18px] text-[var(--warn)]">
              与 OpenRouter 车道行为一致：<b>不参与自动批量探测</b>（避免消耗你自己的额度），
              只能在模型清单里手动探测；模型进入模型池后可被路由选中。
              客户端调用仍然使用上面的 <b>FMH 调用 Key</b>，认证方式不变。
            </div>

            {showCustomForm ? (
              <div className="space-y-3 rounded-[var(--r-md)] border border-[var(--line)] bg-[var(--surface-2)] p-3.5">
                <div className="flex items-center justify-between">
                  <div className="text-[12.5px] font-medium text-[var(--ink)]">
                    {editingCustomId ? '编辑端点' : '添加端点'}
                  </div>
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => {
                      resetCustomForm()
                      setShowCustomForm(false)
                    }}
                  >
                    <X size={13} />
                  </Button>
                </div>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="名称" hint="给自己看的名字，例如「我的 OpenAI 代理」">
                    <Input
                      value={ckName}
                      onChange={(e) => setCkName(e.target.value)}
                      placeholder="如：自建中转 / 官方"
                      maxLength={40}
                    />
                  </Field>
                  <Field label="Base URL" hint="OpenAI 兼容地址，例如 https://api.openai.com/v1">
                    <Input
                      value={ckBase}
                      onChange={(e) => setCkBase(e.target.value)}
                      placeholder="https://api.example.com/v1"
                    />
                  </Field>
                  <Field
                    label="API Key"
                    hint={
                      editingCustomId
                        ? '留空 = 保持已保存的 Key（加密保存，不会明文回显）'
                        : '这个端点的上游 Key（加密保存，不会明文回显）'
                    }
                  >
                    <PasswordInput
                      value={ckKey}
                      onChange={(e) => setCkKey(e.target.value)}
                      placeholder={editingCustomId ? '留空保持不变' : 'sk-…'}
                    />
                  </Field>
                  <Field
                    label="上下文长度（token）"
                    hint="会作为 context_window 展示给 agent；可不填"
                  >
                    <Input
                      value={ckContext}
                      onChange={(e) => setCkContext(e.target.value)}
                      placeholder="如 128000"
                      inputMode="numeric"
                    />
                    {ckContextInvalid ? (
                      <div className="mt-1 text-[11px] text-[var(--warn)]">
                        请填写非负整数
                      </div>
                    ) : ckContextOverflow ? (
                      <div className="mt-1 text-[11px] text-[var(--warn)]">
                        超过探测到的模型上限 {ckDetectedLimit}，请调小
                      </div>
                    ) : ckDetectedLimit > 0 && ckContextNum > 0 ? (
                      <div className="mt-1 text-[11px] text-[var(--ok)]">
                        探测到的模型上限 {ckDetectedLimit}，填写值可用
                      </div>
                    ) : ckContextNum > 0 ? (
                      <div className="mt-1 text-[11px] text-[var(--ink-3)]">
                        未探测到模型上限 —— 会按填写的值展示给 agent
                      </div>
                    ) : null}
                  </Field>
                  <div className="space-y-2 rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface)] p-3 sm:col-span-2">
                    <div className="flex flex-wrap items-start justify-between gap-2">
                      <div className="min-w-0">
                        <div className="text-[12px] font-medium text-[var(--ink)]">接入模型</div>
                        <div className="text-[11px] leading-4 text-[var(--ink-3)]">
                          填好 Base URL 和 Key 后拉取清单，勾选要接入的模型；拉不到时也可以手动填写。
                        </div>
                      </div>
                      <div className="flex shrink-0 gap-2">
                        <Button
                          size="sm"
                          variant="secondary"
                          onClick={doDiscover}
                          disabled={discoverCustom.isPending}
                        >
                          <RotateCw size={13} />
                          {discoverCustom.isPending ? '拉取中…' : '拉取模型'}
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => setCkShowManual((v) => !v)}
                        >
                          {ckShowManual ? '收起手动填写' : '手动填写'}
                        </Button>
                      </div>
                    </div>

                    {ckDiscoverError ? (
                      <div className="rounded-[var(--r-sm)] border border-[var(--warn-line)] bg-[var(--warn-soft)] px-2.5 py-2 text-[11.5px] leading-4 text-[var(--warn)]">
                        {ckDiscoverError}
                      </div>
                    ) : null}

                    {ckModelList.length > 0 ? (
                      <div className="space-y-2">
                        <Input
                          value={ckQuery}
                          onChange={(e) => setCkQuery(e.target.value)}
                          placeholder="搜索模型名…"
                        />
                        <div className="flex flex-wrap items-center justify-between gap-2 text-[11.5px] text-[var(--ink-3)]">
                          <span>
                            已选 {ckPicked.length} / {ckModelList.length}
                          </span>
                          <div className="flex gap-2">
                            <button
                              type="button"
                              className="hover:text-[var(--ink)]"
                              onClick={() => setCkPicked(ckModelList)}
                            >
                              全选
                            </button>
                            <button
                              type="button"
                              className="hover:text-[var(--ink)]"
                              onClick={() => setCkPicked([])}
                            >
                              清空
                            </button>
                          </div>
                        </div>
                        <div className="max-h-56 overflow-y-auto rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface-2)] p-1">
                          {ckFiltered.length === 0 ? (
                            <div className="px-2 py-4 text-center text-[11.5px] text-[var(--ink-3)]">
                              没有匹配的模型
                            </div>
                          ) : (
                            ckFiltered.map((m) => (
                              <label
                                key={m}
                                className="flex cursor-pointer items-center gap-2 rounded-[4px] px-2 py-1.5 hover:bg-[var(--surface)]"
                              >
                                <input
                                  type="checkbox"
                                  className="size-3.5 shrink-0"
                                  checked={ckPicked.includes(m)}
                                  onChange={(e) =>
                                    setCkPicked((prev) =>
                                      e.target.checked ? [...prev, m] : prev.filter((x) => x !== m),
                                    )
                                  }
                                />
                                <span
                                  className="mono min-w-0 truncate text-[12px] text-[var(--ink)]"
                                  title={m}
                                >
                                  {m}
                                </span>
                              </label>
                            ))
                          )}
                        </div>
                      </div>
                    ) : null}

                    {ckShowManual ? (
                      <Field
                        label="手动填写模型"
                        hint="逗号或换行分隔；会与勾选列表合并（自动去重）"
                      >
                        <Input
                          value={ckModels}
                          onChange={(e) => setCkModels(e.target.value)}
                          placeholder="gpt-4o-mini, gpt-4o"
                        />
                      </Field>
                    ) : null}

                    {ckModelList.length === 0 && !ckShowManual ? (
                      <div className="text-[11px] text-[var(--ink-3)]">
                        不勾选、也不手填时，添加会自动请求一次 /v1/models。
                      </div>
                    ) : null}
                  </div>
                </div>
                <div className="flex justify-end gap-2">
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => {
                      resetCustomForm()
                      setShowCustomForm(false)
                    }}
                  >
                    取消
                  </Button>
                  <Button
                    size="sm"
                    onClick={doSaveCustom}
                    disabled={
                      createCustom.isPending ||
                      updateCustom.isPending ||
                      ckContextInvalid ||
                      ckContextOverflow
                    }
                  >
                    {editingCustomId ? <Check size={13} /> : <KeyRound size={13} />}
                    {createCustom.isPending || updateCustom.isPending
                      ? '保存中…'
                      : editingCustomId
                        ? '保存修改'
                        : '添加'}
                  </Button>
                </div>
              </div>
            ) : null}

            {(customKeysData?.keys ?? []).length === 0 ? (
              <div className="py-5 text-center text-[12.5px] text-[var(--ink-3)]">
                还没有自定义端点 —— 添加后模型会出现在模型清单里（车道：自定义）。
              </div>
            ) : (
              (customKeysData?.keys ?? []).map((k) => (
                <div
                  key={k.id}
                  className="space-y-2 rounded-[var(--r-md)] border border-[var(--line)] bg-[var(--surface)] p-3.5"
                >
                  <div className="flex flex-wrap items-center gap-2">
                    <div className="min-w-0">
                      <div className="truncate text-[13px] font-medium text-[var(--ink)]">
                        {k.name || '(未命名)'}
                      </div>
                      <div className="truncate text-[11px] text-[var(--ink-3)]">
                        {k.baseURL}
                      </div>
                    </div>
                    <div className="ml-auto flex items-center gap-1.5">
                      <span className="text-[11.5px] text-[var(--ink-3)]">
                        {k.models.length} 个模型
                        {k.contextLength ? ` · 上下文 ${k.contextLength}` : ''} · 仅手动探测
                      </span>
                      <Button
                        size="sm"
                        variant="ghost"
                        title="编辑名称 / Base URL / Key / 模型 / 上下文长度"
                        onClick={() => startEditCustom(k)}
                      >
                        <Pencil size={13} />
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        title="删除后这个端点的模型会立刻从模型池移除"
                        onClick={() => {
                          if (
                            confirm(
                              `删除「${k.name || '这个端点'}」？它的模型会立刻从模型池移除。`,
                            )
                          ) {
                            deleteCustom.mutate(k.id, {
                              onSuccess: () => toast.ok(`已删除「${k.name || '端点'}」`),
                              onError: (e) => toast.bad((e as Error).message),
                            })
                          }
                        }}
                      >
                        <Trash2 size={13} />
                      </Button>
                    </div>
                  </div>
                  <div className="flex flex-wrap items-center gap-2 text-[11.5px] text-[var(--ink-3)]">
                    <span className="mono rounded-[4px] border border-[var(--line)] bg-[var(--surface-2)] px-1.5 py-0.5">
                      {k.hint || 'key 不可用'}
                    </span>
                    <span className="min-w-0 truncate">
                      模型：{k.models.slice(0, 4).join('、')}
                      {k.models.length > 4 ? ` 等 ${k.models.length} 个` : ''}
                    </span>
                  </div>
                </div>
              ))
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
