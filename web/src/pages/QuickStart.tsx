import { useMemo, useState } from 'react'
import { Check, Copy, X } from 'lucide-react'
import { PageTitle } from '../components/Shell'
import { Card, CardBody, CardHeader, Notice, Badge, Button, cn } from '../components/ui'
import { CopyField } from '../components/common'
import { copyToClipboard } from '../lib/clipboard'
import { useKeys, useSettings, useStatus } from '../api/client'

/** 各个 Agent 工具的接入片段。 */
interface Target {
  key: string
  label: string
  /** 配置片段的生成器。 */
  snippet: (baseURL: string, key: string) => string
  /** 额外说明。 */
  note?: string
}

/**
 * 目标工具列表。
 *
 * ⚠️ 顺序按"在 NAS 场景下最常用来着"排。
 */
const targets: Target[] = [
  {
    key: 'claude-code',
    label: 'Claude Code',
    snippet: (base, key) =>
      `export ANTHROPIC_BASE_URL="${base}"\n` +
      `export ANTHROPIC_AUTH_TOKEN="${key}"`,
    note: 'Claude Code 读 ANTHROPIC_BASE_URL 与 ANTHROPIC_AUTH_TOKEN。',
  },
  {
    key: 'opencode',
    label: 'OpenCode',
    snippet: (base, key) =>
      `export OPENAI_BASE_URL="${base}"\n` +
      `export OPENAI_API_KEY="${key}"`,
    note: '在 opencode.json 里把 provider 指向这个地址也行。',
  },
  {
    key: 'cline',
    label: 'Cline',
    snippet: (base, key) =>
      `{\n` +
      `  "provider": "openai-compatible",\n` +
      `  "openAiBaseUrl": "${base}",\n` +
      `  "openAiApiKey": "${key}"\n` +
      `}`,
  },
  {
    key: 'aider',
    label: 'Aider',
    snippet: (base, key) =>
      `export OPENAI_API_BASE="${base}"\n` +
      `export OPENAI_API_KEY="${key}"\n` +
      `export OPENAI_MODEL="zen/minimax-m3.1-flash-free"`,
  },
  {
    key: 'continue',
    label: 'Continue',
    snippet: (base, key) =>
      `{\n` +
      `  "models": [{\n` +
      `    "title": "Free Model House",\n` +
      `    "provider": "openai",\n` +
      `    "model": "zen/minimax-m3.1-flash-free",\n` +
      `    "apiBase": "${base}",\n` +
      `    "apiKey": "${key}"\n` +
      `  }]\n` +
      `}`,
  },
  {
    key: 'curl',
    label: 'curl',
    snippet: (base, key) =>
      `curl ${base}/chat/completions \\\n` +
      `  -H "Authorization: Bearer ${key}" \\\n` +
      `  -H "Content-Type: application/json" \\\n` +
      `  -d '{\n` +
      `    "model": "zen/minimax-m3.1-flash-free",\n` +
      `    "messages": [{"role": "user", "content": "你好"}]\n` +
      `  }'`,
    note: '直接看网关怎么被调用。',
  },
]

/** 快速接入页 —— 首屏。 */
export function QuickStart() {
  const { data: keys, isLoading: keysLoading } = useKeys(false)
  const { data: status } = useStatus()
  const { data: settings } = useSettings()
  const [active, setActive] = useState(targets[0].key)
  const [revealed, setRevealed] = useState(false)
  const { data: fullKeys } = useKeys(revealed)

  // ⚠️ **用浏览器的实际地址**算 base URL ——
  // 用户是通过 NAS-IP 访问的，写死 localhost 是错的。
  const origin = useMemo(() => window.location.origin, [])
  const baseURL = `${origin}/v1`

  const lanKey = revealed ? (fullKeys?.lan.value ?? '') : (keys?.lan.hint ?? '')

  const activeTarget = targets.find((t) => t.key === active) ?? targets[0]
  const snippet = activeTarget.snippet(baseURL, lanKey)

  const lanDisabled = settings ? !settings.lanEnabled : false

  return (
    <>
      <PageTitle
        title="快速接入"
        description="把免费模型接进你的 Agent，只需要两步。"
      />

      {lanDisabled ? (
        <div className="mb-4">
          <Notice tone="warn">
            ⚠ 局域网推理当前已关闭 —— 去「设置」开启后，LAN 里的 Agent 才能用这把 Key。
          </Notice>
        </div>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_320px]">
        <div className="min-w-0 space-y-4">
          {/* ① 局域网地址 */}
          <Card>
            <CardHeader
              title="① 局域网地址与密钥"
              description="你的 Agent 需要这两样。"
            />
            <CardBody className="space-y-3">
              <div>
                <div className="mb-1.5 text-[12.5px] font-medium text-[var(--ink-2)]">
                  Base URL
                </div>
                <CopyField value={baseURL} masked={false} />
              </div>
              <div>
                <div className="mb-1.5 flex items-center gap-2 text-[12.5px] font-medium text-[var(--ink-2)]">
                  局域网 Key
                  <Badge tone="zen">免额度</Badge>
                </div>
                {keysLoading ? (
                  <div className="text-[12.5px] text-[var(--ink-3)]">读取中…</div>
                ) : (
                  <CopyField
                    value={lanKey}
                    masked
                    allowReveal={false}
                    // ⚠️ **受控** —— 显示状态由页面持有，
                    //   因为要显示完整 Key 就得**重新请求**后端（?reveal=1）。
                    //   不受控的话，组件会把拿到的明文又打回指纹。
                    revealed={revealed}
                  />
                )}
                <div className="mt-1.5 flex items-center gap-2">
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setRevealed((v) => !v)}
                  >
                    {revealed ? '隐藏完整 Key' : '显示完整 Key'}
                  </Button>
                  <span className="text-[11.5px] text-[var(--ink-3)]">
                    {revealed
                      ? '⚠ 完整 Key 已显示 —— 别截图'
                      : '默认打码，避免投屏泄露'}
                  </span>
                </div>
              </div>
            </CardBody>
          </Card>

          {/* ② 选工具 */}
          <Card>
            <CardHeader
              title="② 选一个你的工具"
              description="复制整段配置，粘进对应工具的设置里。"
            />
            <CardBody>
              <div className="mb-3 flex flex-wrap gap-1.5">
                {targets.map((t) => (
                  <button
                    key={t.key}
                    onClick={() => setActive(t.key)}
                    className={cn(
                      // ⚠️ 触屏上要够高 —— 29px 的按钮点不准。
                      'fmh-touchable rounded-[var(--r-sm)] border px-2.5 py-1 text-[12.5px]',
                      'transition-colors duration-150',
                      active === t.key
                        ? 'border-[var(--accent-line)] bg-[var(--accent-soft)] font-medium text-[var(--accent)]'
                        : 'border-[var(--line)] bg-[var(--surface)] text-[var(--ink-2)] hover:bg-[var(--surface-hover)]',
                    )}
                  >
                    {t.label}
                  </button>
                ))}
              </div>

              <SnippetBlock text={snippet} />

              {activeTarget.note ? (
                <div className="mt-2 text-[12px] text-[var(--ink-3)]">{activeTarget.note}</div>
              ) : null}
            </CardBody>
          </Card>
        </div>

        {/* 侧栏：状态速览 */}
        <div className="space-y-4">
          <Card>
            <CardHeader title="现在有什么" />
            <CardBody className="space-y-3">
              <div className="flex items-baseline justify-between">
                <span className="text-[12.5px] text-[var(--ink-3)]">模型池</span>
                <span className="tnum text-[15px] font-semibold text-[var(--ink)]">
                  {status?.modelCount ?? '…'}
                </span>
              </div>
              <div className="flex items-baseline justify-between">
                <span className="text-[12.5px] text-[var(--ink-3)]">可用模型</span>
                <span className="tnum text-[15px] font-semibold text-[var(--ok)]">
                  {status?.availableModels ?? '…'}
                </span>
              </div>

              <div className="border-t border-[var(--line-soft)] pt-3">
                <div className="mb-1.5 text-[12.5px] font-medium text-[var(--ink-2)]">
                  上游源
                </div>
                {(status?.lanes ?? []).map((lane) => (
                  <div key={lane.id} className="mb-1.5 flex items-center gap-2 text-[12.5px]">
                    {/* ⚠️ **指示灯** ——
                        开着且就绪 = 亮色绿 + 光晕 + 闪烁；
                        关着或没就绪 = 浅灰，不闪。

                        之前用 `--ok`（#047857 墨绿）——
                        6px 的墨绿点落在奶油底上看着就是灰点，
                        分不出"开着"还是"关着"。 */}
                    <span
                      className={cn(
                        'h-1.5 w-1.5 shrink-0 rounded-full',
                        !lane.enabled
                          ? 'fmh-lamp-off'
                          : lane.ready
                            ? 'fmh-lamp-on'
                            : 'fmh-lamp-off',
                      )}
                      title={
                        !lane.enabled
                          ? `${lane.name}：已关闭`
                          : lane.ready
                            ? `${lane.name}：运行中`
                            : `${lane.name}：已启用但未就绪`
                      }
                    />
                    <span
                      className={cn(
                        lane.enabled ? 'text-[var(--ink-2)]' : 'text-[var(--ink-3)] line-through',
                      )}
                    >
                      {lane.name}
                    </span>
                    <span className="tnum ml-auto text-[var(--ink-3)]">
                      {lane.enabled ? `${lane.modelCount} 模型` : '已关闭'}
                    </span>
                  </div>
                ))}
              </div>
            </CardBody>
          </Card>

          <Card>
            <CardHeader title="模型怎么写" />
            <CardBody>
              <div className="space-y-2 text-[12.5px] leading-[19px] text-[var(--ink-2)]">
                <div>
                  模型名用 <code className="mono">车道/模型</code> 的形式：
                </div>
                <div className="mono rounded-[var(--r-sm)] bg-[var(--surface-2)] px-2 py-1.5 text-[12px]">
                  zen/minimax-m3.1-flash-free
                  <br />
                  openrouter/deepseek/deepseek-r1:free
                </div>
                <div className="text-[var(--ink-3)]">
                  也可以只写模型名，网关会自动找到它所在的车道；
                  但两个车道有同名模型时必须写全名。
                </div>
              </div>
            </CardBody>
          </Card>
        </div>
      </div>
    </>
  )
}

/** 带复制按钮的代码块。 */
function SnippetBlock({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)

  async function copy() {
    // ⚠️ 用 copyToClipboard —— navigator.clipboard 在 HTTP 下不可用。
    const ok = await copyToClipboard(text)
    if (!ok) {
      setFailed(true)
      setTimeout(() => setFailed(false), 2400)
      return
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 1400)
  }

  return (
    <div className="relative">
      <pre
        className="overflow-x-auto rounded-[var(--r-sm)] border border-[var(--line)] bg-[var(--surface-2)] px-3 py-2.5 font-[family-name:var(--mono)] text-[12px] leading-[19px] text-[var(--ink)]"
        style={{ scrollbarWidth: 'thin' }}
      >
        {text}
      </pre>
      <Button
        size="sm"
        variant="secondary"
        onClick={copy}
        className="absolute right-2 top-2"
        aria-label="复制配置"
        title={copied ? '已复制' : failed ? '复制失败 —— 请手动选中复制' : '复制'}
      >
        {copied ? (
          <Check size={13} className="text-[var(--ok)]" />
        ) : failed ? (
          <>
            <X size={13} className="text-[var(--bad)]" />
            复制失败
          </>
        ) : (
          <>
            <Copy size={13} />
            复制
          </>
        )}
      </Button>
    </div>
  )
}
