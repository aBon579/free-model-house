import { useEffect, useId, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { ArrowDown, ArrowUp, Download, ExternalLink, Gift, KeyRound, Pencil, RefreshCw, Trash2, Upload, Wallet, X } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import { Badge, Button, Card, CardBody, CardHeader, Empty, ErrorNote, Field, Input, Loading, Notice, Select, Spinner, Stat, Switch, cn } from '../components/ui'
import { useToast } from '../components/toast'
import { creditApi, useCredit, type Account, type AccountAction, type AccountUpdate, type ActionResult, type CreditDate, type Credits, type ImportAccount, type Login, type Model, type Provider, type ProviderId } from '../api/credit'

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

function CredentialDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [secret, setSecret] = useState('')
  const [fileName, setFileName] = useState('')
  const op = useOperation()
  async function submit() {
    await op.run(async () => {
      const parsed = parseJSON(secret)
      const entries = Array.isArray(parsed) ? parsed : isObject(parsed) ? parsed.accounts : undefined
      if (isObject(parsed) && parsed.format !== undefined && (parsed.format !== 'fmh-credit' || parsed.version !== 1)) throw new Error('不支持的备份格式或版本，仅支持 fmh-credit v1。')
      if (!Array.isArray(entries) || entries.length === 0) throw new Error('请提供非空 accounts 数组，或完整的 fmh-credit 备份。')
      if (entries.some(entry => !isObject(entry) || !channels.some(c => c.id === entry.provider) || typeof entry.name !== 'string' || !entry.name.trim() || !isObject(entry.credential))) throw new Error('每个账户必须包含有效 provider、name 和 credential 对象。')
      const result = await creditApi.import(entries as ImportAccount[])
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
      <Notice tone="warn">Qoder / Qoder 中国版每日领取需要官方设备信息：请在 credential 中提供 machineToken 和 machineType；缺失时领取可能失败。不要伪造设备信息。</Notice>
      <Field label="敏感账户 JSON（输入已遮蔽）" hint='格式：{"accounts":[{"provider":"qoder","name":"个人账户","credential":{…}}]}，也可粘贴导出的完整备份。'>
        <SecretTextarea value={secret} onChange={setSecret} label="敏感凭据 JSON" disabled={op.busy} placeholder="在此粘贴 JSON，内容不回显" />
      </Field>
      <label className="flex flex-wrap items-center gap-2 text-[12px]">
        <Upload size={14} /><span>从 JSON 文件读取</span>
        <input type="file" accept=".json,application/json" aria-label="导入凭据 JSON 文件" disabled={op.busy} className="max-w-full text-[12px]" onChange={e => {
          const file = e.target.files?.[0]
          e.target.value = ''
          if (!file) return
          void op.run(async () => {
            if (file.size > 5 * 1024 * 1024) throw new Error('文件超过 5 MB，请分批导入。')
            setSecret(await file.text())
            setFileName(file.name)
          })
        }} />
      </label>
      {fileName && <div className="text-[12px] text-[var(--ink-3)]">已读取：{fileName}，提交前不会上传。</div>}
      {op.error && <ErrorNote>{op.error}</ErrorNote>}
      <div className="flex justify-end gap-2"><Button disabled={op.busy} onClick={onClose}>取消</Button><Button variant="primary" disabled={op.busy || !secret.trim()} onClick={() => void submit()}>{op.busy && <Spinner />}确认导入</Button></div>
    </Modal>
  )
}

function AccountSettings({ account, models, onClose, onSave }: { account: Account; models: Model[]; onClose: () => void; onSave: (body: AccountUpdate) => Promise<void> }) {
  const [name, setName] = useState(account.name)
  const [disabled, setDisabled] = useState(account.disabledModels)
  const [effort, setEffort] = useState(account.effort)
  const [maxMode, setMaxMode] = useState(account.maxMode)
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
      {op.error && <ErrorNote>{op.error}</ErrorNote>}
      <div className="flex justify-end gap-2"><Button disabled={op.busy} onClick={onClose}>取消</Button><Button variant="primary" disabled={op.busy || !name.trim()} onClick={() => void op.run(async () => { await onSave({ id: account.id, name: name.trim(), disabledModels: disabled, effort, maxMode }); onClose() })}>{op.busy && <Spinner />}保存设置</Button></div>
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

function CreditBalances({ credits, staleError, busy, onClaim }: { credits?: Credits; staleError?: string; busy: boolean; onClaim: (campaignId: string) => void }) {
  return <div className="space-y-3">
    {staleError && <Notice tone="warn">余额获取失败，当前余额未知；下方如有数据，仅为上次成功快照，不代表当前可用额度。</Notice>}
    {!credits ? <div className="rounded-[var(--r-sm)] border border-dashed border-[var(--line)] p-4 text-[12.5px] text-[var(--ink-3)]">余额与资源包：未知。点击“查询余额”获取，不将未查询或失败当作 0。</div> : <>
      <div className="flex flex-wrap items-center justify-between gap-2 text-[12px] text-[var(--ink-3)]"><span>余额 / 资源包 · {staleError ? '历史快照' : '上次查询'}</span><span>{date(credits.updatedAt)}</span></div>
      {credits.warning && <Notice tone="warn">{credits.warning}</Notice>}
      {!credits.balances?.length ? <p className="text-[12.5px] text-[var(--ink-3)]">上游没有返回余额明细，余额未知。</p> : <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">{credits.balances.map((balance, index) => <div key={`${balance.name}-${index}`} className="space-y-2 rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface-2)] p-3"><div className="flex flex-wrap justify-between gap-1 text-[12.5px] font-medium"><span>{balance.name}</span>{expired(balance.expiresAt) && <Badge tone="warn">已到期</Badge>}</div><div className="text-[20px] font-semibold text-[var(--ink)]">{staleError ? '未知' : quantity(balance.remaining)} <span className="text-[12px] font-normal text-[var(--ink-3)]">{balance.unit || '单位未知'} · 剩余</span></div><div className="text-[11.5px] text-[var(--ink-3)]">总量 {quantity(balance.total)} · 已用 {quantity(balance.used)}{staleError ? ` · 上次剩余 ${quantity(balance.remaining)}` : ''}</div><div className="text-[11.5px] text-[var(--ink-3)]">到期 {date(balance.expiresAt)}</div></div>)}</div>}
      <div className="space-y-2 border-t border-[var(--line)] pt-3"><div className="text-[12.5px] font-medium">活动与每日领取</div>{!credits.campaigns?.length ? <p className="text-[12px] text-[var(--ink-3)]">未返回可展示活动；可尝试单号签到，以实际返回结果为准。</p> : credits.campaigns.map(campaign => <div key={campaign.id} className="flex flex-wrap items-center justify-between gap-2 rounded-[var(--r-sm)] border border-[var(--line)] p-2.5"><div><div className="text-[12.5px]">{campaign.name} · {quantity(campaign.amount)} <span className="text-[var(--ink-3)]">（活动额度，单位以上游为准）</span></div><div className="text-[11.5px] text-[var(--ink-3)]">到期 {date(campaign.expiresAt)}</div></div><Button size="sm" disabled={busy || campaign.claimed || !campaign.claimable || expired(campaign.expiresAt)} onClick={() => onClaim(campaign.id)}>{campaign.claimed ? '已领取' : expired(campaign.expiresAt) ? '已到期' : campaign.claimable ? '领取一次' : '不可领取'}</Button></div>)}</div>
    </>}
  </div>
}

interface Outcome { state: 'running' | 'ok' | 'error'; text: string; result?: ActionResult }
type Dialog = { kind: 'import' | 'export' } | { kind: 'edit' | 'delete' | 'reset' | 'test'; account: Account } | { kind: 'login'; provider: Provider; accountId?: string } | null

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
  const settings = useOperation()
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
  async function runAction(account: Account, action: AccountAction, options: { quiet?: boolean } = {}) {
    if (!lock(account.id)) return
    setOutcomes(values => ({ ...values, [account.id]: { state: 'running', text: '正在处理…' } }))
    try {
      const result = await creditApi.action(action)
      if (!result.ok) throw new Error(result.message || '操作未成功，请查看账户状态后重试。')
      const hasFailed = result.results?.some(item => !item.claimed && !item.alreadyClaimed) ?? false
      const text = result.message || (hasFailed ? '部分活动未领取，请查看逐项结果。' : action.action === 'claim' ? '签到请求已完成，实际领取情况见下方结果。' : '操作完成')
      setOutcomes(values => ({ ...values, [account.id]: { state: hasFailed ? 'error' : 'ok', text, result } }))
      if (result.credits) {
        qc.setQueryData(['credit'], (previous: typeof data) => previous ? { ...previous, accounts: previous.accounts.map(item => item.id === account.id ? { ...item, credits: result.credits } : item) } : previous)
        setCreditErrors(values => { const next = { ...values }; delete next[account.id]; return next })
      }
      if (!options.quiet) (hasFailed ? toast.bad : toast.ok)(`${account.name}：${text}`)
    } catch (e) {
      const text = message(e)
      setOutcomes(values => ({ ...values, [account.id]: { state: 'error', text } }))
      if (action.action === 'credits') setCreditErrors(values => ({ ...values, [account.id]: text }))
      if (!options.quiet) toast.bad(`${account.name}：${text}`)
    } finally { unlock(account.id); if (mounted.current) refresh() }
  }
  function claim(account: Account, campaignId?: string) {
    void runAction(account, { id: account.id, action: 'claim', ...(campaignId ? { campaignId } : {}) })
  }
  async function batchClaim() {
    if (batchLock.current) return
    batchLock.current = true
    setBatch(true)
    const targets = visible.filter(account => account.enabled && !busyIds.has(account.id))
    try {
      for (const account of targets) {
        if (!mounted.current) break
        await runAction(account, { id: account.id, action: 'claim' }, { quiet: true })
      }
      if (mounted.current) toast.info('本轮已逐号处理，请查看每个账户的领取结果。')
    } finally { batchLock.current = false; if (mounted.current) setBatch(false) }
  }
  const busy = (account: Account) => busyIds.has(account.id) || batch
  return (
    <>
      <PageTitle title="Credit" description="管理你自己的账户、模型和额度。按渠道独立管理，不合并不同单位的余额。" actions={<><Button size="sm" onClick={() => setDialog({ kind: 'import' })}><Upload size={14} />导入</Button><Button size="sm" disabled={!accounts.length} onClick={() => setDialog({ kind: 'export' })}><Download size={14} />备份导出</Button></>} />
      <div className="space-y-4">
        <Card><CardBody className="flex flex-wrap items-center justify-between gap-5"><div className="flex flex-wrap gap-8"><Stat label="已连接账户" value={data ? accounts.length : '未知'} hint="仅显示凭据指纹" /><Stat label="已启用" value={data ? accounts.filter(a => a.enabled).length : '未知'} hint="顺序决定渠道内优先级" /></div><div className="space-y-2"><div className="flex items-center justify-between gap-4"><span className="text-[13px] font-medium text-[var(--ink)]">自动签到（全部渠道）</span><Switch label="自动签到（全部渠道）" checked={data?.autoClaim ?? false} disabled={!data || settings.busy} onChange={value => void settings.run(async () => { const result = await creditApi.settings(value); if (!result.ok) throw new Error('自动签到设置未保存'); refresh(); toast.ok(value ? '自动签到已开启，到点按活动清单领取' : '自动签到已关闭') })} /></div><p className="max-w-xs text-[11.5px] text-[var(--ink-3)]">仅由你显式开启；到点只按活动清单领取，不会发送推理请求。</p>{settings.busy && <Spinner />}{settings.error && <ErrorNote>{settings.error}</ErrorNote>}</div></CardBody></Card>
        <div role="tablist" aria-label="Credit 渠道" className="grid grid-cols-2 gap-2 sm:grid-cols-3">{channels.map(item => <button key={item.id} id={`credit-tab-${item.id}`} role="tab" aria-controls={`credit-panel-${providerId}`} aria-selected={providerId === item.id} tabIndex={providerId === item.id ? 0 : -1} onKeyDown={e => { if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') { e.preventDefault(); const step = e.key === 'ArrowRight' ? 1 : channels.length - 1; const next = channels[(channels.findIndex(c => c.id === item.id) + step) % channels.length].id; setProviderId(next); document.getElementById(`credit-tab-${next}`)?.focus() } }} onClick={() => setProviderId(item.id)} className={cn('fmh-hoverable rounded-[var(--r-md)] border p-3 text-left transition-colors', providerId === item.id ? 'border-[var(--accent)] bg-[var(--accent-soft)] text-[var(--accent)]' : 'border-[var(--line)] bg-[var(--surface)] text-[var(--ink-2)]')}><span className="flex items-center justify-between gap-2 text-[13.5px] font-semibold">{item.name}<Badge tone={providerId === item.id ? 'accent' : 'idle'}>{data ? accounts.filter(account => account.provider === item.id).length : '—'}</Badge></span><span className="mt-1 block text-[11.5px] text-[var(--ink-3)]">{item.description}</span></button>)}</div>
        {query.error && <ErrorNote>账户数据读取失败：{message(query.error)}{data ? '。以下为上次加载的数据。' : ''}<Button size="sm" className="ml-2" onClick={() => void query.refetch()}>重试</Button></ErrorNote>}
        <section role="tabpanel" id={`credit-panel-${providerId}`} aria-labelledby={`credit-tab-${providerId}`} className="space-y-3">
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
                <CreditBalances credits={account.credits} staleError={creditErrors[account.id]} busy={accountBusy} onClaim={campaignId => claim(account, campaignId)} />
                <div className="flex flex-wrap gap-x-4 gap-y-1 border-t border-[var(--line)] pt-2 text-[11.5px] text-[var(--ink-3)]"><span>模型 {modelList(account).length || '未知'} · 禁用 {account.disabledModels.length}</span><span>effort：{account.effort || '默认'}</span><span>Max Mode：{account.maxMode ? '开启' : '关闭'}</span></div>
              </CardBody>
            </Card>
          })}
        </section>
      </div>
      {dialog?.kind === 'import' && <CredentialDialog onClose={() => setDialog(null)} onDone={refresh} />}
      {dialog?.kind === 'export' && <ExportDialog onClose={() => setDialog(null)} />}
      {dialog?.kind === 'edit' && <AccountSettings account={dialog.account} models={modelList(dialog.account)} onClose={() => setDialog(null)} onSave={update} />}
      {dialog?.kind === 'delete' && <ConfirmDialog title="删除账户" label="确认删除" danger onClose={() => setDialog(null)} onConfirm={async () => { const result = await creditApi.delete(dialog.account.id); if (!result.ok) throw new Error('删除未成功'); refresh(); toast.ok('账户已删除') }}><Notice tone="warn">确定删除“{dialog.account.name}”？这会移除保存在此服务中的账户凭据，无法撤销，不会注销官方账户。</Notice></ConfirmDialog>}
      {dialog?.kind === 'reset' && <ConfirmDialog title="重置账户状态" label="确认重置" onClose={() => setDialog(null)} onConfirm={async () => { await runAction(dialog.account, { id: dialog.account.id, action: 'reset' }) }}><Notice tone="warn">仅重置“{dialog.account.name}”在此服务中的错误 / 冷却状态，不增加额度，也不能解除上游风控或限流。</Notice></ConfirmDialog>}
      {dialog?.kind === 'test' && <TestDialog account={dialog.account} models={modelList(dialog.account)} onClose={() => setDialog(current => current?.kind === 'test' ? null : current)} onTest={model => runAction(dialog.account, { id: dialog.account.id, action: 'test', model })} />}
      {dialog?.kind === 'login' && <LoginDialog provider={dialog.provider} accountId={dialog.accountId} onClose={() => setDialog(null)} onDone={refresh} />}
    </>
  )
}
