import { useMemo, useState } from 'react'
import { Activity } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import { Card, CardBody, CardHeader, Empty, Loading, Stat, cn } from '../components/ui'
import { LaneBadge, formatCost } from '../components/common'
import { useUsage } from '../api/client'

/** 时间范围。 */
const ranges = [
  { days: 7, label: '7 天' },
  { days: 14, label: '14 天' },
  { days: 30, label: '30 天' },
]

/** 用量页。 */
export function Usage() {
  const { data, isLoading } = useUsage()
  const [range, setRange] = useState(30)

  const daily = useMemo(() => data?.daily ?? [], [data])

  /**
   * 热力图数据 —— **固定 365 格，永远显示**。
   *
   * ⚠️ **从去年的今天排到今天** —— 最后一格永远是今天；
   *   没有用量记录时也是 365 个灰格（那是信息："一年没用"），
   *   不能因为没有记录就整块消失。
   *
   * ⚠️ **maxTokens 只按有数据的天算** ——
   *   把 0 的天也算进 max 会把比例压低（0 永远不是最大值，
   *   但参与 sqrt 分档后有效值会摊薄），所以先建索引再取极值。
   */
  const heat = useMemo(() => {
    const byDay = new Map<string, (typeof daily)[number]>()
    for (const d of daily) byDay.set(d.day, d)

    // 今天的日期串（本地时区）—— 与后端记 `day` 的方式一致。
    const now = new Date()
    const p = (n: number) => String(n).padStart(2, '0')
    const today = `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`

    // 从今天起往前数 365 天（含今天）—— 最后一格 = 今天。
    const days: Array<{ day: string; total: number }> = []
    for (let i = 364; i >= 0; i--) {
      const d = new Date(now)
      d.setDate(d.getDate() - i)
      const key = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
      const rec = byDay.get(key)
      days.push(rec ? { day: key, total: rec.inputTokens + rec.outputTokens } : { day: key, total: 0 })
    }
    const maxTokens = Math.max(1, ...days.map((d) => d.total))
    return { days, maxTokens, today }
  }, [daily])

  return (
    <>
      <PageTitle
        title="用量"
        description="只统计元数据 —— 不记录提示词与回答。"
        actions={
          <div className="flex gap-1">
            {ranges.map((r) => (
              <button
                key={r.days}
                onClick={() => setRange(r.days)}
                className={cn(
                  // ⚠️ 触屏上要够高 —— 28px 的按钮点不准。
                  'fmh-touchable fmh-hoverable rounded-[var(--r-sm)] border px-2 py-1 text-[12px]',
                  'transition-colors duration-150',
                  range === r.days
                    ? 'border-[var(--accent-line)] bg-[var(--accent-soft)] font-medium text-[var(--accent)]'
                    : 'border-[var(--line)] bg-[var(--surface)] text-[var(--ink-2)] hover:bg-[var(--surface-hover)]',
                )}
              >
                {r.label}
              </button>
            ))}
          </div>
        }
      />

      {isLoading ? (
        <Loading label="读取用量" />
      ) : !data ? (
        <Card>
          <Empty
            icon={<Activity size={26} />}
            title="还没有用量记录"
            description="跑一次推理之后，这里会显示按天的曲线与按模型的明细。"
          />
        </Card>
      ) : (
        <div className="space-y-4">
          {/* 总量 */}
          <Card>
            <CardBody className="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-7">
              <Stat label="物理请求" value={data.total.requests} />
              <Stat label="用户回合" value={data.total.turns} />
              <Stat
                label="尝试次数"
                value={data.total.attempts}
                hint="含故障转移"
              />
              <Stat label="输入 token" value={formatTokens(data.total.inputTokens)} />
              <Stat label="输出 token" value={formatTokens(data.total.outputTokens)} />
              <Stat
                label="失败"
                value={data.total.failed}
                tone={data.total.failed > 0 ? 'warn' : undefined}
              />
              {/* ⚠️ 花费只有 OpenRouter 报 ——
                  免密车道永远是 0，所以没花过钱时不显示。 */}
              {(data.total.costUsd ?? 0) > 0 ? (
                <Stat
                  label="花费"
                  value={formatCost(data.total.costUsd ?? 0)}
                  hint="OpenRouter 账户"
                  tone={undefined}
                />
              ) : null}
            </CardBody>
          </Card>

          {/* ⚠️ **热力图** —— 固定 365 格，从去年的今天排到今天，
              最后一格永远是今天。

              ⚠️ **格子连续排** —— 不分月、没有月份标签，
              只是一串 365 个方块；一行放几个由容器宽度自己决定
              （auto-fill），最后一行不满是正常的。

              ⚠️ **永远显示** —— 没有用量记录时也显示
              （全灰的 365 格），那本身是信息："一整年都没用"。 */}
          <Card>
            <CardHeader
              title="Token 用量"
              description="近一年 365 天。输入 + 输出。颜色越深用得越多；灰格 = 那天没有用量。"
            />
            <CardBody>
              <div
                className="grid gap-[3px]"
                style={{
                  gridTemplateColumns:
                    'repeat(auto-fill, minmax(13px, 1fr))',
                  // ⚠️ 格子必须保持正方形 —— 用 aspect-ratio，
                  //   宽度自适应、高度跟着宽度走。
                  //   minmax(13px, 1fr)：最窄 13px（保证手指能点到），
                  //   有富余就摊开，一行塞满为止。
                }}
              >
                {heat.days.map((d) => {
                  const level = heatLevel(d.total, heat.maxTokens)
                  const isToday = d.day === heat.today
                  return (
                    <div
                      key={d.day}
                      title={`${d.day} · ${formatTokens(d.total)} token`}
                      className="aspect-square rounded-[2px] transition-colors duration-150 hover:ring-2 hover:ring-[var(--accent)]"
                      style={{
                        ...heatColor(level),
                        // ⚠️ **今天描边** —— 最后一格永远是今天，
                        //   描个边让人知道"时间轴到这儿为止"。
                        ...(isToday
                          ? { boxShadow: 'inset 0 0 0 1px var(--accent)' }
                          : {}),
                      }}
                    />
                  )
                })}
              </div>
              {/* 图例 —— 没有它，颜色深浅没有意义。 */}
              <div className="mt-3 flex items-center gap-2 text-[11px] text-[var(--ink-3)]">
                <span>少</span>
                {[0, 1, 2, 3, 4].map((lv) => (
                  <div
                    key={lv}
                    className="h-[11px] w-[11px] rounded-[2px]"
                    style={heatColor(lv)}
                  />
                ))}
                <span>多</span>
                <span className="ml-auto">
                  {heat.days[0]?.day ?? ''} → 今天
                </span>
              </div>
            </CardBody>
          </Card>

          {/* 按模型 */}
          <Card>
            <CardHeader
              title="按模型"
              description="“请求”与“回合”分开 —— 回合是用户实际发起的轮次。"
            />
            <div className="overflow-x-auto">
              <table className="w-full text-[12.5px]">
                <thead>
                  <tr className="border-b border-[var(--line)] bg-[var(--surface-2)] text-[11.5px] font-semibold text-[var(--ink-3)]">
                    <th scope="col" className="px-4 py-2 text-left">模型</th>
                    <th scope="col" className="px-3 py-2 text-left">车道</th>
                    <th scope="col" className="px-3 py-2 text-right">请求</th>
                    <th scope="col" className="px-3 py-2 text-right">回合</th>
                    <th scope="col" className="px-3 py-2 text-right">输入</th>
                    <th scope="col" className="px-3 py-2 text-right">输出</th>
                    <th scope="col" className="px-3 py-2 text-right">失败</th>
                    <th scope="col" className="px-3 py-2 text-right">最近调用</th>
                  </tr>
                </thead>
                <tbody>
                  {data.byModel.map((m, i) => {
                    const swapped = m.actualModel && m.actualModel !== m.model
                    return (
                      <tr
                        key={`${m.model}-${m.actualModel}-${i}`}
                        className="border-b border-[var(--line-soft)] last:border-0"
                      >
                        <td className="px-4 py-2">
                          <div className="mono truncate text-[12px]">{m.model}</div>
                          {swapped ? (
                            <div className="text-[11px] text-[var(--warn)]">
                              ⚠ 实际由 {m.actualModel} 应答
                            </div>
                          ) : null}
                        </td>
                        <td className="px-3 py-2">
                          <LaneBadge lane={m.lane as 'zen' | 'openrouter'} />
                        </td>
                        <td className="tnum px-3 py-2 text-right">{m.requests}</td>
                        <td className="tnum px-3 py-2 text-right">{m.turns}</td>
                        <td className="tnum px-3 py-2 text-right">
                          {formatTokens(m.inputTokens)}
                        </td>
                        <td className="tnum px-3 py-2 text-right">
                          {formatTokens(m.outputTokens)}
                        </td>
                        <td className="tnum px-3 py-2 text-right">
                          {m.failed > 0 ? (
                            <span className="text-[var(--bad)]">{m.failed}</span>
                          ) : (
                            <span className="text-[var(--ink-3)]">0</span>
                          )}
                        </td>
                        {/* ⚠️ **最近调用** —— 代替了原来的"花费"列。
                            后端已按 lastUsedAt 降序 —— 最新调用的永远在第一行。 */}
                        <td className="tnum px-3 py-2 text-right whitespace-nowrap text-[var(--ink-3)]">
                          {formatLastUsed(m.lastUsedAt)}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          </Card>
        </div>
      )}
    </>
  )
}

/** 格式化 token 数。 */
function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1000) return `${(n / 1000).toFixed(1)}K`
  return String(n)
}

/**
 * 格式化"最近调用时间"。
 *
 * ⚠️ **24 小时内显示"X 分钟/小时前"** —— 用户要的是"刚用过没有"；
 *   更早的显示日期（MM-DD HH:mm）—— 超过一天精确到分钟就够了，
 *   表格里放"星期二 14:23"那种长格式会把列撑爆。
 */
function formatLastUsed(ms: number): string {
  if (!ms) return '—'
  const age = Date.now() - ms
  if (age < 0) return '刚刚'
  if (age < 60_000) return '刚刚'
  if (age < 3_600_000) return `${Math.floor(age / 60_000)} 分钟前`
  if (age < 86_400_000) return `${Math.floor(age / 3_600_000)} 小时前`
  const d = new Date(ms)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/**
 * 算一个格子是第几档（0 = 最少，4 = 最多）。
 *
 * ⚠️ **按比例分档，不是按绝对值** ——
 *   绝对阈值（比如"1000 token 一档"）在不同规模下没有可比性。
 *
 * ⚠️ **用平方根** ——
//   token 量是长尾分布（一两天特别高，其余接近零）。
//   直接按比例分档的话，几乎所有格子都挤在第 0 档，看不出差别。
 *   开平方后差距才看得出来。
 */
function heatLevel(value: number, max: number): number {
  if (value <= 0 || max <= 0) return 0
  const ratio = Math.sqrt(value / max)
  // 0 → 0 档，1 → 4 档。
  return Math.min(4, Math.max(0, Math.round(ratio * 4)))
}

/**
 * 一档热力格子的颜色。
 *
 * ⚠️ **用内联 style，不用 Tailwind 类** ——
 *
 *	Tailwind 4 的任意值（`bg-[var(--accent)]/8`）**不支持**在
 *	CSS 变量上加透明度修饰符 —— 它编译不出来（class 根本不存在）。
 *
 *	而且颜色**要跟着主题变**（亮色和暗色的 accent 不一样），
 *	所以也不能写死 rgb。
 *
 *	内联 style + `color-mix` 是唯一稳的做法：
 *	`color-mix(in srgb, var(--accent) 20%, transparent)`
 */
function heatColor(level: number): { background: string } {
  const pct = [8, 20, 38, 60, 88][Math.min(4, Math.max(0, level))]
  return {
    background: `color-mix(in srgb, var(--accent) ${pct}%, transparent)`,
  }
}
