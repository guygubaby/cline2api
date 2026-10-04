export interface Model {
  id: string
  provider?: string
  source?: string
  cost?: string
  custom?: boolean
  delisted?: boolean
  context?: number
  output?: number
  channelCount?: number
}

export interface ModelStat {
  modelId: string
  usageCount: number
  promptTokens: number
  completionTokens: number
  totalTokens: number
  cachedTokens: number
}

export interface Account {
  accountId: string
  email: string
  status: string
  cooldownUntil?: string
  usageCount: number
  promptTokens: number
  completionTokens: number
  totalTokens: number
  cachedTokens: number
  lastUsed?: string
  createdAt?: string
  modelStats?: Record<string, ModelStat>
  modelCooldowns?: Record<string, string>
  modelLatencies?: Record<string, { ewmaMs: number }>
}

export interface RequestLog {
  id: string
  startedAt: string
  accountEmail?: string
  upstream?: string
  apiKeyPreview?: string
  apiKeyId?: string
  apiKeyName?: string
  protocol: string
  model: string
  completed: boolean
  usageAvailable: boolean
  inputTokens: number
  outputTokens: number
  cachedTokens: number
  totalTokens: number
  durationMs: number
  ttftMs: number
  upstreamTtftMs?: number
  thinkingTtftMs?: number
  visibleTtftMs?: number
  outputTokensPerSecond: number
  error?: string
  errorCode?: string
  finishReason?: string
  retryCount?: number
  upstreamAttempts?: number
  estimatedInputTokens?: number
  reasoningChars?: number
  thinkingTokens?: number
  sawDone?: boolean
  retrySuppressed?: boolean
}

export interface ProviderModel { id: string; publicId: string; enabled: boolean }
export interface Provider {
  id: string
  name: string
  protocol: string
  baseURL: string
  enabled: boolean
  forceStream: boolean
  allowPrivateNetwork: boolean
  keyPreview?: string
  models: ProviderModel[]
  runtime?: { lastSuccess?: string; lastError?: string; cooldowns?: Record<string, string>; latencies?: Record<string, { ewmaMs: number }> }
}

export interface AdminConfig {
  address: string
  host: string
  strategy: string
  version: string
  poolPath: string
  defaultModel: string
  anthropicEffort: string
  headers: Record<string, string>
  localIPs: string[]
  hasPassword: boolean
}
