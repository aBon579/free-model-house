import { useEffect, useId, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowDown, ArrowUp, Download, ExternalLink, Gift, KeyRound, Pencil, RefreshCw, Trash2, Upload, Wallet, X } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import { Badge, Button, Card, CardBody, CardHeader, Empty, ErrorNote, Field, Input, Loading, Notice, Select, Spinner, Stat, Switch, cn } from '../components/ui'
import { useToast } from '../components/toast'
import { creditApi, useCredit, type Account, type AccountAction, type AccountUpdate, type ActionResult, type CreditDate, type Credits, type ImportAccount, type Login, type MachineIdentity, type MachineStatus, type Model, type Provider, type ProviderId } from '../api/credit'

const channels: { id: ProviderId; name: string; description: string }[] = [
  { id: 'qoder', name: 'Qoder', description: '国际版账户与资源包' },
  { id: 'qodercn', name: 'Qoder 中国版', description: '中国版账户与每日领取' },
  { id: 'trae', name: 'TRAE', description: '浏览器授权 · 支持远端 NAS' },
]

function message(error: unknown) {
  return error instanceof Error ? error.message : '操作失败，请重试'
}

function timestamp(value?: CreditDate): number {
  if (value === undefined || value === null || value === '') return NaN
  if (typeof value === 'number') return value > 0 && value < 1e12 ? value * 1000 : value
  if (/^\d+(\.\d+)?$/.test(value)) return timestamp(Number(value))
  return Date.parse(value)
}

function date(value?: CreditDate) {
  const ms = timestamp(value)
  return Number.isFinite(ms) && ms > 0 ? new Date(ms).toLocaleString() : '未知'
}

function expired(value?: CreditDate) {
  const ms = timestamp(value)
  return ms > 0 && ms <= Date.now()
}

function quantity(value?: number) {
  return typeof value === 'number' && Number.isFinite(value) ? value.toLocaleString(undefined, { maximumFractionDigits: 6 }) : '未知'
}

// 每个渠道"上次为这一屏打过余额请求"的时刻，只在当前浏览器会话里有效。
const balanceFreshAt = new Map<string, number>()

// 领取分档的中文名。这些档位只影响自动领取，跟推理车道的 cooldownUntil 是两套开关。
const claimHoldNames: Record<string, string> = { credential: '凭据失效', plan: '套餐上限', server: '上游异常', rate: '领取过快' }

function claimHoldName(kind?: string) {
  return (kind && claimHoldNames[kind]) || kind || '无'
}

function claimHoldText(account: Account) {
  if (!account.claimCooldownKind) return ''
  if (account.claimCooldownKind === 'credential' || (account.claimCooldownUntil ?? 0) < 0) {
    return `已暂停（${claimHoldName(account.claimCooldownKind)}，需重新登录、刷新凭据或重置状态）`
  }
  const until = new Date(account.claimCooldownUntil ?? 0)
  return `暂停至 ${until.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}（${claimHoldName(account.claimCooldownKind)}）`
}

// 只有 Qoder / Qoder 中国版的每日领取要那对设备标头，TRAE 不要 —— 所以设备身份这一栏
// 只挂在 Qoder 两个标签下，别让用户以为 TRAE 也缺东西。
function needsMachine(provider: ProviderId) {
  return provider === 'qoder' || provider === 'qodercn'
}

/**
 * 从一段文本里解出成对的设备身份，三种来源都认：
 * PC 脚本生成的身份文件（machineIdentity 块）、FMH 完整备份、旧的逐账户 credential。
 * 解不出来返回 null（边输入边判断用，不能抛错）。
 */
function machineFromText(raw: string): MachineIdentity | null {
  let doc: unknown
  try {
    // PowerShell 的 Set-Content -Encoding UTF8 会在文件头留一个 BOM，JSON.parse 不认它。
    doc = JSON.parse(raw.charCodeAt(0) === 0xFEFF ? raw.slice(1) : raw)
  } catch {
    return null
  }
  if (!isObject(doc)) return null
  const source = isObject(doc.machineIdentity) ? doc.machineIdentity : isObject(doc.credential) ? doc.credential : doc
  const machineToken = typeof source.machineToken === 'string' ? source.machineToken.trim() : ''
  const machineType = typeof source.machineType === 'string' ? source.machineType.trim() : ''
  return machineToken && machineType ? { machineToken, machineType } : null
}

/**
 * 把一次签到的一堆逐项结果说成一句人话。三种"没领到"是不同的事，
 * 混成一句"签到完成"就会让人以为额度已经到账（上游余额真没动）。
 */
function claimSummary(items: ActionResult['results'], machineMissing: boolean): { ok: boolean; text: string } {
  const list = items ?? []
  const got = list.filter(item => item.claimed)
  const failed = list.filter(item => !item.claimed && !item.alreadyClaimed && item.id)
  const already = list.filter(item => item.alreadyClaimed)
  if (got.length) return { ok: !failed.length, text: failed.length ? `领到 ${got.length} 项，另有 ${failed.length} 项没成：${failed[0].message}` : `领到 ${got.length} 项，余额已刷新` }
  if (failed.length) return { ok: false, text: `没领到：${failed[0].message}` }
  if (already.length) return { ok: true, text: '上游记着这一项今天已经领过了，所以余额没变 —— 如果不是这台网关领的，那就是你在电脑上的 Qoder 客户端里领的。' }
  if (machineMissing) return { ok: false, text: '这台网关还没有 Qoder 设备身份：上游因此连「每日签到」都不会下发，现在点多少次都是空清单。先在上方「设备身份」导入。' }
  return { ok: false, text: '上游这个账号当前没有可领取的活动 —— 这不代表今天领过了（今日那份可能已在客户端领掉，或这份设备身份已经换新）。可点「查询余额」再看一次清单。' }
}

function isObject(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value)
}

function parseJSON(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    throw new Error('JSON 格式无效，请检查引号、逗号和括号；不会回显凭据内容。')
  }
}

function useOperation() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const lock = useRef(false)
  async function run(task: () => Promise<void>) {
    if (lock.current) return
    lock.current = true
    setBusy(true)
    setError('')
    try {
      await task()
    } catch (e) {
      setError(message(e))
    } finally {
      lock.current = false
      setBusy(false)
    }
  }
  return { busy, error, run }
}

function Modal({ title, onClose, busy = false, children }: { title: string; onClose: () => void; busy?: boolean; children: ReactNode }) {
  const titleId = useId()
  const root = useRef<HTMLDivElement>(null)
  const close = useRef(onClose)
  const locked = useRef(busy)
  close.current = onClose
  locked.current = busy
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null
    root.current?.focus()
    function keydown(event: KeyboardEvent) {
      if (event.key === 'Escape' && !locked.current) {
        event.stopPropagation()
        close.current()
      }
      if (event.key !== 'Tab' || !root.current?.contains(event.target as Node)) return
      const focusable = Array.from(root.current.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), textarea:not(:disabled), select:not(:disabled), a[href], [tabindex="0"]')).filter(el => el.getClientRects().length > 0)
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (!first) { event.preventDefault(); return }
      if (event.shiftKey && (document.activeElement === first || document.activeElement === root.current)) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && (document.activeElement === last || document.activeElement === root.current)) {
        event.preventDefault()
        first.focus()
      }
    }
    root.current?.addEventListener('keydown', keydown)
    const element = root.current
    return () => {
      element?.removeEventListener('keydown', keydown)
      previous?.focus()
    }
  }, [])
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/45 p-4" onMouseDown={e => { if (e.target === e.currentTarget && !busy) onClose() }}>
      <div ref={root} role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1} className="max-h-[90dvh] w-full max-w-2xl overflow-y-auto rounded-[var(--r-md)] outline-none">
        <Card>
          <CardHeader title={<span id={titleId}>{title}</span>} actions={<Button size="sm" variant="ghost" disabled={busy} onClick={onClose} aria-label="关闭弹窗"><X size={16} /></Button>} />
          <CardBody className="space-y-3 text-[13px] text-[var(--ink-2)]">{children}</CardBody>
        </Card>
      </div>
    </div>
  )
}

function SecretTextarea({ value, onChange, label, placeholder, disabled }: { value: string; onChange: (value: string) => void; label: string; placeholder?: string; disabled?: boolean }) {
  return <textarea value={value} onChange={e => onChange(e.target.value)} aria-label={label} placeholder={placeholder} disabled={disabled} autoComplete="off" autoCorrect="off" autoCapitalize="off" spellCheck={false} rows={5} style={{ WebkitTextSecurity: 'disc' } as CSSProperties} className="mono w-full resize-y rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface)] p-3 text-[13px] text-[var(--ink)] placeholder:text-[var(--ink-3)] focus:border-[var(--accent)] focus:outline-none disabled:opacity-55" />
}

async function readJSONFile(file: File) {
  if (file.size > 5 * 1024 * 1024) throw new Error('文件超过 5 MB，请分批导入。')
  return file.text()
}

/** 敏感 JSON 的两种入口：粘贴（遮蔽显示）与选文件。两者都只是填进输入框，点了确认才发出去。 */
function SecretIntake({ busy, value, onChange, fileName, onFile, hint, secretLabel, fileLabel }: { busy: boolean; value: string; onChange: (value: string) => void; fileName: string; onFile: (file: File) => void; hint: string; secretLabel: string; fileLabel: string }) {
  return <>
    <Field label="敏感 JSON（输入已遮蔽）" hint={hint}>
      <SecretTextarea value={value} onChange={onChange} label={secretLabel} disabled={busy} placeholder="在此粘贴 JSON，内容不回显" />
    </Field>
    <label className="flex flex-wrap items-center gap-2 text-[12px]">
      <Upload size={14} /><span>从 JSON 文件读取</span>
      <input type="file" accept=".json,application/json" aria-label={fileLabel} disabled={busy} className="max-w-full text-[12px]" onChange={e => {
        const file = e.target.files?.[0]
        e.target.value = ''
        if (file) onFile(file)
      }} />
    </label>
    {fileName && <div className="text-[12px] text-[var(--ink-3)]">已读取：{fileName}，提交前不会上传。</div>}
  </>
}

function CredentialDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [secret, setSecret] = useState('')
  const [fileName, setFileName] = useState('')
  const op = useOperation()
  async function submit() {
    await op.run(async () => {
      const parsed = parseJSON(secret)
      const doc = isObject(parsed) ? parsed : undefined
      const entries = Array.isArray(parsed) ? parsed : doc?.accounts
      if (doc && doc.format !== undefined && (doc.format !== 'fmh-credit' || doc.version !== 1)) throw new Error('不支持的备份格式或版本，仅支持 fmh-credit v1。')
      if (!Array.isArray(entries) || entries.length === 0) throw new Error('请提供非空 accounts 数组，或完整的 fmh-credit 备份。')
      if (entries.some(entry => !isObject(entry) || !channels.some(c => c.id === entry.provider) || typeof entry.name !== 'string' || !entry.name.trim() || !isObject(entry.credential))) throw new Error('每个账户必须包含有效 provider、name 和 credential 对象。')
      // 完整备份里那一份设备身份跟着一起回灌，换机器重建时不必再从 PC 送一次。
      const identity = doc && isObject(doc.machineIdentity) ? doc.machineIdentity : undefined
      const result = await creditApi.import({ accounts: entries as ImportAccount[], ...(identity ? { machineIdentity: identity } : {}) })
      setSecret('')
      onDone()
      onClose()
      toast.ok(`已导入 ${result.imported} 个账户`)
    })
  }
  const toast = useToast()
  return (
    <Modal title="导入账户 / 备份" onClose={onClose} busy={op.busy}>
      <Notice>只使用你授权提供的凭据，不读取桌面真实账户。输入仅保存在当前弹窗内存中，关闭即清除，不写入 localStorage。</Notice>
      <Notice tone="warn">Qoder / Qoder 中国版的每日领取要设备身份：账户 credential 里成对的 machineToken 和 machineType 优先，没带就用「设备身份」里全局那一份。备份里若带 machineIdentity，导入时会一并覆盖全局那份。不要伪造设备信息。</Notice>
      <SecretIntake
        busy={op.busy}
        value={secret}
        onChange={setSecret}
        fileName={fileName}
        onFile={file => void op.run(async () => { setSecret(await readJSONFile(file)); setFileName(file.name) })}
        hint='格式：{"accounts":[{"provider":"qoder","name":"个人账户","credential":{…}}]}，也可粘贴导出的完整备份。'
        secretLabel="敏感凭据 JSON"
        fileLabel="导入凭据 JSON 文件"
      />
      {op.error && <ErrorNote>{op.error}</ErrorNote>}
      <div className="flex justify-end gap-2"><Button disabled={op.busy} onClick={onClose}>取消</Button><Button variant="primary" disabled={op.busy || !secret.trim()} onClick={() => void submit()}>{op.busy && <Spinner />}确认导入</Button></div>
    </Modal>
  )
}

function MachineDialog({ machine, onClose, onDone }: { machine?: MachineStatus; onClose: () => void; onDone: () => void }) {
  const [text, setText] = useState('')
  const [fileName, setFileName] = useState('')
  const op = useOperation()
  const toast = useToast()
  const found = text.trim() ? machineFromText(text) : null
  async function submit() {
    await op.run(async () => {
      // found 为空时按钮是禁用的；这一句是给"绕过按钮"的调用者兜底，顺便把话说清楚。
      if (!found) throw new Error('没读出一对 machineToken / machineType：请选 PC 脚本生成的 qoder-machine-identity.json，或粘它 -PrintJson 打出的那段 JSON。')
      const result = await creditApi.machine(found)
      setText('')
      onDone()
      onClose()
      toast.ok(`设备身份已写入：${result.machine.hint || '指纹未知'}`)
    })
  }
  return (
    <Modal title="导入 Qoder 设备身份" onClose={onClose} busy={op.busy}>
      <Notice>每日领取要的一对请求头（Cosy-MachineToken / Cosy-MachineType）。它由 Qoder 客户端在<b>你这台电脑</b>上算出来，和登录哪个账号无关；网关跑在 NAS 上没有客户端，只能由电脑送一份过来，全局存一份共用。</Notice>
      <Notice tone="warn">原值只进不出：这个接口发给你自己的网关这一次，页面不显示也不保存它（账户快照每 30 秒轮询一次，带上原值等于把凭据挂在列表里）。不要伪造这两个值。</Notice>
      <Notice>怎么拿：在装有 Qoder 客户端的电脑上运行 <span className="mono">tools/qoder-machine-identity.ps1</span>，它默认在<b>桌面</b>覆盖生成 <span className="mono">qoder-machine-identity.json</span> —— 用下面的「从 JSON 文件读取」选它就行，原值不经网络（所以要在这台电脑自己的浏览器里打开 FMH）。身份会随客户端升级换新，只导刚跑出来的那一份。</Notice>
      <SecretIntake
        busy={op.busy}
        value={text}
        onChange={setText}
        fileName={fileName}
        onFile={file => void op.run(async () => { setText(await readJSONFile(file)); setFileName(file.name) })}
        hint='认这三种：PC 脚本生成的身份文件、完整备份（含 machineIdentity 块）、带这两个字段的旧 credential。也可手工粘 {"machineToken":"…","machineType":"…"}。'
        secretLabel="设备身份 JSON"
        fileLabel="设备身份 JSON 文件"
      />
      {!!text.trim() && <div className={cn('text-[12px]', found ? 'text-[var(--ok)]' : 'text-[var(--warn)]')}>{found ? '已识别出一对设备身份，可以保存。' : '还没读出一对 machineToken / machineType —— 两个都要有，只给一个上游不认。'}</div>}
      {machine?.configured && <Notice tone="warn">当前已有 {machine.hint || '指纹未知'}（{date(machine.updatedAt)}），保存会用新的一份覆盖它。</Notice>}
      {op.error && <ErrorNote>{op.error}</ErrorNote>}
      <div className="flex justify-end gap-2"><Button disabled={op.busy} onClick={onClose}>取消</Button><Button variant="primary" disabled={op.busy || !found} onClick={() => void submit()}>{op.busy && <Spinner />}{machine?.configured ? '覆盖为新身份' : '保存设备身份'}</Button></div>
    </Modal>
  )
}

function AccountSettings({ account, models, onClose, onSave }: { account: Account; models: Model[]; onClose: () => void; onSave: (body: AccountUpdate) => Promise<void> }) {
  const [name, setName] = useState(account.name)
  const [disabled, setDisabled] = useState(account.disabledModels)
  const [effort, setEffort] = useState(account.effort)
  const [maxMode, setMaxMode] = useState(account.maxMode)
  const [autoClaimAt, setAutoClaimAt] = useState(account.autoClaimAt ?? '')
  // Qoder 的每日领取活动原文写着「每日 10:00（UTC+8）刷新」，后端在保存时就拒绝更早的值，
  // 这里提前把下限写进提示，免得用户点了保存才知道白填。
  const claimFloor = account.provider === 'qoder' || account.provider === 'qodercn' ? '10:00' : ''
  const op = useOperation()
  const efforts = [...new Set([account.effort, ...models.flatMap(m => m.efforts ?? [])].filter(Boolean))]
  const missing = disabled.filter(id => !models.some(m => m.id === id))
  const supportsMax = models.some(m => m.maxMode)
  return (
    <Modal title="账户与模型设置" onClose={onClose} busy={op.busy}>
      <Field label="账户名称"><Input value={name} aria-label="重命名账户" onChange={e => setName(e.target.value)} maxLength={120} disabled={op.busy} /></Field>
      <Field label="禁用模型（可多选）" hint="勾选表示禁用；未勾选的模型可参与路由。未知能力不推测支持。">
        <div className="flex flex-wrap gap-2 pb-2"><Button size="sm" disabled={op.busy || !models.length} onClick={() => setDisabled([...new Set([...disabled, ...models.map(m => m.id)])])}>全部禁用</Button><Button size="sm" disabled={op.busy || !disabled.length} onClick={() => setDisabled([])}>全部启用</Button></div>
        <div className="max-h-64 space-y-2 overflow-y-auto rounded-[var(--r-sm)] border border-[var(--line)] p-3">
          {models.length === 0 && <p>模型列表未知，请先刷新模型。</p>}
          {models.map(model => <label key={model.id} className="flex items-start gap-2"><input type="checkbox" checked={disabled.includes(model.id)} disabled={op.busy} onChange={e => setDisabled(values => e.target.checked ? [...values, model.id] : values.filter(id => id !== model.id))} className="mt-1 accent-[var(--accent)]" /><span className="min-w-0"><span className="break-all font-medium">{model.name || model.id}</span><span className="block break-all text-[11.5px] text-[var(--ink-3)]">{model.id} · 上下文 {quantity(model.contextWindow)} · {model.vision ? '视觉' : '无视觉'} · {model.reasoning ? '推理' : '标准'}{model.multiplier !== undefined ? ` · ${quantity(model.multiplier)}×` : ''}{model.maxMode ? ' · Max Mode' : ''}{model.efforts?.length ? ` · effort: ${model.efforts.join(' / ')}` : ''}</span></span></label>)}
          {missing.map(id => <label key={id} className="flex items-center gap-2 text-[var(--ink-3)]"><input type="checkbox" checked disabled={op.busy} onChange={() => setDisabled(values => values.filter(v => v !== id))} /><span className="break-all">{id}（当前列表外，保留禁用）</span></label>)}
        </div>
      </Field>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="推理强度 effort" hint="按模型支持的档位生效。"><Select className="w-full" value={effort} aria-label="推理强度 effort" disabled={op.busy} onChange={e => setEffort(e.target.value)}><option value="">默认</option>{efforts.map(value => <option key={value} value={value}>{value}</option>)}</Select></Field>
        <Field label="Max Mode" hint={supportsMax ? '可能提高额度消耗，仅支持的模型生效。' : '当前未发现支持此模式的模型。'}><Switch checked={maxMode} onChange={setMaxMode} label="启用 Max Mode" disabled={op.busy || (!supportsMax && !maxMode)} /></Field>
      </div>
      <Field label="自动领取时间（UTC+8）" hint={claimFloor ? `每天到点领一次，失败当天不重试。该渠道 ${claimFloor} 之后才刷新，不能设得更早；留空表示关闭。` : '每天到点领一次，失败当天不重试；留空表示关闭。'}>
        <Input type="time" className="w-full" value={autoClaimAt} aria-label="自动领取时间" disabled={op.busy} onChange={e => setAutoClaimAt(e.target.value)} />
      </Field>
      {op.error && <ErrorNote>{op.error}</ErrorNote>}
      <div className="flex justify-end gap-2"><Button disabled={op.busy} onClick={onClose}>取消</Button><Button variant="primary" disabled={op.busy || !name.trim()} onClick={() => void op.run(async () => { await onSave({ id: account.id, name: name.trim(), disabledModels: disabled, effort, maxMode, autoClaimAt }); onClose() })}>{op.busy && <Spinner />}保存设置</Button></div>
    </Modal>
  )
}

function ConfirmDialog({ title, children, label, onClose, onConfirm, danger = false }: { title: string; children: ReactNode; label: string; onClose: () => void; onConfirm: () => Promise<void>; danger?: boolean }) {
  const op = useOperation()
  return <Modal title={title} onClose={onClose} busy={op.busy}>{children}{op.error && <ErrorNote>{op.error}</ErrorNote>}<div className="flex justify-end gap-2"><Button onClick={onClose} disabled={op.busy}>取消</Button><Button variant={danger ? 'danger' : 'primary'} disabled={op.busy} onClick={() => void op.run(async () => { await onConfirm(); onClose() })}>{op.busy && <Spinner />}{label}</Button></div></Modal>
}

function ExportDialog({ onClose }: { onClose: () => void }) {
  const [confirmed, setConfirmed] = useState(false)
  const op = useOperation()
  return (
    <Modal title="二次确认：导出敏感备份" onClose={onClose} busy={op.busy}>
      <Notice tone="warn">备份包含所有渠道账户的完整凭据，可用于访问你的账户。文件不会加密，请仅保存到可信设备，不要发送给他人。</Notice>
      <label className="flex items-start gap-2"><input type="checkbox" checked={confirmed} onChange={e => setConfirmed(e.target.checked)} disabled={op.busy} className="mt-1" /><span>我确认导出完整敏感凭据，并自行保管下载文件。</span></label>
      {op.error && <ErrorNote>{op.error}</ErrorNote>}
      <div className="flex justify-end gap-2"><Button onClick={onClose} disabled={op.busy}>取消</Button><Button variant="danger" disabled={!confirmed || op.busy} onClick={() => void op.run(async () => {
        const backup = await creditApi.export()
        const blob = new Blob([JSON.stringify(backup, null, 2)], { type: 'application/json' })
        const url = URL.createObjectURL(blob)
        const link = document.createElement('a')
        link.href = url
        link.download = `fmh-credit-${new Date().toISOString().slice(0, 10)}.json`
        document.body.appendChild(link)
        link.click()
        link.remove()
        window.setTimeout(() => URL.revokeObjectURL(url), 30_000)
        onClose()
      })}>{op.busy ? <Spinner /> : <Download size={14} />}确认下载敏感备份</Button></div>
    </Modal>
  )
}

function loginURL(url: string) {
  try {
    const parsed = new URL(url)
    return parsed.protocol === 'https:' ? parsed.href : undefined
  } catch { return undefined }
}

function LoginDialog({ provider, accountId, onClose, onDone }: { provider: Provider; accountId?: string; onClose: () => void; onDone: () => void }) {
  const [mode, setMode] = useState(provider.loginModes[0] ?? '')
  const [login, setLogin] = useState<Login | null>(null)
  const [callback, setCallback] = useState('')
  const [pollError, setPollError] = useState('')
  const [pollVersion, setPollVersion] = useState(0)
  const op = useOperation()
  const active = useRef<Login | null>(null)
  const mounted = useRef(true)
  const done = useRef(onDone)
  done.current = onDone
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      const session = active.current
      active.current = null
      if (session?.state === 'pending') void creditApi.cancelLogin(session.id).catch(() => {})
    }
  }, [])
  useEffect(() => {
    if (!login || login.state !== 'pending') return
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    let stopped = false
    const interval = login.interval > 0 && login.interval < 100 ? login.interval * 1000 : login.interval
    async function poll() {
      if (stopped || !login) return
      if (expired(login.expiresAt)) {
        const ended: Login = { ...login, state: 'error', message: '登录会话已过期，请重新开始。' }
        active.current = ended
        setLogin(ended)
        void creditApi.cancelLogin(login.id).catch(() => {})
        return
      }
      try {
        const next = await creditApi.loginStatus(login.id, controller.signal)
        if (stopped || active.current?.id !== next.id) return
        active.current = next
        setPollError('')
        if (next.state !== 'pending') {
          setLogin(next)
          setCallback('')
          if (next.state === 'success') done.current()
          return
        }
      } catch (e) {
        if (stopped) return
        setPollError(`读取登录状态失败：${message(e)}。可重试查询或取消。`)
        return
      }
      timer = setTimeout(poll, Math.max(1_000, Math.min(interval || 2_000, 30_000)))
    }
    void poll()
    return () => { stopped = true; controller.abort(); clearTimeout(timer) }
  }, [login, pollVersion])
  async function cancel() {
    await op.run(async () => {
      const session = active.current
      if (session?.state === 'pending') await creditApi.cancelLogin(session.id)
      active.current = null
      setCallback('')
      onClose()
    })
  }
  return (
    <Modal title={`${provider.name} 浏览器登录`} onClose={() => void cancel()} busy={op.busy}>
      <Notice>由你在浏览器中完成官方授权。本页只读取后台登录状态，不会读取本机桌面账户。离开本页会尝试取消尚未完成的登录。</Notice>
      {provider.id === 'trae' && <Notice tone="warn">远端 NAS：在你自己的浏览器完成 TRAE 官方登录，复制最终跳转的完整官方回调 URL，粘贴到下方。即使回调地址无法打开，也可从地址栏复制；不要让 NAS 监听服务端 localhost，也不要在这里打开回调链接。</Notice>}
      {!login || login.state !== 'pending' ? <>
        <Field label="登录方式"><Select value={mode} onChange={e => setMode(e.target.value)} disabled={op.busy || !provider.loginModes.length} aria-label="登录方式" className="w-full">{provider.loginModes.length ? provider.loginModes.map(value => <option key={value} value={value}>{value}</option>) : <option value="">此渠道未提供浏览器登录，请导入凭据</option>}</Select></Field>
        <Button variant="primary" disabled={op.busy || !mode} onClick={() => void op.run(async () => {
          const session = await creditApi.login({ provider: provider.id, mode, accountId })
          if (!mounted.current) { if (session.state === 'pending') await creditApi.cancelLogin(session.id); return }
          active.current = session
          setLogin(session)
          setCallback('')
          setPollError('')
          if (session.state === 'success') done.current()
        })}>{op.busy ? <Spinner /> : <ExternalLink size={14} />}{login ? '重新发起登录' : '生成官方登录链接'}</Button>
      </> : <>
        {loginURL(login.url) ? <a href={loginURL(login.url)} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-2 rounded-[var(--r-sm)] border border-[var(--accent-line)] px-3 py-2 font-medium text-[var(--accent)]"><ExternalLink size={14} />打开官方登录页面</a> : <ErrorNote>后台未提供有效的 HTTPS 登录链接，请取消后重试。</ErrorNote>}
        <div className="flex items-center gap-2"><Spinner />等待官方授权 · 会话到期 {date(login.expiresAt)}</div>
        {provider.id === 'trae' && <Field label="TRAE 官方回调 URL（敏感内容，输入已遮蔽）" hint="只提交给当前登录会话；后台负责校验回调来源和 state。">
          <SecretTextarea value={callback} onChange={setCallback} label="TRAE 官方回调 URL" disabled={op.busy} placeholder="粘贴完整回调 URL，不是登录页 URL" />
          <Button className="mt-2" disabled={op.busy || !callback.trim()} onClick={() => void op.run(async () => {
            try { new URL(callback.trim()) } catch { throw new Error('请粘贴完整的官方回调 URL。') }
            await creditApi.completeLogin(login.id, callback.trim())
            setCallback('')
            setPollVersion(v => v + 1)
          })}>{op.busy && <Spinner />}提交回调并继续查询</Button>
        </Field>}
      </>}
      {login && login.state !== 'pending' && <Notice tone={login.state === 'success' ? 'ok' : login.state === 'error' ? 'bad' : 'warn'}>{login.state === 'success' ? '登录成功，账户列表已刷新。' : login.state === 'cancelled' ? '登录已取消。' : '登录失败。'}{login.message}</Notice>}
      {pollError && <ErrorNote>{pollError}<Button className="ml-2" size="sm" disabled={op.busy} onClick={() => setPollVersion(v => v + 1)}>重试查询</Button></ErrorNote>}
      {op.error && <ErrorNote>{op.error}</ErrorNote>}
      <div className="flex justify-end"><Button disabled={op.busy} onClick={() => void cancel()}>{login?.state === 'pending' ? '取消登录并关闭' : '关闭'}</Button></div>
    </Modal>
  )
}

type Balance = Credits['balances'][number]

// 只累加已知值，未知项落到 missing 计数里。绝不按零混进合计：
// 一个「总量 100 · 已用未知」的包按零算，等于凭空造出 100 可用额度。
function sumField(pick: (balance: Balance) => number | undefined) {
  return (items: Balance[]) => {
    const known = items.map(pick).filter((v): v is number => typeof v === 'number' && Number.isFinite(v))
    return { value: known.length ? known.reduce((a, b) => a + b, 0) : undefined, missing: items.length - known.length }
  }
}

const sumTotal = sumField(balance => balance.total)
const sumUsed = sumField(balance => balance.used)
const sumRemaining = sumField(balance => balance.remaining)

function expiryMs(value?: CreditDate) {
  const ms = timestamp(value)
  return Number.isFinite(ms) && ms > 0 ? ms : undefined
}

// 不同单位的余额不能相加（页头就写着「不合并不同单位的余额」），所以先按 unit 分块，
// 块内再按 name 分类。签到奖励这类同名包只差到期日，一个个平铺正是「一堆未知」的观感来源。
function summarize(balances: Balance[]) {
  const units = new Map<string, Map<string, Balance[]>>()
  for (const balance of balances) {
    const unit = balance.unit || '单位未知'
    const categories = units.get(unit) ?? new Map<string, Balance[]>()
    const items = categories.get(balance.name) ?? []
    items.push(balance)
    categories.set(balance.name, items)
    units.set(unit, categories)
  }
  const group = (items: Balance[]) => {
    const expiries = items.map(balance => expiryMs(balance.expiresAt)).filter((ms): ms is number => ms !== undefined)
    return {
      items,
      count: items.length,
      total: sumTotal(items),
      used: sumUsed(items),
      remaining: sumRemaining(items),
      expired: items.filter(balance => expired(balance.expiresAt)).length,
      earliest: expiries.length ? Math.min(...expiries) : undefined,
    }
  }
  return [...units].map(([unit, categories]) => {
    const all = [...categories.values()].flat()
    return { unit, ...group(all), categories: [...categories].map(([name, items]) => ({ name, ...group(items) })) }
  })
}

function Unknown({ count }: { count: number }) {
  return count > 0 ? <span className="text-[11px] font-normal text-[var(--ink-3)]">（{count} 包未知）</span> : null
}

function CreditBalances({ credits, staleError, busy, machineMissing, onClaim }: { credits?: Credits; staleError?: string; busy: boolean; machineMissing?: boolean; onClaim: (campaignId: string) => void }) {
  const units = summarize(credits?.balances ?? [])
  return <div className="space-y-3">
    {staleError && <Notice tone="warn">余额获取失败，当前余额未知；下方如有数据，仅为上次成功快照，不代表当前可用额度。</Notice>}
    {!credits ? <div className="rounded-[var(--r-sm)] border border-dashed border-[var(--line)] p-4 text-[12.5px] text-[var(--ink-3)]">余额与资源包：未知。点击“查询余额”获取，不将未查询或失败当作 0。</div> : <>
      <div className="flex flex-wrap items-center justify-between gap-2 text-[12px] text-[var(--ink-3)]"><span>余额 / 资源包 · {staleError ? '历史快照' : '上次查询'}</span><span>{date(credits.updatedAt)}</span></div>
      {credits.warning && <Notice tone="warn">{credits.warning}</Notice>}
      {!units.length ? <p className="text-[12.5px] text-[var(--ink-3)]">上游没有返回余额明细，余额未知。</p> : units.map(unit => <div key={unit.unit} className="space-y-2">
        <div className="flex flex-wrap items-end justify-between gap-2 rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface-2)] p-3">
          <div><div className="text-[11.5px] text-[var(--ink-3)]">合计剩余 · {unit.unit}</div><div className="text-[22px] font-semibold text-[var(--ink)]">{staleError ? '未知' : quantity(unit.remaining.value)}<Unknown count={unit.remaining.missing} /></div></div>
          <div className="text-[11.5px] text-[var(--ink-3)]">{unit.count} 个额度包 · 总量 {quantity(unit.total.value)}<Unknown count={unit.total.missing} /> · 已用 {quantity(unit.used.value)}<Unknown count={unit.used.missing} />{unit.expired ? ` · 含 ${unit.expired} 个已到期` : ''}</div>
        </div>
        {unit.categories.map(category => <div key={category.name} className="space-y-2 rounded-[var(--r-sm)] border border-[var(--line)] p-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="text-[12.5px] font-medium text-[var(--ink)]">{category.name} <span className="text-[11.5px] font-normal text-[var(--ink-3)]">· {category.count} 个包</span>{category.expired ? <span className="text-[11.5px] font-normal text-[var(--warn)]"> · {category.expired} 个已到期</span> : null}</div>
            <div className="text-[15px] font-semibold text-[var(--ink)]">{staleError ? '未知' : quantity(category.remaining.value)} <span className="text-[11.5px] font-normal text-[var(--ink-3)]">{unit.unit} · 剩余</span></div>
          </div>
          <div className="text-[11.5px] text-[var(--ink-3)]">总量 {quantity(category.total.value)} · 已用 {quantity(category.used.value)} · 最早到期 {date(category.earliest)}{category.remaining.missing ? ` · ${category.remaining.missing} 个包剩余未知，未计入` : ''}</div>
          {category.count > 1 && <div className="space-y-1 border-t border-[var(--line)] pt-2">{category.items.map((balance, index) => <div key={`${balance.name}-${index}`} className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 text-[11.5px] text-[var(--ink-3)]"><span>{date(balance.expiresAt)}{expired(balance.expiresAt) && <Badge tone="warn" className="ml-1.5">已到期</Badge>}{staleError ? ` · 上次剩余 ${quantity(balance.remaining)}` : ''}</span><span>剩余 {quantity(balance.remaining)} · 总量 {quantity(balance.total)} · 已用 {quantity(balance.used)}</span></div>)}</div>}
        </div>)}
      </div>)}
      <div className="space-y-2 border-t border-[var(--line)] pt-3"><div className="text-[12.5px] font-medium">活动与每日领取</div>{!credits.campaigns?.length ? <p className="text-[12px] text-[var(--ink-3)]">{machineMissing ? '清单里没有可领活动，而这台网关还没有 Qoder 设备身份：缺它时上游压根不下发「每日签到」，所以这不代表你的账号没有活动。先在上方导入设备身份，再点一次「查询余额」。' : '未返回可展示活动；可尝试单号签到，以实际返回结果为准。'}</p> : credits.campaigns.map(campaign => <div key={campaign.id} className="flex flex-wrap items-center justify-between gap-2 rounded-[var(--r-sm)] border border-[var(--line)] p-2.5"><div><div className="text-[12.5px]">{campaign.name} · {quantity(campaign.amount)} <span className="text-[var(--ink-3)]">（活动额度，单位以上游为准）</span></div><div className="text-[11.5px] text-[var(--ink-3)]">到期 {date(campaign.expiresAt)}</div></div><Button size="sm" disabled={busy || campaign.claimed || !campaign.claimable || expired(campaign.expiresAt)} onClick={() => onClaim(campaign.id)}>{campaign.claimed ? '已领取' : expired(campaign.expiresAt) ? '已到期' : campaign.claimable ? '领取一次' : '不可领取'}</Button></div>)}</div>
    </>}
  </div>
}

interface Outcome { state: 'running' | 'ok' | 'error'; text: string; result?: ActionResult }
type Dialog = { kind: 'import' | 'export' | 'machine' | 'machineClear' } | { kind: 'edit' | 'delete' | 'reset' | 'test'; account: Account } | { kind: 'login'; provider: Provider; accountId?: string } | null

function TestDialog({ account, models, onClose, onTest }: { account: Account; models: Model[]; onClose: () => void; onTest: (model: string) => Promise<void> }) {
  const available = models.filter(model => !account.disabledModels.includes(model.id))
  const [model, setModel] = useState(available[0]?.id ?? '')
  const [confirmed, setConfirmed] = useState(false)
  const op = useOperation()
  return <Modal title={`手动测试 · ${account.name}`} onClose={onClose} busy={op.busy}><Notice tone="warn">这会向上游发送一次真实模型请求，会消耗账户额度。不会自动测试其他模型，也不会自动重试网络失败。</Notice><Field label="测试模型"><Select value={model} onChange={e => setModel(e.target.value)} className="w-full" aria-label="测试模型" disabled={op.busy}>{available.length ? available.map(item => <option key={item.id} value={item.id}>{item.name || item.id}</option>) : <option value="">没有可用模型，请刷新模型或调整禁用列表</option>}</Select></Field><label className="flex items-center gap-2"><input type="checkbox" checked={confirmed} disabled={op.busy} onChange={e => setConfirmed(e.target.checked)} />我了解本次测试会花费额度</label>{op.error && <ErrorNote>{op.error}</ErrorNote>}<div className="flex justify-end gap-2"><Button onClick={onClose} disabled={op.busy}>取消</Button><Button variant="primary" disabled={!confirmed || !model || op.busy} onClick={() => void op.run(async () => { await onTest(model); onClose() })}>{op.busy && <Spinner />}确认测试一次</Button></div></Modal>
}

export function Credit() {
  const query = useCredit()
  const qc = useQueryClient()
  const toast = useToast()
  const [providerId, setProviderId] = useState<ProviderId>('qoder')
  const [dialog, setDialog] = useState<Dialog>(null)
  const [busyIds, setBusyIds] = useState<Set<string>>(new Set())
  const locks = useRef(new Set<string>())
  const [outcomes, setOutcomes] = useState<Record<string, Outcome>>({})
  const [creditErrors, setCreditErrors] = useState<Record<string, string>>({})
  const [batch, setBatch] = useState(false)
  const batchLock = useRef(false)
  const mounted = useRef(true)
  const data = query.data
  const accounts = data?.accounts ?? []
  const provider = data?.providers.find(p => p.id === providerId)
  const channel = channels.find(c => c.id === providerId)!
  const visible = accounts.filter(account => account.provider === providerId)
  const [, tick] = useState(0)
  useEffect(() => {
    mounted.current = true
    const timer = setInterval(() => tick(value => value + 1), 30_000)
    return () => { mounted.current = false; clearInterval(timer) }
  }, [])
  // 进页面或切渠道标签时，把**当下这一屏**的余额主动打一发上游。
  //
  // 30 秒一次的轮询读的是 `/api/credit` 的内存快照，余额数字不会因为轮询变新 ——
  // 不补这一发，页面上一直是"上次查询"那一刻的数。只刷当前渠道、按渠道 60 秒去重，
  // 是为了让"标签页切出去再切回来"不至于变成一台常驻爬虫。
  useEffect(() => {
    if (!data) return
    if (Date.now() - (balanceFreshAt.get(providerId) ?? 0) < 60_000) return
    const targets = data.accounts.filter(account => account.provider === providerId && account.enabled)
    if (!targets.length) return
    balanceFreshAt.set(providerId, Date.now())
    let cancelled = false
    void (async () => {
      for (const account of targets) {
        if (cancelled || !mounted.current) return
        try {
          const result = await creditApi.action({ id: account.id, action: 'credits' })
          if (cancelled || !mounted.current || !result.credits) continue
          qc.setQueryData(['credit'], (previous: typeof data) => previous
            ? { ...previous, accounts: previous.accounts.map(item => item.id === account.id ? { ...item, credits: result.credits } : item) }
            : previous)
          setCreditErrors(values => { const next = { ...values }; delete next[account.id]; return next })
        } catch (e) {
          if (!cancelled && mounted.current) setCreditErrors(values => ({ ...values, [account.id]: message(e) }))
        }
      }
    })()
    return () => { cancelled = true }
    // 依赖里只写渠道和"数据到没到"：data 每 30 秒就是一个新对象，写进去这条就退化成常驻轮询。
  }, [providerId, !!data])
  function refresh() {
    void qc.invalidateQueries({ queryKey: ['credit'] })
    void qc.invalidateQueries({ queryKey: ['models'] })
    void qc.invalidateQueries({ queryKey: ['status'] })
  }
  function modelList(account: Account) {
    return account.models?.length ? account.models : data?.providers.find(p => p.id === account.provider)?.models ?? []
  }
  function lock(id: string) {
    if (locks.current.has(id)) return false
    locks.current.add(id)
    setBusyIds(new Set(locks.current))
    return true
  }
  function unlock(id: string) {
    locks.current.delete(id)
    if (mounted.current) setBusyIds(new Set(locks.current))
  }
  async function update(body: AccountUpdate) {
    if (!lock(body.id)) throw new Error('此账户有操作进行中，请稍后重试。')
    try {
      const result = await creditApi.update(body)
      if (!result.ok) throw new Error('账户设置未保存。')
      refresh()
      toast.ok('账户设置已更新')
    } finally { unlock(body.id) }
  }
  async function runAction(account: Account, action: AccountAction, options: { quiet?: boolean } = {}): Promise<ActionResult | undefined> {
    if (!lock(account.id)) return
    setOutcomes(values => ({ ...values, [account.id]: { state: 'running', text: '正在处理…' } }))
    // settled 是"后端到底怎么答的"，与页面把它显示成成功还是失败无关：
    // 领取失败也带 claimHold 回来，批量签到要靠它做整批早停，所以不能只留成功那条路。
    let settled: ActionResult | undefined
    try {
      const result = await creditApi.action(action)
      settled = result
      if (!result.ok) throw new Error(result.message || '操作未成功，请查看账户状态后重试。')
      const hasFailed = result.results?.some(item => !item.claimed && !item.alreadyClaimed) ?? false
      // 签到单独说一句人话：领到了 / 上游记着已领 / 根本没有可领项，是三件不同的事，
      // 统一报"操作完成"就会让人以为额度已经到账（而余额其实没动）。
      const summary = action.action === 'claim' ? claimSummary(result.results, !account.machineReady) : null
      const bad = summary ? !summary.ok : hasFailed
      const text = result.message || summary?.text || (hasFailed ? '部分活动未领取，请查看逐项结果。' : '操作完成')
      setOutcomes(values => ({ ...values, [account.id]: { state: bad ? 'error' : 'ok', text, result } }))
      if (result.credits) {
        qc.setQueryData(['credit'], (previous: typeof data) => previous ? { ...previous, accounts: previous.accounts.map(item => item.id === account.id ? { ...item, credits: result.credits } : item) } : previous)
        setCreditErrors(values => { const next = { ...values }; delete next[account.id]; return next })
      }
      if (!options.quiet) (bad ? toast.bad : toast.ok)(`${account.name}：${text}`)
    } catch (e) {
      const text = message(e)
      setOutcomes(values => ({ ...values, [account.id]: { state: 'error', text } }))
      if (action.action === 'credits') setCreditErrors(values => ({ ...values, [account.id]: text }))
      if (!options.quiet) toast.bad(`${account.name}：${text}`)
    } finally { unlock(account.id); if (mounted.current) refresh() }
    return settled
  }
  function claim(account: Account, campaignId?: string) {
    void runAction(account, { id: account.id, action: 'claim', ...(campaignId ? { campaignId } : {}) })
  }
  async function batchClaim() {
    if (batchLock.current) return
    const targets = visible.filter(account => account.enabled && !busyIds.has(account.id))
    // 整渠道一个号都带不上设备身份时，批量签到只会拿到 N 份"没有可领活动"：
    // 先把话说清楚，也省掉这 N 次注定空手的上游请求。
    if (targets.length && targets.every(account => !account.machineReady)) {
      toast.bad('这些账号都还没有 Qoder 设备身份，上游不会下发「每日签到」——先在上方「设备身份」导入。')
      return
    }
    batchLock.current = true
    setBatch(true)
    // 同类领取错误连着两个号就整批停：429/9074 是"你太快了"、1005 是套餐级上限、
    // 401 是凭据没了 —— 这几种判决跟是哪个号无关，接着往下撞只是把一次点击
    // 放大成"账号数 × 一轮完整上游请求"。所以第 2 个号照常试（万一是单号问题），
    // 第 3 个开始不试了。
    let lastKind = ''
    let repeat = 0
    let stopped = false
    try {
      for (const account of targets) {
        if (!mounted.current) break
        const result = await runAction(account, { id: account.id, action: 'claim' }, { quiet: true })
        const kind = result?.claimHold?.kind ?? ''
        if (!kind) { lastKind = ''; repeat = 0; continue }
        repeat = kind === lastKind ? repeat + 1 : 1
        lastKind = kind
        if (repeat >= 2) { stopped = true; break }
      }
      if (!mounted.current) return
      if (stopped) toast.bad(`连续两个账号返回同一类领取错误（${claimHoldName(lastKind)}），本渠道剩余账号本轮未尝试；自动领取会按冷却顺延，手动「单号签到」不受限。`)
      else toast.info('本轮已逐号处理，请查看每个账户的领取结果。')
    } finally { batchLock.current = false; if (mounted.current) setBatch(false) }
  }
  const busy = (account: Account) => busyIds.has(account.id) || batch
  return (
    <>
      <PageTitle title="Credit" description="管理你自己的账户、模型和额度。按渠道独立管理，不合并不同单位的余额。" actions={<><Button size="sm" onClick={() => setDialog({ kind: 'import' })}><Upload size={14} />导入</Button><Button size="sm" disabled={!accounts.length} onClick={() => setDialog({ kind: 'export' })}><Download size={14} />备份导出</Button></>} />
      <div className="space-y-4">
        <Card><CardBody className="flex flex-wrap items-center justify-between gap-5"><div className="flex flex-wrap gap-8"><Stat label="已连接账户" value={data ? accounts.length : '未知'} hint="仅显示凭据指纹" /><Stat label="已启用" value={data ? accounts.filter(a => a.enabled).length : '未知'} hint="顺序决定渠道内优先级" /><Stat label="已设自动领取" value={data ? accounts.filter(a => a.autoClaimAt).length : '未知'} hint="每个账号各一个时刻" /></div><p className="max-w-xs text-[11.5px] text-[var(--ink-3)]">自动领取在各账号的「设置」里填每日时刻（UTC+8），留空即不自动领。到点只按活动清单领取一次，失败当天不重试，也不会发送推理请求。</p></CardBody></Card>
        <div role="tablist" aria-label="Credit 渠道" className="grid grid-cols-2 gap-2 sm:grid-cols-3">{channels.map(item => <button key={item.id} id={`credit-tab-${item.id}`} role="tab" aria-controls={`credit-panel-${providerId}`} aria-selected={providerId === item.id} tabIndex={providerId === item.id ? 0 : -1} onKeyDown={e => { if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') { e.preventDefault(); const step = e.key === 'ArrowRight' ? 1 : channels.length - 1; const next = channels[(channels.findIndex(c => c.id === item.id) + step) % channels.length].id; setProviderId(next); document.getElementById(`credit-tab-${next}`)?.focus() } }} onClick={() => setProviderId(item.id)} className={cn('fmh-hoverable rounded-[var(--r-md)] border p-3 text-left transition-colors', providerId === item.id ? 'border-[var(--accent)] bg-[var(--accent-soft)] text-[var(--accent)]' : 'border-[var(--line)] bg-[var(--surface)] text-[var(--ink-2)]')}><span className="flex items-center justify-between gap-2 text-[13.5px] font-semibold">{item.name}<Badge tone={providerId === item.id ? 'accent' : 'idle'}>{data ? accounts.filter(account => account.provider === item.id).length : '—'}</Badge></span><span className="mt-1 block text-[11.5px] text-[var(--ink-3)]">{item.description}</span></button>)}</div>
        {query.error && <ErrorNote>账户数据读取失败：{message(query.error)}{data ? '。以下为上次加载的数据。' : ''}<Button size="sm" className="ml-2" onClick={() => void query.refetch()}>重试</Button></ErrorNote>}
        <section role="tabpanel" id={`credit-panel-${providerId}`} aria-labelledby={`credit-tab-${providerId}`} className="space-y-3">
          {needsMachine(providerId) && <Card><CardBody className="space-y-2">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="text-[12.5px] font-medium text-[var(--ink)]">Qoder 设备身份</div>
              <div className="flex flex-wrap gap-2"><Button size="sm" onClick={() => setDialog({ kind: 'machine' })}><Upload size={13} />{data?.machine?.configured ? '换一份' : '导入'}</Button>{data?.machine?.configured ? <Button size="sm" variant="ghost" onClick={() => setDialog({ kind: 'machineClear' })}>清除</Button> : null}</div>
            </div>
            {data?.machine?.configured
              ? <div className="flex flex-wrap gap-x-4 gap-y-1 text-[11.5px] text-[var(--ink-3)]"><span className="mono">指纹：{data.machine.hint || '未知'}</span><span>写入于 {date(data.machine.updatedAt)}</span><span>全局共用一份；凭据里自带成对值的账号用自己的。</span></div>
              : <Notice tone="warn">还没有设备身份。上游缺这两个头时不会下发「每日签到」，签到也就一定领不到 —— 这不代表你的账号没有活动。在装有 Qoder 客户端的电脑上跑 <span className="mono">tools/qoder-machine-identity.ps1</span>，它会在<b>桌面</b>生成 <span className="mono">qoder-machine-identity.json</span>；用这台电脑的浏览器点右上「导入」，选这个文件即可。</Notice>}
          </CardBody></Card>}
          <div className="flex flex-wrap items-center justify-between gap-3"><h2 className="text-[15px] font-semibold text-[var(--ink)]">{channel.name} 账户</h2><div className="flex flex-wrap gap-2"><Button size="sm" disabled={query.isFetching} onClick={() => void query.refetch()}>{query.isFetching ? <Spinner /> : <RefreshCw size={13} />}刷新列表</Button><Button size="sm" disabled={batch || !visible.some(a => a.enabled && !busy(a))} onClick={() => void batchClaim()}>{batch ? <Spinner /> : <Gift size={13} />}{batch ? '逐号签到中…' : '本渠道批量签到'}</Button><Button size="sm" variant="primary" disabled={!provider?.loginModes.length} onClick={() => { if (provider) setDialog({ kind: 'login', provider }) }}><ExternalLink size={13} />浏览器登录</Button></div></div>
          {query.isLoading ? <Card><Loading label="正在加载 Credit 账户…" /></Card> : !visible.length && data ? <Card><Empty icon={<Wallet size={32} />} title={`还没有 ${channel.name} 账户`} description={`点上方「浏览器登录」连接你的 ${channel.name} 账户。未添加账户时不会查询上游，也不会测试模型。`} /></Card> : visible.map((account, index) => {
            const outcome = outcomes[account.id]
            const accountBusy = busy(account)
            const cooling = timestamp(account.cooldownUntil) > Date.now()
            return <Card key={account.id}>
              <CardBody className="space-y-4">
                <div className="flex flex-wrap items-start justify-between gap-3"><div className="min-w-0 space-y-1.5"><div className="flex flex-wrap items-center gap-2"><span className="text-[11px] text-[var(--ink-3)]">#{index + 1}</span><h3 className="break-all text-[14px] font-semibold text-[var(--ink)]">{account.name}</h3><Badge tone={account.enabled ? 'ok' : 'idle'}>{account.enabled ? '启用' : '停用'}</Badge>{expired(account.expiresAt) && <Badge tone="bad">凭据已过期</Badge>}{cooling && <Badge tone="warn">冷却中</Badge>}</div><div className="flex flex-wrap gap-x-4 gap-y-1 text-[11.5px] text-[var(--ink-3)]"><span className="mono break-all">凭据指纹：{account.hint || '未提供'}</span>{account.uid && <span className="break-all">UID：{account.uid}</span>}<span>凭据到期：{date(account.expiresAt)}</span></div></div><div className="flex flex-wrap items-center gap-1"><Switch label={`启用 ${account.name}`} checked={account.enabled} disabled={accountBusy} onChange={enabled => void update({ id: account.id, enabled }).catch(e => toast.bad(message(e)))} /><Button size="sm" variant="ghost" disabled={accountBusy || index === 0} onClick={() => void update({ id: account.id, move: 'up' }).catch(e => toast.bad(message(e)))} aria-label={`上移 ${account.name}`} title="上移"><ArrowUp size={14} /></Button><Button size="sm" variant="ghost" disabled={accountBusy || index === visible.length - 1} onClick={() => void update({ id: account.id, move: 'down' }).catch(e => toast.bad(message(e)))} aria-label={`下移 ${account.name}`} title="下移"><ArrowDown size={14} /></Button><Button size="sm" variant="ghost" disabled={accountBusy} onClick={() => setDialog({ kind: 'edit', account })}><Pencil size={13} />设置</Button><Button size="sm" variant="ghost" disabled={accountBusy} onClick={() => setDialog({ kind: 'delete', account })} aria-label={`删除 ${account.name}`} title="删除账户"><Trash2 size={14} /></Button></div></div>
                {account.error && <ErrorNote>{account.error}</ErrorNote>}
                {expired(account.expiresAt) && <Notice tone="warn">凭据已过期，请刷新凭据或重新登录。过期不表示账户余额为 0。</Notice>}
                {cooling && <Notice tone="warn">账户冷却至 {date(account.cooldownUntil)}。建议等待冷却结束；重置只清除本地状态，不能绕过上游限制。</Notice>}
                <div className="flex flex-wrap items-center gap-2"><Button size="sm" disabled={accountBusy} onClick={() => void runAction(account, { id: account.id, action: 'credits' })}><Wallet size={13} />查询余额</Button><Button size="sm" disabled={accountBusy} onClick={() => claim(account)}><Gift size={13} />单号签到</Button><Button size="sm" disabled={accountBusy} onClick={() => void runAction(account, { id: account.id, action: 'refresh' })}><KeyRound size={13} />刷新凭据</Button><Button size="sm" disabled={accountBusy} onClick={() => void runAction(account, { id: account.id, action: 'models' })}><RefreshCw size={13} />刷新模型</Button>{provider?.loginModes.length ? <Button size="sm" disabled={accountBusy} onClick={() => setDialog({ kind: 'login', provider, accountId: account.id })}>重新登录</Button> : null}<Button size="sm" variant="ghost" disabled={accountBusy} onClick={() => setDialog({ kind: 'test', account })}>手动测试（耗额度）</Button><Button size="sm" variant="ghost" disabled={accountBusy} onClick={() => setDialog({ kind: 'reset', account })}>重置状态</Button>{busyIds.has(account.id) && <Spinner />}</div>
                {outcome && <div aria-live="polite" className="space-y-2"><Notice tone={outcome.state === 'error' ? 'bad' : outcome.state === 'ok' ? 'ok' : 'accent'}>{outcome.text}</Notice>{outcome.result?.results?.map((result, resultIndex) => <div key={`${result.id}-${resultIndex}`} className="flex flex-wrap items-center gap-2 text-[12px]"><Badge tone={result.claimed || result.alreadyClaimed ? 'ok' : 'bad'}>{result.alreadyClaimed ? '已领过' : result.claimed ? '领取成功' : '未领取'}</Badge><span>{result.name} · 数量 {quantity(result.amount)}</span><span className="text-[var(--ink-3)]">{result.message}</span></div>)}</div>}
                <CreditBalances credits={account.credits} staleError={creditErrors[account.id]} busy={accountBusy} machineMissing={!account.machineReady} onClaim={campaignId => claim(account, campaignId)} />
                <div className="flex flex-wrap gap-x-4 gap-y-1 border-t border-[var(--line)] pt-2 text-[11.5px] text-[var(--ink-3)]"><span>模型 {modelList(account).length || '未知'} · 禁用 {account.disabledModels.length}</span><span>effort：{account.effort || '默认'}</span><span>Max Mode：{account.maxMode ? '开启' : '关闭'}</span><span>自动领取：{account.autoClaimAt || '关闭'}</span>{account.claimCooldownKind ? <span className="text-[var(--bad)]">签到冷却：{claimHoldText(account)}</span> : null}</div>
              </CardBody>
            </Card>
          })}
        </section>
      </div>
      {dialog?.kind === 'import' && <CredentialDialog onClose={() => setDialog(null)} onDone={refresh} />}
      {dialog?.kind === 'machine' && <MachineDialog machine={data?.machine} onClose={() => setDialog(null)} onDone={refresh} />}
      {dialog?.kind === 'machineClear' && <ConfirmDialog title="清除设备身份" label="确认清除" danger onClose={() => setDialog(null)} onConfirm={async () => { const result = await creditApi.machine({ machineToken: '', machineType: '' }); if (!result.ok) throw new Error('清除未成功。'); refresh(); toast.ok('已清除全局设备身份') }}><Notice tone="warn">清除之后 Qoder 的每日签到会领不到，上游也不再下发「每日签到」这一项；只有凭据里自带成对 machineToken / machineType 的账号不受影响。重新导入即可恢复。</Notice></ConfirmDialog>}
      {dialog?.kind === 'export' && <ExportDialog onClose={() => setDialog(null)} />}
      {dialog?.kind === 'edit' && <AccountSettings account={dialog.account} models={modelList(dialog.account)} onClose={() => setDialog(null)} onSave={update} />}
      {dialog?.kind === 'delete' && <ConfirmDialog title="删除账户" label="确认删除" danger onClose={() => setDialog(null)} onConfirm={async () => { const result = await creditApi.delete(dialog.account.id); if (!result.ok) throw new Error('删除未成功'); refresh(); toast.ok('账户已删除') }}><Notice tone="warn">确定删除“{dialog.account.name}”？这会移除保存在此服务中的账户凭据，无法撤销，不会注销官方账户。</Notice></ConfirmDialog>}
      {dialog?.kind === 'reset' && <ConfirmDialog title="重置账户状态" label="确认重置" onClose={() => setDialog(null)} onConfirm={async () => { await runAction(dialog.account, { id: dialog.account.id, action: 'reset' }) }}><Notice tone="warn">仅重置“{dialog.account.name}”在此服务中的错误 / 冷却状态，不增加额度，也不能解除上游风控或限流。</Notice></ConfirmDialog>}
      {dialog?.kind === 'test' && <TestDialog account={dialog.account} models={modelList(dialog.account)} onClose={() => setDialog(current => current?.kind === 'test' ? null : current)} onTest={async model => { await runAction(dialog.account, { id: dialog.account.id, action: 'test', model }) }} />}
      {dialog?.kind === 'login' && <LoginDialog provider={dialog.provider} accountId={dialog.accountId} onClose={() => setDialog(null)} onDone={refresh} />}
    </>
  )
}
