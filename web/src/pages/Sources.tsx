import { useMemo, useState } from 'react'
import { Check, KeyRound, ShieldCheck, Stethoscope, Trash2 } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import {
  Badge,
  Button,
  Card,
  CardBody,
  CardHeader,
  Field,
  Input,
  Notice,
  Spinner,
  Switch,
} from '../components/ui'
import { LaneBadge, formatCost } from '../components/common'
import { useToast } from '../components/toast'
import { useProbeRound } from '../hooks/probeRound'
import {
  useLanes,
  useLaneSwitch,
  useProbes,
  useSaveSecret,
  useSaveSettings,
  useSecrets,
  useSettings,
  useStatus,
} from '../api/client'

/** 上游源页。 */
export function Sources() {
  const { data: status } = useStatus()
  const { data: lanes } = useLanes()
  // ⚠️ **额度在 `lanes` 里**，不在 `status` 里 ——
  //   `status` 只带车道状态，`quota` 是单独查的。
  const orQuota = lanes?.lanes.find((l) => l.id === 'openrouter')?.quota
  const { data: secrets } = useSecrets()
  const { data: settings } = useSettings()
  const { data: probes } = useProbes()
  const saveSecret = useSaveSecret()
  const saveSettings = useSaveSettings()
  const laneSwitch = useLaneSwitch()
  const toast = useToast()

  // ⚠️ **整轮探测的真实状态** ——
  //   后端是异步的，`onSuccess` 只表示"开始了"。
  const probeRound = useProbeRound((result) => {
    if (result === 'done') toast.ok('探测完成')
    else toast.bad('探测中断')
  })

  const [orKey, setOrKey] = useState('')
  const [saved, setSaved] = useState(false)

  const orSecret = secrets?.secrets.find((s) => s.name === 'openrouter_api_key')
  // ⚠️ **两条车道要用各自的状态** ——
  // 之前 zen 卡片误用了 orLane，所以显示成"未就绪"，
  // 而且模型数取错了。
  // ⚠️ **`statusLanes` 不能叫 `lanes`** —— 上面已经有 `useLanes()` 的 `lanes` 了。
  //   而且 `status?.lanes` 可能是 undefined（登录后先种缓存、
  //   真实数据还在路上），所以要给空数组兜底。
  const statusLanes = status?.lanes ?? []
  const zenLane = statusLanes.find((l) => l.id === 'zen')
  const kiloLane = statusLanes.find((l) => l.id === 'kilo')
  const orLane = statusLanes.find((l) => l.id === 'openrouter')

  // ⚠️ **出口 + 车道级封锁** ——
  // 免密车道的可用性是**网络路径的属性**（"this IP address"）。
  const egress = status?.egress
  const laneLock = useMemo(() => {
    const locks = status?.laneLocks
    if (!locks) return undefined
    // ⚠️ 只关心**免密车道** —— OpenRouter 车道不靠出口判定。
    const zen = locks['zen']
    return zen?.locked ? zen : undefined
  }, [status?.laneLocks])

  async function saveOrKey() {
    await saveSecret.mutateAsync({ name: 'openrouter_api_key', value: orKey })
    setOrKey('')
    setSaved(true)
    setTimeout(() => setSaved(false), 2000)
  }

  async function clearOrKey() {
    await saveSecret.mutateAsync({ name: 'openrouter_api_key', value: '' })
  }

  return (
    <>
      <PageTitle title="上游源" />

      {/* ⚠️ **出口 + 车道级封锁** ——
          免密车道的可用性是**网络路径的属性**，不是账号的属性。
          上游的原话："detected abusive traffic from **this IP address**"。

          所以：
          ① 面板要显示**当前从哪个 IP 出去** ——
             否则用户看到"被风控"却不知道该换什么。
          ② **所有**模型被同一种理由拒绝时，
             那不是"这 N 个模型坏了"，而是"**这个出口用不了整条车道**"。
             逐个标红会误导用户去逐个换模型 —— 真正要换的是出口。 */}
      {laneLock ? (
        <div className="mb-4 flex flex-wrap items-center gap-3 rounded-[var(--r-md)] border border-[var(--bad)] bg-[var(--bad-soft)] px-4 py-3 text-[12.5px]">
          <span className="inline-flex items-center gap-1.5 font-semibold text-[var(--bad)]">
            <span className="inline-block h-1.5 w-1.5 rounded-full bg-[var(--bad)]" />
            整条免密车道被锁
          </span>
          <span className="text-[var(--ink-2)]">{laneLock.reason}</span>
          <span className="text-[var(--ink-3)]">
            {laneLock.count} / {laneLock.total} 个模型被同一理由拒绝
          </span>
          <div className="ml-auto flex items-center gap-3">
            {egress?.ip ? (
              <span className="mono text-[11.5px] text-[var(--ink-3)]">
                出口 {egress.ip}
                {egress.country ? `（${egress.country}）` : ''}
              </span>
            ) : null}
            <span className="text-[11.5px] text-[var(--ink-3)]">换出口才有用</span>
          </div>
        </div>
      ) : null}

      {!laneLock && egress?.ip ? (
        <div className="mb-4 flex flex-wrap items-center gap-3 rounded-[var(--r-md)] border border-[var(--line)] bg-[var(--surface)] px-4 py-2.5 text-[12.5px]">
          <span className="text-[var(--ink-3)]">当前出口</span>
          <span className="mono text-[var(--ink-2)]">
            {egress.ip}
            {egress.country ? `（${egress.country}）` : ''}
          </span>
          <span className="text-[var(--ink-3)]">
            免密车道的可用性跟着这个出口走 —— 换了 VPN 或代理，能用的模型就变了。
          </span>
        </div>
      ) : null}

      {/* ⚠️ **指纹诊断已砍掉** ——
          它要发 7 次请求才能定位一个**很少发生**的问题
          （上游改了指纹规格）。
          而那 7 次请求会**真的消耗上游额度**。

          真的出问题时，「合规检查」那一栏的灯已经够用了 ——
          它明确告诉用户「上游闸门拒绝」和「出口被风控」两种可能。 */}

      <div className="space-y-4">
        {/* 免密车道 */}
        <Card>
          <CardHeader
            title={
              <span className="inline-flex items-center gap-2">
                <LaneBadge lane="zen" />
                <span>OpenCode Zen 免密车道</span>
              </span>
            }
            actions={
              <span className="inline-flex items-center gap-2">
                <Switch
                  checked={zenLane?.enabled ?? true}
                  onChange={(v) =>
                    laneSwitch.mutate(
                      { lane: 'zen', enabled: v },
                      {
                        onSuccess: () =>
                          toast.ok(v ? '免密车道已启用' : '免密车道已关闭 —— 它的模型不再参与路由'),
                        onError: (e) => toast.bad((e as Error).message),
                      },
                    )
                  }
                  label="启用 OpenCode Zen 免密车道"
                  disabled={laneSwitch.isPending}
                />
                <Badge tone={zenLane?.enabled === false ? 'idle' : zenLane?.ready ? 'ok' : 'idle'}>
                  {zenLane?.enabled === false
                    ? '已关闭'
                    : zenLane?.ready
                      ? '已启用'
                      : '未就绪'}
                </Badge>
              </span>
            }
          />
          <CardBody className="space-y-2.5 text-[12.5px] leading-[19px] text-[var(--ink-2)]">
            <div className="flex flex-wrap gap-x-6 gap-y-1">
              <span>
                模型数{' '}
                <span className="tnum font-medium text-[var(--ink)]">
                  {zenLane?.modelCount ?? 0}
                </span>
              </span>
              <span>
                凭据{' '}
                <span className="font-medium text-[var(--ink)]">无需凭据</span>
                <span className="text-[var(--ink-3)]">（共享匿名池）</span>
              </span>
              <span>
                上次刷新{' '}
                <span className="font-medium text-[var(--ink)]">
                  {formatAgo(zenLane?.lastRefresh)}
                </span>
              </span>
            </div>
            <div className="text-[var(--ink-3)]">
              ⚠ 高峰期会被限流。限流时网关会自动换模型，不需要你干预。
            </div>
          </CardBody>
        </Card>

        {/* ⚠️ **Kilo 免 key 池** ——
            这是**最省事**的一个源：标准 OpenAI 兼容，
            免费切片**完全不需要凭据**。

            ⚠️ 池子的说明**用官方原话**（见卡片底部）——
               不要自己写总结，那既说不到点上，
               又会漏掉上游的免责条款。 */}
        <Card>
          <CardHeader
            title={
              <span className="inline-flex items-center gap-2">
                <LaneBadge lane="kilo" />
                <span>Kilo 免 key 池</span>
              </span>
            }
            actions={
              <span className="inline-flex items-center gap-2">
                <Switch
                  checked={kiloLane?.enabled ?? true}
                  onChange={(v) =>
                    laneSwitch.mutate(
                      { lane: 'kilo', enabled: v },
                      {
                        onSuccess: () =>
                          toast.ok(v ? 'Kilo 车道已启用' : 'Kilo 车道已关闭'),
                        onError: (e) => toast.bad((e as Error).message),
                      },
                    )
                  }
                  label="启用 Kilo 免 key 池"
                  disabled={laneSwitch.isPending}
                />
                <Badge
                  tone={kiloLane?.enabled === false ? 'idle' : kiloLane?.ready ? 'ok' : 'idle'}
                >
                  {kiloLane?.enabled === false
                    ? '已关闭'
                    : kiloLane?.ready
                      ? '已启用'
                      : '未就绪'}
                </Badge>
              </span>
            }
          />
          <CardBody className="space-y-2.5 text-[12.5px] leading-[19px] text-[var(--ink-2)]">
            <div className="flex flex-wrap gap-x-6 gap-y-1">
              <span>
                模型数{' '}
                <span className="tnum font-medium text-[var(--ink)]">
                  {kiloLane?.modelCount ?? 0}
                </span>
              </span>
              <span>
                凭据{' '}
                <span className="font-medium text-[var(--ink)]">无需凭据</span>
                <span className="text-[var(--ink-3)]">（标准 OpenAI 兼容）</span>
              </span>
              <span>
                上次刷新{' '}
                <span className="font-medium text-[var(--ink)]">
                  {formatAgo(kiloLane?.lastRefresh)}
                </span>
              </span>
            </div>
            {/* ⚠️ **用 Kilo 官方的说法**，不要自己写总结。

                官方文档（kilo.ai/docs/gateway/models-and-providers）对
                `kilo-auto/free` 的原文是：

                    "Free with limited capability. No credits required.
                     The resolved model is selected dynamically per session
                     from a curated set of available free models; the mapping
                     updates server-side as free model availability shifts."

                而"数据处理"那段官方明确警告：

                    "Auto Free may route your requests to providers that log
                     prompts and outputs and use them to improve their services.
                     Do not submit personal or confidential data when using Auto Free."

                ⚠️ **我们不该替上游改写它的条款** ——
                   用官方原话最安全，也不会漏掉免责。 */}
            <div className="rounded-[var(--r-sm)] border border-[var(--line-soft)] bg-[var(--surface-2)] px-3 py-2 text-[11.5px] leading-[17px] text-[var(--ink-3)]">
              <b className="text-[var(--ink-2)]">Kilo 官方说明</b>
              <div className="mt-1">
                “Free with limited capability. No credits required. The resolved model is
                selected dynamically per session from a curated set of available free
                models; the mapping updates server-side as free model availability shifts.”
              </div>
              <div className="mt-1.5 text-[var(--warn)]">
                ⚠ Auto Free may route your requests to providers that log prompts and
                outputs and use them to improve their services. Do not submit personal or
                confidential data when using Auto Free.
              </div>
              <a
                className="mt-1.5 inline-block underline decoration-dotted"
                href="https://kilo.ai/docs/gateway/models-and-providers"
                target="_blank"
                rel="noreferrer"
              >
                kilo.ai/docs/gateway/models-and-providers
              </a>
            </div>
          </CardBody>
        </Card>

        {/* OpenRouter */}
        <Card>
          <CardHeader
            title={
              <span className="inline-flex items-center gap-2">
                <LaneBadge lane="openrouter" />
                <span>OpenRouter 免费模型</span>
              </span>
            }
            actions={
              <span className="inline-flex items-center gap-2">
                <Switch
                  checked={orLane?.enabled ?? true}
                  onChange={(v) =>
                    laneSwitch.mutate(
                      { lane: 'openrouter', enabled: v },
                      {
                        onSuccess: () =>
                          toast.ok(v ? 'OpenRouter 车道已启用' : 'OpenRouter 车道已关闭'),
                        onError: (e) => toast.bad((e as Error).message),
                      },
                    )
                  }
                  label="启用 OpenRouter 车道"
                  disabled={laneSwitch.isPending}
                />
                <Badge
                  tone={orLane?.enabled === false ? 'idle' : orLane?.ready ? 'ok' : 'warn'}
                >
                  {orLane?.enabled === false
                    ? '已关闭'
                    : orLane?.ready
                      ? '已启用'
                      : '未配置 Key'}
                </Badge>
              </span>
            }
          />
          <CardBody className="space-y-4">
            <div className="flex flex-wrap gap-x-6 gap-y-1 text-[12.5px] text-[var(--ink-2)]">
              <span>
                模型数{' '}
                <span className="tnum font-medium text-[var(--ink)]">{orLane?.modelCount ?? 0}</span>
              </span>
              <span>
                上次刷新{' '}
                <span className="font-medium text-[var(--ink)]">
                  {formatAgo(orLane?.lastRefresh)}
                </span>
              </span>
              {orQuota ? (
                <span>
                  剩余额度{' '}
                  <span className="tnum font-medium text-[var(--ink)]">
                    {/* ⚠️ 小额要多给几位小数 ——
                        $0.01 的额度显示成 "$0.00" 会让人以为"没额度了"。 */}
                    {formatCost(orQuota.remaining)}
                  </span>
                  {orQuota.usage > 0 ? (
                    <span className="text-[var(--ink-3)]">
                      {' '}
                      （已用 {formatCost(orQuota.usage)}）
                    </span>
                  ) : null}
                </span>
              ) : null}
            </div>

            {/* ⚠️ **为什么 OR 车道显示「未探测」** ——

                这不是 bug，是**故意的**。

                实测（2026-10-20）：一轮 18 个 OR 模型就把当天的
                `free-models-per-day` 配额**打满了**，剩下的全是
                "Rate limit exceeded: free-models-per-day"。

                而那是**用户自己的额度**。拿它做「每 15 分钟全量探测」
                就是在**花用户的钱**。

                所以 OR 车道**不参与自动探测**：
                它的可用性由**真实流量**验证 ——
                局域网里的 agent 真的调了、通了，
                面板上的「实际服务」列就有记录。

                想主动验证某个 OR 模型？模型清单里点「探测」，
                那一次是**你**主动花的。 */}
            {orLane && orLane.autoProbe === false ? (
              <div className="rounded-[var(--r-sm)] border border-[var(--line-soft)] bg-[var(--surface-2)] px-3 py-2 text-[11.5px] leading-[17px] text-[var(--ink-3)]">
                ℹ️ <b className="text-[var(--ink-2)]">这条车道不参与自动探测</b>
                {orLane.autoProbeReason ? ` —— ${orLane.autoProbeReason}` : '。'}
                想主动测某个模型，在模型清单点「探测」即可。
              </div>
            ) : null}

            <Field
              label="OpenRouter API Key"
              hint="只保存在你的 NAS 上，永远不会下发给客户端，管理台也只显示指纹。"
            >
              <div className="flex flex-wrap items-center gap-2">
                <Input
                  type="password"
                  value={orKey}
                  onChange={(e) => setOrKey(e.target.value)}
                  placeholder={
                    orSecret?.set ? `已配置（${orSecret.hint}）—— 留空表示不改` : 'sk-or-v1-…'
                  }
                  className="max-w-sm"
                  autoComplete="off"
                />
                <Button
                  variant="primary"
                  onClick={saveOrKey}
                  disabled={!orKey.trim() || saveSecret.isPending}
                >
                  {saved ? <Check size={14} /> : <KeyRound size={14} />}
                  {saved ? '已保存' : '保存'}
                </Button>
                {orSecret?.set ? (
                  <Button
                    variant="ghost"
                    onClick={clearOrKey}
                    disabled={saveSecret.isPending}
                    aria-label="清除 Key"
                  >
                    <Trash2 size={14} />
                    清除
                  </Button>
                ) : null}
              </div>
              {saveSecret.isError ? (
                <div className="mt-2 text-[12px] text-[var(--bad)]">
                  保存失败：{(saveSecret.error as Error).message}
                </div>
              ) : null}
            </Field>

            {orSecret?.set ? (
              <div className="inline-flex items-center gap-1.5 text-[12px] text-[var(--ok)]">
                <ShieldCheck size={13} />
                已配置 · 指纹 {orSecret.hint}
              </div>
            ) : (
              <Notice tone="warn">
                还没配置 OpenRouter Key。没有它，OpenRouter 车道不可用 —— 但免密车道仍然工作。
              </Notice>
            )}
          </CardBody>
        </Card>

        {/* 探测 */}
        <Card>
          <CardHeader
            title="探测"
            description="定期发一个最小请求，确认模型是不是真的可用。"
          />
          <CardBody className="space-y-3">
            <div className="flex flex-wrap items-center gap-3 text-[12.5px]">
              <span className="text-[var(--ok)]">可用 {probes?.summary.available ?? 0}</span>
              <span className="text-[var(--warn)]">异常 {probes?.summary.failed ?? 0}</span>
              <span className="text-[var(--ink-3)]">未探测 {probes?.summary.unknown ?? 0}</span>
              <span className="ml-auto text-[var(--ink-3)]">
                共批量探测 {probes?.summary.probed ?? 0} 个模型
              </span>
            </div>
            <div className="text-[12.5px] leading-[19px] text-[var(--ink-3)]">
              ⚠ 探测会消耗上游额度 —— 每探一个模型就是一次真实推理。
            </div>
            {/* ⚠️ **正在探测时的进度** ——
                一整轮可能几十秒。 */}
            {probeRound.running ? (
              <div className="flex flex-wrap items-center gap-3 rounded-[var(--r-sm)] border border-[var(--accent-line)] bg-[var(--accent-soft)] px-3 py-2 text-[12.5px]">
                <span className="inline-flex items-center gap-1.5 font-medium text-[var(--accent)]">
                  <Spinner size={13} />
                  {probeRound.finalizing ? '正在复核' : '正在探测'}
                </span>
                <span className="tnum text-[var(--ink-2)]">
                  {probeRound.done} / {probeRound.total || '…'}
                </span>
                <div className="h-1.5 min-w-[100px] flex-1 overflow-hidden rounded-full bg-[var(--accent-line)]">
                  <div
                    className="h-full rounded-full bg-[var(--accent)] transition-[width] duration-300"
                    style={{
                      width: probeRound.total
                        ? `${Math.round((probeRound.done / probeRound.total) * 100)}%`
                        : '30%',
                    }}
                  />
                </div>
              </div>
            ) : null}
            <Button
              onClick={() => {
                toast.info('正在探测全部模型…（每个模型一次真实推理，会花上游额度）')
                probeRound.start()
              }}
              disabled={probeRound.running}
              title="会消耗上游额度"
            >
              {probeRound.running ? <Spinner size={14} /> : <Stethoscope size={14} />}
              {probeRound.running
                ? probeRound.finalizing
                  ? '正在复核'
                  : '正在探测'
                : '立即探测一轮'}
            </Button>
          </CardBody>
        </Card>

        {/* 自动跨模型调用 */}
        <Card>
          <CardHeader
            title="自动跨模型调用"
            description="当前模型限流或不可用时，自动改用其他可用模型。"
          />
          <CardBody>
            <div className="flex items-start justify-between gap-4">
              <div className="min-w-0 text-[12.5px] leading-[19px] text-[var(--ink-2)]">
                {settings?.autoModelSwitch
                  ? '切换顺序：Kilo ↔ Zen（免密，不花你的额度），两条都不行才用 OpenRouter。'
                  : '已关闭 —— 只重试当前模型，重试仍不行就直接报错。'}
                {settings?.autoModelSwitch ? (
                  <div className="mt-1.5 text-[var(--warn)]">
                    切到 OpenRouter 的那部分用你的 OpenRouter 额度。
                  </div>
                ) : null}
              </div>
              <Button
                variant={settings?.autoModelSwitch ? 'primary' : 'secondary'}
                onClick={() =>
                  saveSettings.mutate({
                    autoModelSwitch: !settings?.autoModelSwitch,
                  })
                }
                disabled={!settings}
              >
                {settings?.autoModelSwitch ? '已开启' : '已关闭'}
              </Button>
            </div>
          </CardBody>
        </Card>
      </div>
    </>
  )
}

/** 格式化"多久之前"。 */
function formatAgo(ts?: number): string {
  if (!ts) return '—'
  const diff = Date.now() - ts
  if (diff < 0) return '刚刚'
  const min = Math.floor(diff / 60_000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const hour = Math.floor(min / 60)
  if (hour < 24) return `${hour} 小时前`
  return `${Math.floor(hour / 24)} 天前`
}
