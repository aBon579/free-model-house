// 手写的类型 —— 后端 Go 结构体的镜像。
//
// ⚠️ **不用代码生成**：后端结构体很少变，而生成器会引入一个构建步骤
//（然后那个步骤迟早会坏）。手写的代价是"后端改了字段这里要跟着改"，
// 但这个代价是**显式**的，TypeScript 会在编译时抓到不匹配。

/** 车道 id。 */
export type LaneID = 'zen' | 'openrouter' | 'kilo'

/**
 * 模型状态。
 *
 * ⚠️ **取值必须与后端的 `lane.ProbeState` 完全一致** —— 后端用连字符
 *（`region-blocked`）不是下划线。写错的话状态判断会静默失配
 *（`region-blocked` 落到 `unknown` 分支，用户看到"未知"而不是"区域限制"）。
 */
export type ModelState =
  | 'available'
  | 'unknown'
  | 'throttled'
  | 'region-blocked'
  | 'abuse-blocked'
  | 'unavailable'

/** 能力结论的来源 —— 决定这个结论有多可靠。 */
export type CapabilitySource = 'probe' | 'listing' | 'heuristic' | 'unknown'

/** 一个模型。 */
export interface ModelInfo {
  /** 完整 id（`车道/模型`）。 */
  id: string
  lane: LaneID
  name: string
  free: boolean
  enabled: boolean
  /** 上游协议线。 */
  wire: 'chat' | 'responses' | 'messages'
  reasoning: boolean
  /** 是否能关掉思考。 */
  canDisableThinking?: boolean
  vision: boolean
  audio: boolean
  file: boolean
  context: number
  maxOutput: number
  capability: CapabilitySource
  /** 支持的思考档位。 */
  efforts: string[]
  state: ModelState
  /** 首字延迟（毫秒）。 */
  ttftMs: number
  /** 限流冷却剩余秒数（0 = 不在冷却）。 */
  cooldownSec?: number
  /** 预计恢复时间（epoch ms）。 */
  recoveryAt?: number
  /** 探测结论的说明（"为什么不可用"）。 */
  probeDetail?: string
  /**
   * 上次探测的时间（epoch 毫秒）。
   *
   * ⚠️ **手动和自动探测都记** —— 用户靠它判断结论新不新。
   */
  probedAt?: number
}

/** 车道状态。 */
export interface LaneStatus {
  id: LaneID
  name?: string
  ready: boolean
  /** 这条车道开不开（关掉后它的模型不进候选链）。 */
  enabled: boolean
  modelCount: number
  lastRefresh: number
  lastError: string
  reason: string
  needsCredential: boolean
  credentialHint: string
  /**
   * 这条车道**是否参与自动探测**。
   *
   * ⚠️ 必须是 false 时，界面上要说明为什么 ——
   *   否则用户看到满屏"未探测"会以为是 bug。
   */
  autoProbe: boolean
  /** 不参与自动探测的原因（给用户看的话）。 */
  autoProbeReason?: string
}

/** 一把 Key 的展示形态。 */
export interface KeyInfo {
  name: string
  /** 明文（默认空，只有 reveal=1 时才有）。 */
  value: string
  hint: string
}

/** 一把调用 Key（多 Key 模式）。 */
export interface ApiKey {
  id: string
  /** 命名标签（≤6 字符）。 */
  label: string
  /** 限流档位：light / standard / deep（网关级为 off 时生效）。 */
  tier: 'light' | 'standard' | 'deep'
  enabled: boolean
  createdAt: number
  /** 指纹（永远有）。 */
  hint: string
  /** 明文（只在 reveal=1 / 刚创建时有）。 */
  value?: string
}

/** 网关级档位 —— 比单 Key 多一个 off。 */
export type GatewayTier = 'off' | 'light' | 'standard' | 'deep'

/** Key 集合。 */
export interface KeysResponse {
  prefix: string
  /** 多 Key 列表。 */
  keys: ApiKey[]
  /** 标签最大长度（字符数）。 */
  maxLabel: number
  /** 网关级档位 —— 非 off 时单 Key 的档位被锁定。 */
  gatewayTier: GatewayTier
  local: KeyInfo
  lan: KeyInfo
  revealed: boolean
}

/** 上游凭据。 */
export interface SecretInfo {
  name: string
  hint: string
  set: boolean
}

/** 总体状态。 */
export interface StatusResponse {
  version: string
  lanes: LaneStatus[]
  /** 模型池 = 清单里的模型总数。 */
  modelCount: number
  /** 清单里标记为免费的模型数。 */
  freeModels: number
  /** 可用模型 = **探测验证过可用**的模型数。 */
  availableModels: number
  lanKeyHint: string
  /**
   * 是否**必须改密码**。
   *
   * ⚠️ 用初始密码 `admin` 登录时为 true —— 前端要弹「设置新密码」。
   */
  mustChangePassword: boolean
  usage: UsageTotal
  /** 当前出口（免密车道的可用性是**网络路径**的属性）。 */
  egress?: EgressInfo
  /** 车道级封锁（全被拒 = 出口问题，不是模型问题）。 */
  laneLocks?: Record<string, LaneLockInfo>
  /** 车道开关（哪条车道开不开）。 */
  laneSwitches?: Record<string, boolean>
}

/**
 * 当前出口。
 *
 * ⚠️ 上游的原话是 "detected abusive traffic from **this IP address**" ——
 *   所以"被风控"是**出口**的属性，不是模型的属性。
 */
export interface EgressInfo {
  ip: string
  country: string
  at: number
  err: string
}

/** 车道级封锁状态。 */
export interface LaneLockInfo {
  locked: boolean
  reason: string
  state: string
  /** 被同一理由拒绝的模型数。 */
  count: number
  /** 这一轮探测的模型数。 */
  total: number
}

/** 总量。 */
export interface UsageTotal {
  requests: number
  turns: number
  attempts: number
  inputTokens: number
  outputTokens: number
  failed: number
  recovered: number
  /** 总花费（美元）—— **只有 OpenRouter 报**。 */
  costUsd?: number
}

/** 按天。 */
export interface DailyUsage {
  day: string
  requests: number
  turns: number
  attempts: number
  inputTokens: number
  outputTokens: number
  failed: number
  /** 这天的花费（美元）—— **只有 OpenRouter 报**。 */
  costUsd?: number
}

/** 按模型。 */
export interface ModelUsage {
  model: string
  lane: LaneID
  /** 实际服务的模型 —— 可能与 model 不同（故障转移）。 */
  actualModel: string
  requests: number
  turns: number
  inputTokens: number
  outputTokens: number
  failed: number
  /** 最近一次被调用的时间（epoch ms）—— 后端按它降序排。 */
  lastUsedAt: number
  /** 这个模型累计花的美元 —— **只有 OpenRouter 报**。 */
  costUsd?: number
}

/** 用量响应。 */
export interface UsageResponse {
  daily: DailyUsage[]
  byModel: ModelUsage[]
  total: UsageTotal
}

/** 探测结论。 */
export interface ProbeResult {
  model: string
  state: ModelState
  detail: string
  latency: number
  ttft: number
  probedAt: number
}

/**
 * **上一轮批量探测**的统计。
 *
 * ⚠️ 这是"探测结论"卡片该显示的数字 ——
 *   只含批量轮次，手动单探不进来（否则数字跨轮累加）。
 *
 * ⚠️ `stale` = 已经过了探测间隔 —— 那时数字已归零，等下一轮。
 */
export interface RoundSummary {
  probed: number
  available: number
  failed: number
  /** 上一轮结束时间（epoch ms）。 */
  at: number
  /** 是否已过探测间隔（过了 = 数字已归零）。 */
  stale: boolean
}

/**
 * 局域网访问档位。
 *
 * ⚠️ 取值必须与后端的 `server.LanTier` 完全一致。
 *
 * ⚠️ 单 Key 只有 light/standard/deep 三档；
 *   off 只在**网关级**存在（网关不限制，各 Key 按自己的档位）。
 */
export type LanTier = 'light' | 'standard' | 'deep'

/** 网关档位的说明（后端发的兜底）。 */
export const GATEWAY_TIERS: Array<{
  value: GatewayTier
  label: string
  hint: string
}> = [
  { value: 'off', label: 'off · 单 Key 分治', hint: '网关不限制 —— 每把 Key 按自己的档位限流' },
  { value: 'light', label: 'light · 轻量', hint: '全部 Key 强制 10 次/分钟、2 并发' },
  { value: 'standard', label: 'standard · 标准', hint: '全部 Key 强制 60 次/分钟、6 并发' },
  { value: 'deep', label: 'deep · 不限', hint: '全部 Key 不限量' },
]

/** 档位的中文说明（后端也会发一份，这里是兜底）。 */
export const LAN_TIERS: Array<{
  value: LanTier
  label: string
  hint: string
}> = [
  {
    value: 'light',
    label: '轻量（Light）',
    hint: '少量调用 —— 10 次/分钟、2 并发。给偶尔查一下的 agent',
  },
  {
    value: 'standard',
    label: '标准（Standard）',
    hint: '正常使用 —— 60 次/分钟、6 并发。家里的主力档',
  },
  {
    value: 'deep',
    label: '深度（Deep）',
    hint: '不限量 —— 自己独占，或出口很干净时用',
  },
]

/** 设置。 */
export interface Settings {
  lanEnabled: boolean
  /** 局域网访问档位。 */
  lanTier: LanTier
  /** 档位说明（后端发的）。 */
  lanTiers?: Array<{ value: string; label: string; hint: string }>
  /** 每分钟请求上限（<= 0 = 不限）。 */
  lanPerMinute: number
  /** 并发上限（<= 0 = 不限）。 */
  lanMaxConcurrent: number
  autoModelSwitch: boolean
  probeEnabled: boolean
  probeIntervalMin: number
  // ⚠️ **没有 probeBatchSize** —— 探测是**全量**的，
  //   每 15 分钟把每个免费模型都探一遍。
  /** 探测批量自适应 —— 上游越拥挤，探得越少。 */
  adaptiveProbe: boolean
}

/** 额度。 */
export interface QuotaInfo {
  limit: number
  usage: number
  remaining: number
  freeTierUsageToday: number
  label?: string
}

/** 出口代理配置。 */
export interface ProxyConfig {
  enabled: boolean
  /** 协议：http / https / socks5。 */
  scheme: 'http' | 'https' | 'socks5'
  host: string
  port: number
}

/** 出口代理的运行状态。 */
export interface ProxyStatus {
  enabled: boolean
  scheme: string
  host: string
  port: number
  /** `scheme://host:port`（空 = 未配置）。 */
  address: string
  /** 最近一次连通检测：代理活着吗。 */
  up: boolean
  lastCheckAt?: number
  lastError?: string
}

/** 代理检测结果 —— 代理连通 + 经代理的实际出口（异步轮询的形状）。 */
export interface ProxyTestResult {
  running: boolean
  ok: boolean
  address?: string
  error?: string
  /** 经代理出去的公网出口（代理通了才有）。 */
  egressIp?: string
  egressCountry?: string
  egressError?: string
  startedAt?: number
  doneAt?: number
}
