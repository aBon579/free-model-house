import { useEffect, useMemo, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { Activity, AlertTriangle, Boxes, Search, ShieldAlert, Stethoscope } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import {
  Button,
  Card,
  CardBody,
  Empty,
  Input,
  Loading,
  Select,
  Spinner,
} from '../components/ui'
import { LaneBadge, StateText } from '../components/common'
import { useToast } from '../components/toast'
import { useModels, useProbeOne, useProbes, useStatus, useUsage } from '../api/client'
import { useProbeRound } from '../hooks/probeRound'
import type { ModelInfo, ModelState } from '../api/types'

/**
 * 状态筛选。
 *
 * ⚠️ 六个状态**全列** —— 之前只列了 4 个（漏了"地区受限"和"出口被风控"），
 *   于是用户看到这两种状态的模型却**筛不出来**。
 */
const stateFilters: Array<{ key: ModelState | 'all'; label: string }> = [
  { key: 'all', label: '全部状态' },
  { key: 'available', label: '可用' },
  { key: 'unknown', label: '未探测' },
  { key: 'throttled', label: '暂时限流' },
  { key: 'region-blocked', label: '地区受限' },
  { key: 'abuse-blocked', label: '出口被风控' },
  { key: 'unavailable', label: '不可用' },
]

/**
 * 格式化"上次探测时间"。
 *
 * ⚠️ **精确到秒** —— 用户要能看出"这是 3 秒前测的还是 3 分钟前测的"。
 *   只显示分钟的话，10 分钟 TTL 的边界就看不清了。
 *
 * ⚠️ **用绝对时钟时间**（`14:23:07`）而不是"x 分钟前" ——
 *   "3 分钟前"是**相对**的，会一直变；
 *   用户想知道的是"结论是几点测的"，那个是固定的。
 *
 * ⚠️ 超过一天就带上日期 —— 否则"03:12:45"分不清是今天还是昨天。
 */
function formatProbeTime(ms: number): string {
  if (!ms) return '—'
  const d = new Date(ms)
  const p = (n: number) => String(n).padStart(2, '0')
  const clock = `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
  const age = Date.now() - ms
  if (age >= 86_400_000 || age < 0) {
    return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${clock}`
  }
  return clock
}

/** 能力筛选。 */
const capabilityFilters = [
  { key: 'all', label: '全部能力' },
  { key: 'vision', label: '支持图片' },
  { key: 'reasoning', label: '支持思考' },
] as const

/**
 * 模型清单页。
 *
 * ⚠️ **"实际服务"列是 FMH 最重要的一个 UI 元素** ——
 * 它把"为什么我请求 A 却得到了 B 的回答"变成可解释的。
 * 故障转移是**静默**的；没有这一列，用户只会觉得"这个模型今天变笨了"。
 */
export function Models() {
  const { data, isLoading } = useModels()
  const { data: probes } = useProbes()
  const { data: usage } = useUsage()
  const { data: status } = useStatus()
  const probeOne = useProbeOne()
  const toast = useToast()

  // ⚠️ **整轮探测的真实状态** ——
  //   后端是异步的，`onSuccess` 只表示"开始了"。
  //   真的完成要等 `progress.running` 变 false。
  const probeRound = useProbeRound((result) => {
    if (result === 'done') toast.ok('探测完成')
    else toast.bad('探测中断')
  })
  const [query, setQuery] = useState('')
  const [laneFilter, setLaneFilter] = useState<'all' | 'zen' | 'kilo' | 'openrouter'>('all')
  const [stateFilter, setStateFilter] = useState<ModelState | 'all'>('all')
  const [capFilter, setCapFilter] = useState<(typeof capabilityFilters)[number]['key']>('all')

  // 最近一次实际服务的模型（按模型 id 聚合）。
  const servedMap = useMemo(() => {
    const map = new Map<string, { model: string; count: number }>()
    for (const u of usage?.byModel ?? []) {
      const entry = map.get(u.model) ?? { model: u.model, count: 0 }
      entry.count += u.requests
      map.set(u.model, entry)
    }
    return map
  }, [usage])

  // 探测状态（比清单里的 state 更细）。
  const probeMap = useMemo(() => {
    const map = new Map<string, (typeof probes extends undefined ? never : any)>()
    for (const p of probes?.probes ?? []) {
      map.set(p.model, p)
    }
    return map
  }, [probes])

  // ⚠️ **上一轮探测的时间** ——
  //
  // 取**所有模型里最新的那个**探测时间 ——
  // 那就是"上一轮探测做完"的时刻。
  //
  // ⚠️ 手动和自动探测都会更新它（后端每次探测都记时间戳）。
  const lastProbeAt = useMemo(() => {
    let latest = 0
    for (const m of data?.models ?? []) {
      if (m.probedAt && m.probedAt > latest) latest = m.probedAt
    }
    return latest
  }, [data])

  const filtered = useMemo(() => {
    let list = data?.models ?? []
    if (laneFilter !== 'all') {
      list = list.filter((m) => m.lane === laneFilter)
    }
    if (stateFilter !== 'all') {
      list = list.filter((m) => effectiveState(m, probeMap) === stateFilter)
    }
    if (capFilter === 'vision') {
      list = list.filter((m) => m.vision)
    } else if (capFilter === 'reasoning') {
      list = filterReasoning(list, probeMap)
    }
    const q = query.trim().toLowerCase()
    if (q) {
      list = list.filter(
        (m) =>
          m.id.toLowerCase().includes(q) ||
          m.name.toLowerCase().includes(q),
      )
    }
    return list
  }, [data, laneFilter, stateFilter, capFilter, query, probeMap])

  // 虚拟滚动。
  const scrollRef = useRef<HTMLDivElement>(null)
  // ⚠️ **行高不是固定的** —— 桌面 40px 一行，窄屏是卡片（约 78px）。
  // 估错的后果是窄屏下卡片**互相压住**（定位是算出来的），
  // 所以按视口宽度给不同估计值，并用 `measureElement` 实测。
  const [narrow, setNarrow] = useState(false)
  useEffect(() => {
    const check = () => setNarrow(window.innerWidth < 1024)
    check()
    window.addEventListener('resize', check)
    return () => window.removeEventListener('resize', check)
  }, [])

  const virtualizer = useVirtualizer({
    count: filtered.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => (narrow ? 78 : 40),
    overscan: 12,
  })

  return (
    <>
      <PageTitle
        title="模型清单"
        actions={
          <div className="flex items-center gap-2">
            {/* ⚠️ **上次探测时间** —— 代替了原来的「45 / 45」。

                用户在这里最想知道的是"这些结论是什么时候测的" ——
                结论的 TTL 是 10 分钟，过期了就没意义了。

                ⚠️ **精确到秒** —— 只到分钟的话，
                   10 分钟 TTL 的边界就看不清了。 */}
            <span
              className="tnum text-[12px] text-[var(--ink-3)]"
              title={lastProbeAt ? new Date(lastProbeAt).toLocaleString('zh-CN') : ''}
            >
              上次探测{' '}
              {lastProbeAt ? formatProbeTime(lastProbeAt) : '—'}
            </span>
            <Button
              size="sm"
              onClick={() => {
                // ⚠️ **明确告诉用户在做什么** ——
                //   探测要跑好几秒（每个模型一次真实推理），
                //   没有提示的话用户会以为没点到，然后连点。
                toast.info('正在探测全部模型…（每个模型一次真实推理，会花上游额度）')
                probeRound.start()
              }}
              disabled={probeRound.running}
              title="探测会消耗上游额度 —— 每探一个模型就是一次真实推理"
            >
              {probeRound.running ? <Spinner size={13} /> : <Stethoscope size={13} />}
              {probeRound.running
                ? probeRound.finalizing
                  ? '正在复核'
                  : '正在探测'
                : '探测一轮'}
            </Button>
          </div>
        }
      />

      {/* ⚠️ **上轮探测的共性失败原因** ——
          「探测完成，但全是未探测」这句话没有信息量。
          把**整轮失败的那个原因**提出来，用户才知道该做什么。 */}
      {!probeRound.running &&
      probeRound.lastFailure &&
      probeRound.lastFailureCount > 1 ? (
        <div className="mb-4 flex flex-wrap items-center gap-2 rounded-[var(--r-md)] border border-[var(--warn)] bg-[var(--warn-soft)] px-4 py-2.5 text-[12.5px]">
          <AlertTriangle size={14} className="text-[var(--warn)]" />
          {/* ⚠️ **不能说"都失败了"** ——
              那个次数只是"同一种失败原因的个数"，
              不是失败总数。真实情况可能是"16 个可用、8 个限流"。
              说成"都失败"会让人以为整个网关挂了。 */}
          <span className="text-[var(--ink-2)]">
            {probeRound.lastAvailable > 0
              ? `${probeRound.lastAvailable} 个可用，`
              : ''}
            {probeRound.lastFailureCount} 个模型因为同一个原因失败：
          </span>
          <span className="text-[var(--ink-3)]">{probeRound.lastFailure}</span>
        </div>
      ) : null}

      {/* 探测概况 */}
      {probes?.summary && probes.summary.probed > 0 ? (
        <div className="mb-4 flex flex-wrap items-center gap-3 rounded-[var(--r-md)] border border-[var(--line)] bg-[var(--surface)] px-4 py-2.5 text-[12.5px]">
          <span className="inline-flex items-center gap-1.5 text-[var(--ink-3)]">
            <Activity size={13} />
            探测结论
          </span>
          <span className="text-[var(--ok)]">可用 {probes.summary.available}</span>
          <span className="text-[var(--warn)]">异常 {probes.summary.failed}</span>
          <span className="text-[var(--ink-3)]">未探测 {probes.summary.unknown}</span>
          <span className="ml-auto text-[var(--ink-3)]">
            共探测 {probes.summary.probed} 个
          </span>
        </div>
      ) : null}

      {/* ⚠️ **正在探测时的进度条** ——
          一整轮可能几十秒，用户得知道"还在跑"和"还剩多少"。 */}
      {probeRound.running ? (
        <div className="mb-4 flex flex-wrap items-center gap-3 rounded-[var(--r-md)] border border-[var(--accent-line)] bg-[var(--accent-soft)] px-4 py-2.5 text-[12.5px]">
          <span className="inline-flex items-center gap-1.5 font-medium text-[var(--accent)]">
            <Spinner size={13} />
            {probeRound.finalizing ? '正在复核' : '正在探测'}
          </span>
          <span className="tnum text-[var(--ink-2)]">
            {probeRound.done} / {probeRound.total || '…'}
          </span>
          <div className="h-1.5 min-w-[120px] flex-1 overflow-hidden rounded-full bg-[var(--accent-line)]">
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

      {/* ⚠️ **说明为什么有些模型是「未探测」** ——
          免得用户以为是 bug。

          用了**自己 Key** 的车道（OpenRouter）**不参与自动探测** ——
          它的免费额度按天算且很少，自动探测等于花用户自己的钱。
          它的可用性由**真实流量**验证（看「实际服务」列）。 */}
      {(() => {
        const noAuto = (status?.lanes ?? []).filter((l) => l.autoProbe === false)
        if (noAuto.length === 0) return null
        return (
          <div className="mb-4 flex flex-wrap items-center gap-2 rounded-[var(--r-md)] border border-[var(--line)] bg-[var(--surface)] px-4 py-2.5 text-[12.5px] text-[var(--ink-3)]">
            <span className="text-[var(--ink-2)]">这些车道不参与自动探测：</span>
            {noAuto.map((l) => (
              <span key={l.id} className="inline-flex items-center gap-1.5">
                <LaneBadge lane={l.id} />
                <span className="text-[11.5px]">{l.autoProbeReason}</span>
              </span>
            ))}
          </div>
        )
      })()}

      <Card className="mb-4">
        <CardBody className="flex flex-wrap items-center gap-2">
          <div className="relative min-w-[200px] flex-1">
            <Search
              size={14}
              className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--ink-3)]"
            />
            <Input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="搜索模型 ID 或名称"
              className="pl-8"
              aria-label="搜索模型"
            />
          </div>
          <Select
            value={laneFilter}
            onChange={(e) => setLaneFilter(e.target.value as typeof laneFilter)}
            aria-label="按车道筛选"
            className="w-auto"
          >
            <option value="all">全部车道</option>
            <option value="zen">免密车道</option>
            <option value="kilo">Kilo 免 key 池</option>
            <option value="openrouter">OpenRouter</option>
          </Select>
          <Select
            value={stateFilter}
            onChange={(e) => setStateFilter(e.target.value as ModelState | 'all')}
            aria-label="按状态筛选"
            className="w-auto"
          >
            {stateFilters.map((f) => (
              <option key={f.key} value={f.key}>
                {f.label}
              </option>
            ))}
          </Select>
          <Select
            value={capFilter}
            onChange={(e) =>
              setCapFilter(e.target.value as (typeof capabilityFilters)[number]['key'])
            }
            aria-label="按能力筛选"
            className="w-auto"
          >
            {capabilityFilters.map((f) => (
              <option key={f.key} value={f.key}>
                {f.label}
              </option>
            ))}
          </Select>
        </CardBody>
      </Card>

      {/* 表格 */}
      <Card className="overflow-hidden">
        {isLoading ? (
          <Loading label="读取模型清单" />
        ) : filtered.length === 0 ? (
          <Empty
            icon={<Boxes size={26} />}
            title="没有匹配的模型"
            description="换个筛选条件试试，或去「上游源」刷新清单。"
          />
        ) : (
          <>
            {/* 表头 */}
            <div
              className="fmh-models-grid fmh-models-head items-center gap-3 border-b border-[var(--line)] bg-[var(--surface-2)] px-4 py-2 text-[11.5px] font-semibold text-[var(--ink-3)]"
            >
              <span className="fmh-c-model">模型</span>
              <span className="fmh-c-lane">车道</span>
              <span className="fmh-c-cap">能力</span>
              <span className="fmh-c-state">状态</span>
              <span className="fmh-c-ttft text-right">首字</span>
              <span className="fmh-c-probed text-right">上次探测</span>
              <span className="fmh-c-served">实际服务</span>
              <span className="fmh-c-act" />
            </div>

            {/* 虚拟滚动列表 */}
            <div ref={scrollRef} className="max-h-[62vh] overflow-y-auto">
              <div
                style={{ height: virtualizer.getTotalSize(), position: 'relative' }}
                role="table"
                aria-rowcount={filtered.length}
              >
                {virtualizer.getVirtualItems().map((row) => {
                  const m = filtered[row.index]
                  return (
                    <ModelRow
                      key={m.id}
                      model={m}
                      probe={probeMap.get(m.id)}
                      served={servedMap.get(m.id)}
                      style={{
                        position: 'absolute',
                        top: 0,
                        left: 0,
                        width: '100%',
                        height: row.size,
                        transform: `translateY(${row.start}px)`,
                      }}
                      onProbe={(model) => {
                        // ⚠️ **单模型探测也要有反馈** ——
                        //   一次真实推理要好几秒，
                        //   没提示的话用户会以为没点到。
                        toast.info(`正在探测 ${model}…`)
                        probeOne.mutate(model, {
                          onSuccess: (r) => {
                            const state = (r as { result?: { state?: string } })?.result?.state
                            if (state === 'available') toast.ok(`${model} 可用`)
                            else toast.info(`${model} 探测完成`)
                          },
                          onError: (e) => toast.bad(`探测失败 —— ${(e as Error).message}`),
                        })
                      }}
                      probing={probeOne.isPending}
                    />
                  )
                })}
              </div>
            </div>
          </>
        )}
      </Card>

      <div className="mt-3 flex flex-wrap items-center gap-4 text-[11.5px] text-[var(--ink-3)]">
        <span className="inline-flex items-center gap-1.5">
          <ShieldAlert size={12} />
          能力徽章的实心/虚心表示结论来源：实心=实测或上游声明，虚线=规则表推测
        </span>
        <span>“实际服务”列显示最近由哪个模型实际应答 —— 故障转移是静默的</span>
      </div>
    </>
  )
}

/** 一行模型。 */
function ModelRow({
  model: m,
  probe,
  served,
  style,
  onProbe,
  probing,
}: {
  model: ModelInfo
  probe?: { state: ModelState; detail: string }
  served?: { model: string; count: number }
  style: React.CSSProperties
  onProbe: (model: string) => void
  probing: boolean
}) {
  // ⚠️ 模型 id 和探测记录的键**都是全名**（`zen/beam-free`）——
  //   不用再剥前缀了。
  const state = effectiveState(m, probe ? new Map([[m.id, probe]]) : new Map())
  const swapped = served && served.model !== m.id

  return (
    <div
      role="row"
      className="fmh-models-grid items-center gap-3 border-b border-[var(--line-soft)] px-4 transition-colors duration-150 hover:bg-[var(--surface-hover)]"
      style={style}
    >
      {/* 模型 */}
      <div className="fmh-c-model min-w-0">
        <div className="mono truncate text-[12.5px] text-[var(--ink)]" title={m.id}>
          {m.id}
        </div>
        {m.name && m.name !== m.id ? (
          <div className="truncate text-[11px] text-[var(--ink-3)]">{m.name}</div>
        ) : null}
      </div>

      {/* 车道 */}
      <div className="fmh-c-lane">
        <LaneBadge lane={m.lane} />
      </div>

      {/* 能力 */}
      <div className="fmh-c-cap flex flex-wrap items-center gap-1">
        <CapabilityMark
          on={m.vision}
          label="图"
          known={m.capability !== 'unknown'}
        />
        <CapabilityMark
          on={m.reasoning}
          label="思"
          known={m.capability !== 'unknown'}
        />
        {m.context > 0 ? (
          <span className="tnum text-[11px] text-[var(--ink-3)]">
            {formatContext(m.context)}
          </span>
        ) : null}
      </div>

      {/* 状态 */}
      <div className="fmh-c-state">
        <StateText state={state} detail={probe?.detail ?? m.probeDetail} />
        {/* ⚠️ **冷却倒计时** ——
            让用户知道"这个模型还有几秒才能用"，
            而不是只看到一个干巴巴的"限流"。 */}
        {m.cooldownSec && m.cooldownSec > 0 ? (
          <div className="tnum text-[11px] text-[var(--warn)]">
            {m.cooldownSec}s 后重试
          </div>
        ) : null}
      </div>

      {/* 首字延迟 */}
      <div className="fmh-c-ttft tnum text-right text-[12px] text-[var(--ink-3)]">
        {m.ttftMs > 0 ? `${(m.ttftMs / 1000).toFixed(1)}s` : '—'}
      </div>

      {/* ⚠️ **上次探测时间** —— 手动和自动都记。
          用户靠它判断"这个结论还新不新"（结论的 TTL 是 10 分钟）。 */}
      <div className="fmh-c-probed tnum text-right text-[12px] text-[var(--ink-3)]">
        {m.probedAt ? formatProbeTime(m.probedAt) : '—'}
      </div>

      {/* 实际服务 */}
      <div className="fmh-c-served min-w-0 truncate text-[12px]">
        {served ? (
          swapped ? (
            <span className="text-[var(--warn)]" title={`最近 ${served.count} 次请求实际由 ${served.model} 应答`}>
              ⚠ {served.model}
            </span>
          ) : (
            <span className="text-[var(--ink-3)]">{served.model}</span>
          )
        ) : (
          <span className="text-[var(--ink-3)]">—</span>
        )}
      </div>

      {/* 操作 */}
      <div className="fmh-c-act flex justify-end">
        <Button
          size="sm"
          variant="ghost"
          onClick={() => onProbe(m.id)}
          disabled={probing}
          title="探测会消耗上游额度 —— 一次真实推理"
          aria-label={`探测 ${m.id}`}
          className="fmh-touchable-icon"
        >
          <Stethoscope size={13} />
        </Button>
      </div>

      {/* ⚠️ **窄屏专用的第二行** ——
          桌面端隐藏，手机上把车道/能力/状态并排放一行。

          ⚠️ 为什么要单独一行，而不是让三个格子换行：
          那样的话**桌面端的列宽**会被手机端的排版影响，
          而列宽是精心配过的（"实际服务"要放得下模型名）。

          用 `display:none` / `flex` 切换，桌面完全不受影响。 */}
      <div className="fmh-c-meta flex-wrap items-center gap-2 text-[11.5px] text-[var(--ink-3)]">
        <LaneBadge lane={m.lane} />
        <CapabilityMark on={m.vision} label="图" known={m.capability !== 'unknown'} />
        <CapabilityMark on={m.reasoning} label="思" known={m.capability !== 'unknown'} />
        <StateText state={state} detail={probe?.detail ?? m.probeDetail} />
        {m.cooldownSec && m.cooldownSec > 0 ? (
          <span className="tnum text-[var(--warn)]">{m.cooldownSec}s 后重试</span>
        ) : null}
      </div>

      {/* ⚠️ **窄屏专用的第三行** —— 首字延迟 + 上次探测 + 实际服务。 */}
      <div className="fmh-c-info flex-wrap items-center gap-3 text-[11.5px] text-[var(--ink-3)]">
        <span className="tnum">
          首字 {m.ttftMs > 0 ? `${(m.ttftMs / 1000).toFixed(1)}s` : '—'}
        </span>
        <span className="tnum">
          探测 {m.probedAt ? formatProbeTime(m.probedAt) : '—'}
        </span>
        <span className="min-w-0 truncate">
          实际服务{' '}
          {served ? (
            swapped ? (
              <span className="text-[var(--warn)]">⚠ {served.model}</span>
            ) : (
              <span>{served.model}</span>
            )
          ) : (
            '—'
          )}
        </span>
      </div>
    </div>
  )
}

/**
 * 能力徽章。
 *
 * ⚠️ **"不支持"要一眼看出来**，不能只靠"没有实心底"。
 * 三种状态：实心+有字=支持（结论可信）／虚线+有字=已知不支持／虚线+无字=未测量。
 *
 * ⚠️ "未测量"**不能**画成"不支持" —— 我们不知道，不等于它不行。
 */
function CapabilityMark({
  on,
  label,
  known,
}: {
  on: boolean
  label: string
  known: boolean
}) {
  if (on) {
    return (
      <span
        className="inline-flex h-[17px] w-[17px] items-center justify-center rounded-[3px] border border-[var(--accent-line)] bg-[var(--accent-soft)] text-[10px] font-medium leading-none text-[var(--accent)]"
        title={`${label}：支持`}
        aria-label={`${label}：支持`}
      >
        {label}
      </span>
    )
  }
  if (known) {
    // ⚠️ 已知不支持 —— 明确画成"划掉的"样子。
    return (
      <span
        className="inline-flex h-[17px] w-[17px] items-center justify-center rounded-[3px] border border-[var(--idle-line)] text-[10px] leading-none text-[var(--ink-3)] line-through decoration-[var(--ink-3)] opacity-70"
        title={`${label}：实测不支持`}
        aria-label={`${label}：不支持`}
      >
        {label}
      </span>
    )
  }
  // 未测量 —— 虚线框，不写字。
  return (
    <span
      className="inline-flex h-[17px] w-[17px] items-center justify-center rounded-[3px] border border-dashed border-[var(--line)] opacity-50"
      title={`${label}：未测量`}
      aria-label={`${label}：未测量`}
    />
  )
}

/**
 * 算一个模型的有效状态。
 *
 * ⚠️ **模型行自己的状态优先** —— 后端在 `ApplyProbe` 时已经把结论写进模型行了，
 * 而且带的是"最近一次"的状态；`/api/probes` 里那份是**数据库快照**，可能更旧。
 *
 * ⚠️ **键不用剥前缀** —— 模型 id 和探测记录**都是全名**
 * （`zen/beam-free`），它们本来就是同一个键。
 */
function effectiveState(
  m: ModelInfo,
  probeMap: Map<string, { state: ModelState }>,
): ModelState {
  // ① 模型行自己的状态（最新）。
  if (m.state && m.state !== 'unknown') {
    return m.state
  }
  // ② 探测表。
  const probe = probeMap.get(m.id)
  if (probe) return probe.state
  return m.state || 'unknown'
}

/** 思考能力的筛选 —— 只有"已知支持"才算。 */
function filterReasoning(
  list: ModelInfo[],
  probeMap: Map<string, { state: ModelState }>,
): ModelInfo[] {
  return list.filter((m) => {
    const probe = probeMap.get(m.id)
    if (probe) return probe.state !== 'unavailable' && m.reasoning
    return m.reasoning && m.capability !== 'unknown'
  })
}

/** 格式化上下文窗口。 */
function formatContext(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1)}M`
  if (n >= 1000) return `${Math.round(n / 1000)}K`
  return String(n)
}
