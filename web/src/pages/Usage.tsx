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
  const maxTokens = useMemo(
    () => Math.max(1, ...daily.map((d) => d.inputTokens + d.outputTokens)),
    [daily],
  )

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
      ) : !data || data.total.requests === 0 ? (
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
              {/* ⚠️ 费用只有 OpenRouter 报 ——
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

          {/* ⚠️ **热力图** —— 不用柱状图。

              为什么：柱状图在 30 天 × 稀疏数据时**大部分格子是空的**，
              而且"高矮"要靠眼睛比对坐标轴；
              热力图直接用**颜色深浅**表达量级，一眼看出
              "哪天忙、哪天闲"。

              ⚠️ **格子是方块**（不是竖条）——
                 竖条只是换个画法的柱状图，方块才像热力图。 */}
          <Card>
            <CardHeader
              title="Token 用量"
              description="按天。输入 + 输出。颜色越深用得越多。"
            />
            <CardBody>
              {daily.length === 0 ? (
                <div className="py-8 text-center text-[12.5px] text-[var(--ink-3)]">
                  这个区间没有数据
                </div>
              ) : (
                <>
                  <div className="flex flex-wrap gap-[3px]">
                    {daily.map((d) => {
                      const total = d.inputTokens + d.outputTokens
                      const level = heatLevel(total, maxTokens)
                      return (
                        <div
                          key={d.day}
                          className="group relative"
                          title={`${d.day} · ${formatTokens(total)} token`}
                        >
                          <div
                            className="h-[15px] w-[15px] rounded-[3px] transition-colors duration-150 group-hover:ring-2 group-hover:ring-[var(--accent)]"
                            style={heatColor(level)}
                          />
                          <div className="pointer-events-none absolute bottom-full left-1/2 z-10 mb-1 hidden -translate-x-1/2 whitespace-nowrap rounded-[var(--r-sm)] bg-[var(--ink)] px-2 py-1 text-[11px] text-[var(--surface)] group-hover:block">
                            {d.day} · {formatTokens(total)} token
                          </div>
                        </div>
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
                      {daily[0]?.day ?? ''} → {daily[daily.length - 1]?.day ?? ''}
                    </span>
                  </div>
                </>
              )}
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
                    <th scope="col" className="px-3 py-2 text-right">花费</th>
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
                        {/* ⚠️ 花费 —— **只有 OpenRouter 报**。
                            免密车道永远是 0，所以显示"—"。 */}
                        <td className="tnum px-3 py-2 text-right">
                          {(m.costUsd ?? 0) > 0 ? (
                            <span title="OpenRouter 账户扣费">{formatCost(m.costUsd ?? 0)}</span>
                          ) : (
                            <span className="text-[var(--ink-3)]">—</span>
                          )}
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
