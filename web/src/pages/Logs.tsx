import { useState } from 'react'
import { ScrollText, Trash2 } from 'lucide-react'
import {
  Badge,
  Button,
  Card,
  CardBody,
  CardHeader,
  Empty,
  ErrorNote,
  Loading,
  Select,
  Switch,
  cn,
} from '../components/ui'
import { useToast } from '../components/toast'
import { PageTitle } from '../components/Shell'
import { useClearLogs, useLogAll, useLogConfig, useLogs } from '../api/client'

/** 把毫秒时间戳格式化成 HH:mm:ss。 */
function fmtTime(at: number): string {
  const d = new Date(at)
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, '0'))
    .join(':')
}

/** 类目 → 颜色提示。 */
function categoryTone(cat: string): 'ok' | 'warn' | 'bad' | 'idle' {
  switch (cat) {
    case 'request':
      return 'ok'
    case 'probe':
      return 'idle'
    case 'key':
      return 'warn'
    case 'lane':
      return 'idle'
    case 'system':
      return 'idle'
    default:
      return 'idle'
  }
}

/**
 * 日志页。
 *
 * ⚠️ **关闭的类目** ——
 *   后端既不再写入，也不在查询结果里返回；
 *   历史行仍然留在数据库里（重新开启就能看到）。
 */
export function Logs() {
  const [category, setCategory] = useState('')
  const { data, isLoading, error } = useLogs(category || undefined)
  const setConfig = useLogConfig()
  const setAll = useLogAll()
  const clear = useClearLogs()
  const toast = useToast()

  const categories = data?.categories ?? []
  const logs = data?.logs ?? []
  const counts = data?.counts ?? {}

  return (
    <>
      <PageTitle
        title="日志"
        description="按类目记录运行事件。关掉某个类目后不再记录，也不再显示；历史数据仍保留。"
        actions={
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              clear.mutate(undefined, {
                onSuccess: () => toast.ok('已清空全部日志'),
              })
            }}
            disabled={clear.isPending}
          >
            <Trash2 size={13} />
            清空全部
          </Button>
        }
      />

      <Card className="mb-4">
        <CardHeader
          title="类目开关"
          description="关闭 = 不再记录这一类的新日志，日志页也不再显示（历史数据不删除）。"
          actions={
            <div className="flex flex-wrap items-center gap-1.5">
              <Button
                size="sm"
                variant="secondary"
                onClick={() => setAll.mutate(true)}
                disabled={setAll.isPending}
              >
                全开
              </Button>
              <Button
                size="sm"
                variant="secondary"
                onClick={() => setAll.mutate(false)}
                disabled={setAll.isPending}
              >
                全关
              </Button>
            </div>
          }
        />
        <CardBody>
          <div className="flex flex-wrap gap-x-6 gap-y-3">
            {categories.map((c) => (
              <div key={c.id} className="flex items-center gap-2.5">
                <Switch
                  checked={c.enabled}
                  label={`${c.label}日志开关`}
                  onChange={(enabled) =>
                    setConfig.mutate(
                      { category: c.id, enabled },
                      {
                        onSuccess: () =>
                          toast.info(
                            enabled ? `已开启「${c.label}」日志` : `已关闭「${c.label}」日志`,
                          ),
                      },
                    )
                  }
                />
                <span className="text-[13px] text-[var(--ink-2)]">{c.label}</span>
                <Badge tone="idle">{counts[c.id] ?? 0}</Badge>
                {!c.enabled ? <Badge tone="warn">已关闭</Badge> : null}
              </div>
            ))}
          </div>
        </CardBody>
      </Card>

      <Card>
        <CardHeader
          title="最近日志"
          description="最多显示最近 300 条；10 秒自动刷新。"
          actions={
            <div className="flex items-center gap-2">
              <Select
                value={category}
                onChange={(e) => setCategory(e.target.value)}
                aria-label="按类目筛选"
              >
                <option value="">全部类目</option>
                {categories
                  .filter((c) => c.enabled)
                  .map((c) => (
                    <option key={c.id} value={c.id}>
                      {c.label}
                    </option>
                  ))}
              </Select>
              {category ? (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    clear.mutate(category, {
                      onSuccess: () => toast.ok('已清空该类目日志'),
                    })
                  }}
                >
                  清空此类
                </Button>
              ) : null}
            </div>
          }
        />
        <CardBody className="p-0">
          {isLoading ? (
            <div className="p-5">
              <Loading label="读取日志" />
            </div>
          ) : error ? (
            <div className="p-5">
              <ErrorNote>{(error as Error).message}</ErrorNote>
            </div>
          ) : logs.length === 0 ? (
            <div className="p-2">
              <Empty icon={<ScrollText size={28} />} title="还没有日志" description="发生调用、探测或设置变更后会出现在这里。" />
            </div>
          ) : (
            <div className="max-h-[62vh] overflow-y-auto">
              <table className="w-full border-collapse text-[12.5px]">
                <thead className="sticky top-0 bg-[var(--surface)]">
                  <tr className="border-b border-[var(--line)] text-left text-[var(--ink-3)]">
                    <th className="w-[92px] px-4 py-2 font-medium">时间</th>
                    <th className="w-[92px] px-2 py-2 font-medium">类目</th>
                    <th className="px-2 py-2 font-medium">内容</th>
                  </tr>
                </thead>
                <tbody>
                  {logs.map((l) => (
                    <tr
                      key={l.id}
                      className={cn(
                        'border-b border-[var(--line-soft)] align-top',
                        'hover:bg-[var(--surface-hover)]',
                      )}
                    >
                      <td className="mono whitespace-nowrap px-4 py-1.5 text-[var(--ink-3)]">
                        {fmtTime(l.at)}
                      </td>
                      <td className="px-2 py-1.5">
                        <Badge tone={categoryTone(l.category)}>
                          {categories.find((c) => c.id === l.category)?.label ?? l.category}
                        </Badge>
                      </td>
                      <td className="mono break-all px-2 py-1.5 text-[var(--ink-2)]">
                        {l.message}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardBody>
      </Card>
    </>
  )
}
