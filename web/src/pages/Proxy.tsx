import { useEffect, useState } from 'react'
import { Globe, ShieldCheck, Waypoints, XCircle } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import {
  Button,
  Card,
  CardBody,
  CardHeader,
  Input,
  Notice,
  Select,
  Spinner,
  Switch,
} from '../components/ui'
import { useToast } from '../components/toast'
import { useProxy, useSaveProxy, useStatus, useTestProxy } from '../api/client'

/** 代理协议选项。 */
const schemes = [
  { value: 'http', label: 'HTTP' },
  { value: 'https', label: 'HTTPS' },
  { value: 'socks5', label: 'SOCKS5' },
] as const

/** 出口代理页 —— 独立一页，不和现有的页混合。 */
export function Proxy() {
  const { data: proxy, isLoading } = useProxy()
  const { data: status } = useStatus()
  const save = useSaveProxy()
  const test = useTestProxy()
  const toast = useToast()

  const [draft, setDraft] = useState<{
    enabled: boolean
    scheme: string
    host: string
    port: number
  } | null>(null)

  // ⚠️ **dirty 只看"代理信息"（协议/地址/端口），不看开关** ——
  //
  // 开关是即点即生效的（拨了立刻保存），它永远和后端真值一致；
  // "保存"按钮管的是**信息**的修改。
  // 之前把 enabled 也算进 dirty：关掉开关 → "保存并生效"亮起 →
  // 用户误以为开关也要按保存才生效 —— 语义混乱的根源。
  const dirty = draft && proxy
    ? draft.scheme !== proxy.scheme ||
      draft.host !== proxy.host ||
      draft.port !== proxy.port
    : false

  // ⚠️ 两个 effect 合一：proxy 到来且（没有 draft，或信息与真值一致）→ 同步。
  //   信息有未保存编辑时不碰 —— 别把用户正在改的地址冲掉。
  //   enabled 不参与判定 —— 开关即点即保存，永远自己会追平。
  useEffect(() => {
    if (!proxy) return
    const next = {
      enabled: proxy.enabled,
      scheme: proxy.scheme || 'http',
      host: proxy.host,
      port: proxy.port,
    }
    if (!draft) {
      setDraft(next)
      return
    }
    const same =
      draft.scheme === next.scheme &&
      draft.host === next.host &&
      draft.port === next.port
    if (same) {
      setDraft(next)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [proxy])

  // 检测结果（保留最近一次，直到再点）。
  const [testResult, setTestResult] = useState<{
    ok: boolean
    ip: string
    country: string
    error: string
    egressError: string
  } | null>(null)

  if (isLoading || !draft) {
    return (
      <>
        <PageTitle title="出口代理" />
        <Card>
          <CardBody className="py-8 text-center text-[12.5px] text-[var(--ink-3)]">
            读取中…
          </CardBody>
        </Card>
      </>
    )
  }

  const address = draft.host
    ? `${draft.scheme}://${draft.host}${draft.port ? ':' + draft.port : ''}`
    : ''

  async function doSave() {
    if (!draft) return
    const d = { ...draft, host: draft.host.trim() }
    try {
      await save.mutateAsync(d)
      toast.ok('代理信息已保存并生效')
    } catch (e) {
      toast.bad((e as Error).message)
    }
  }

  async function doTest() {
    setTestResult(null)
    try {
      const res = await test.mutateAsync()
      setTestResult({
        ok: res.ok,
        ip: res.egressIp ?? '',
        country: res.egressCountry ?? '',
        error: res.error ?? '',
        egressError: res.egressError ?? '',
      })
      if (res.ok) {
        toast.ok('代理连通')
      } else {
        toast.bad('代理不通 —— 看页面上的原因')
      }
    } catch (e) {
      setTestResult({
        ok: false, ip: '', country: '',
        error: (e as Error).message, egressError: '',
      })
    }
  }

  return (
    <>
      <PageTitle
        title="出口代理"
        description="让发往上游的流量经过你指定的代理 —— 换出口 IP 就能换限额、解地区封锁。"
      />

      <div className="space-y-4">
        {/* 当前出口 —— 检测按钮就在这张卡上：
            ① 代理开着 → 测代理 + 经出口代理
            ② 代理关着 → 测直连出口（照样能点） */}
        <Card>
          <CardHeader
            title="当前出口"
            description="出口 IP 显示的是 FMH 当前实际走哪条路出去 —— 开着代理经代理，关着直连。"
          />
          <CardBody className="space-y-3">
            <div className="flex flex-wrap items-center gap-x-8 gap-y-3">
              <div className="flex items-center gap-2.5">
                <Waypoints size={15} className="text-[var(--ink-3)]" />
                <div>
                  <div className="text-[11.5px] text-[var(--ink-3)]">出口 IP</div>
                  <div className="mono text-[13px] text-[var(--ink)]">
                    {(testResult?.ok && testResult.ip) || status?.egress?.ip || '未知'}
                    {(testResult?.ok && testResult.country) || status?.egress?.country ? (
                      <span className="ml-1.5 text-[12px] text-[var(--ink-3)]">
                        {(testResult?.ok && testResult.country) || status?.egress?.country}
                      </span>
                    ) : null}
                  </div>
                </div>
              </div>
              {/* 检测 —— 与出口 IP 同一行；只依赖"登录着"，不依赖代理开没开。 */}
              <Button
                size="sm"
                variant="secondary"
                onClick={doTest}
                disabled={test.isPending}
                title="重新检测出口 IP"
                className="ml-auto"
              >
                {test.isPending ? <Spinner size={13} /> : <Globe size={13} />}
                {test.isPending ? '检测中…' : '检测出口'}
              </Button>
              <div className="flex items-center gap-2.5">
                {proxy!.up ? (
                  <ShieldCheck size={15} className="text-[var(--ok)]" />
                ) : proxy!.enabled ? (
                  <XCircle size={15} className="text-[var(--bad)]" />
                ) : (
                  <XCircle size={15} className="text-[var(--ink-3)]" />
                )}
                <div>
                  <div className="text-[11.5px] text-[var(--ink-3)]">代理状态</div>
                  <div className="text-[13px] text-[var(--ink)]">
                    {!proxy!.enabled
                      ? '未启用'
                      : proxy!.up
                        ? '连通'
                        : proxy!.lastError || '不通'}
                  </div>
                </div>
              </div>
            </div>
            {/* 检测失败的原因（成功时出口 IP 就在上一行，不需要重复）。 */}
            {testResult && !testResult.ok ? (
              <Notice tone="bad">
                <div className="text-[12px]">
                  {proxy!.enabled ? '代理不通' : '出口检测失败'} —— {testResult.error || '原因未知'}
                </div>
                {proxy!.enabled ? (
                  <div className="mt-1 text-[11.5px] text-[var(--ink-3)]">
                    检查：地址/端口写对了吗、代理工具开着吗、FMH 所在的机器能不能连到它。
                  </div>
                ) : null}
              </Notice>
            ) : null}
          </CardBody>
        </Card>

        {/* 配置 */}
        <Card>
          <CardHeader
            title="代理设置"
            actions={
              <Switch
                checked={draft.enabled}
                onChange={(v) => {
                  // ⚠️ **开关即点即保存，只发 enabled** ——
                  //
                  // 二元开关没有"中间态"：拨了就是生效。
                  // 只发 enabled：地址/协议/端口可能正被编辑着，
                  // 后端 POST 字段可选合并，不带上它们就不会碰。
                  setDraft({ ...draft, enabled: v })
                  save.mutate({ enabled: v }, {
                    onSuccess: () =>
                      toast.ok(v ? '代理已启用并生效' : '代理已关闭 —— 上游流量直连'),
                    onError: (e) => toast.bad((e as Error).message),
                  })
                }}
                label="启用代理"
              />
            }
          />
          <CardBody className="space-y-4">
            {!draft.enabled ? (
              <Notice tone="accent">
                <div className="text-[12px]">
                  代理已关闭 —— 所有上游请求直连。下面的配置保留着，随时可以再开。
                </div>
              </Notice>
            ) : null}

            {/* 协议 / 地址 / 端口 —— 一行三个输入，各带自己的标签；
                地址的标签就是那句话，不再叫"代理地址"。 */}
            <div className="flex w-full items-end gap-1.5 py-2.5">
              <div className="w-[104px] shrink-0">
                <div className="text-[13px] font-medium text-[var(--ink)]">协议</div>
                <Select
                  value={draft.scheme}
                  onChange={(e) => setDraft({ ...draft, scheme: e.target.value })}
                  className="mt-2 w-full"
                >
                  {schemes.map((s) => (
                    <option key={s.value} value={s.value}>
                      {s.label}
                    </option>
                  ))}
                </Select>
              </div>
              <span className="pb-[9px] text-[14px] text-[var(--ink-3)]" aria-hidden>
                :
              </span>
              <div className="min-w-0 flex-1">
                <div className="text-[13px] font-medium text-[var(--ink)]">
                  局域网里跑着的代理工具（Clash / v2ray 等）的地址
                </div>
                <Input
                  value={draft.host}
                  onChange={(e) => setDraft({ ...draft, host: e.target.value })}
                  placeholder="代理所在机器的局域网 IP"
                  className="mt-2 w-full"
                />
              </div>
              <span className="pb-[9px] text-[14px] text-[var(--ink-3)]" aria-hidden>
                :
              </span>
              <div className="w-[90px] shrink-0">
                <div className="text-[13px] font-medium text-[var(--ink)]">端口</div>
                <Input
                  type="number"
                  min={1}
                  max={65535}
                  value={draft.port}
                  onChange={(e) => {
                    const v = Number(e.target.value) || 0
                    setDraft({ ...draft, port: v })
                  }}
                  placeholder="端口"
                  className="mt-2 w-full"
                />
              </div>
            </div>

            {/* 地址预览 */}
            {draft.enabled && address ? (
              <div className="rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface-2)] px-3 py-2 text-[12px] text-[var(--ink-2)]">
                上游流量将经
                <code className="mono mx-1 rounded bg-[var(--surface)] px-1.5 py-0.5 text-[12px]">
                  {address}
                </code>
                转发。
              </div>
            ) : null}

            <div className="text-[11.5px] leading-[17px] text-[var(--ink-3)]">
              <p>· 没有用户名/密码 —— 局域网里的代理工具通常不开认证。</p>
              <p>· 全局一个入口，所有车道共用 —— 上游按出口 IP 记额度，分多个代理只会把额度打散。</p>
              <p>· 修改立即生效 —— 已建立的连接走完就断，新连接全部走新配置。</p>
            </div>

            <div className="flex flex-wrap items-center gap-2 pt-1">
              <Button
                onClick={doSave}
                disabled={!dirty || save.isPending || (draft.enabled && (!draft.host || !draft.port))}
              >
                {save.isPending ? '保存中…' : dirty ? '保存并生效' : '已保存'}
              </Button>
            </div>
          </CardBody>
        </Card>
      </div>
    </>
  )
}
