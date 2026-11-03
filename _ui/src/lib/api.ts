export class HTTPError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.name = 'HTTPError'
  }
}

export function errorMessage(e: unknown): string {
  if (e instanceof Error) return e.message
  return String(e)
}

export function isNotFound(e: unknown): boolean {
  return e instanceof HTTPError && e.status === 404
}

// Relative to the page, so the UI works under any base path.
const base = 'api/v1'

async function request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  let res: Response
  try {
    res = await fetch(`${base}${path}`, {
      method,
      // Tells the audit log a change came from the UI.
      headers: { 'X-Kronik-Client': 'ui', ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    })
  } catch (e) {
    if (signal?.aborted) throw e
    throw new HTTPError(0, 'Cannot reach the kronik server.')
  }

  if (!res.ok) throw await responseError(res)

  if (res.status === 204) return undefined as T
  return res.json()
}

// responseError keeps the server's {message}; a proxy's HTML error page is
// replaced by the status, it is not meant to be read in an alert.
async function responseError(res: Response): Promise<HTTPError> {
  const fallback = `${res.status} ${res.statusText || 'error'}`.trim()
  const type = res.headers.get('Content-Type') ?? ''
  const text = await res.text().catch(() => '')

  if (type.includes('json')) {
    try {
      const data = JSON.parse(text)
      if (typeof data?.message === 'string' && data.message) return new HTTPError(res.status, data.message)
    } catch {}
  }

  if (type.startsWith('text/plain') && text && text.length < 300) return new HTTPError(res.status, text.trim())

  const hint = res.status === 502 || res.status === 503 || res.status === 504 ? ': the kronik server is not reachable' : ''
  return new HTTPError(res.status, fallback + hint)
}

export type Concurrency = 'allow' | 'forbid' | 'replace'

export type RunStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'timed_out' | 'canceled' | 'skipped' | 'lost'

export type HTTPConfig = {
  method: string
  url: string
  headers: Record<string, string>
  body: string
  expected_status: number[]
  insecure_skip_verify: boolean
  no_redirect: boolean
}

export type JSConfig = {
  script: string
  env: Record<string, string>
}

export type Mount = { type: 'bind' | 'volume'; source: string; target: string; read_only: boolean }

export type ContainerConfig = {
  image: string
  command: string[]
  args: string[]
  env: Record<string, string>
  workdir: string
  user: string
  cpu: string
  memory: string
  pull: '' | 'missing' | 'always' | 'never'
  labels: Record<string, string>
  network: string
  networks: string[]
  mounts: Mount[]
  privileged: boolean
  constraints: string[]
  namespace: string
  service_account: string
  node_selector: Record<string, string>
  secret_env: Record<string, string>
}

export type CommandConfig = {
  command: string
  env: Record<string, string>
  dir: string
}

export type Job = {
  id: string
  name: string
  description: string
  tags: string[]
  schedule: string
  timezone: string
  enabled: boolean
  executor: string
  config: any
  concurrency: Concurrency
  catch_up: boolean
  timeout_seconds: number
  retries: number
  retry_delay_seconds: number
  next_run_at: string | null
  created_at: string
  updated_at: string
  created_by: string
  updated_by: string
  jitter_seconds: number
  alert_after_seconds: number
  alerted_at: string | null
  health: '' | 'ok' | 'failing'
  notify: Notify
  has_hook: boolean
  after_jobs: string[]
  after_on: AfterOn
  last_run?: { id: string; status: RunStatus; started_at: string | null; finished_at: string | null; result: string }
}

export type NotifyEvent = 'failure' | 'recovery' | 'no_run' | 'success'
export type Notify = { channels: string[]; on: NotifyEvent[] }
export type AfterOn = 'succeeded' | 'failed' | 'finished'

export type JobInput = Omit<
  Job,
  'id' | 'next_run_at' | 'created_at' | 'updated_at' | 'created_by' | 'updated_by' | 'last_run' | 'alerted_at' | 'health' | 'has_hook'
> & {
  updated_at?: string
}

export type Run = {
  id: string
  job_id: string
  job_name: string
  trigger: 'schedule' | 'manual' | 'retry' | 'webhook' | 'after' | 'ping'
  status: RunStatus
  attempt: number
  scheduled_at: string
  not_before: string
  node: string
  created_at: string
  started_at: string | null
  finished_at: string | null
  result_code: number | null
  output?: string
  output_truncated: boolean
  error: string
  cancel_requested: boolean
  result: string
  triggered_by: string
  canceled_by: string
  params: Record<string, string>
}

export type Channel = {
  id: string
  name: string
  type: 'slack' | 'teams' | 'webhook' | 'email'
  config: { url?: string; headers?: Record<string, string>; to?: string[] }
  created_at: string
  created_by: string
  updated_at: string
  updated_by: string
}

export type ChannelInput = Pick<Channel, 'name' | 'type' | 'config'>

export type Notification = {
  id: string
  channel_id: string
  channel_name: string
  job_id: string
  run_id: string
  event: string
  payload: { job_name?: string; status?: string; error?: string }
  status: 'pending' | 'sent' | 'failed'
  attempts: number
  next_at: string
  created_at: string
  sent_at: string | null
  error: string
}

export type Secret = {
  name: string
  description: string
  source: 'ui' | 'env'
  created_at?: string
  created_by?: string
  updated_at?: string
  updated_by?: string
  readable: boolean
  shadows?: boolean
}

export type SecretKey = { id: string; master: string; active: boolean; created_at: string; secrets: number }

export type SecretsPage = {
  master_id: string
  configured_id: string
  stored_id: string
  problem: string
  config_outdated: boolean
  secrets: Secret[]
  keys: SecretKey[]
}

export type RunPoint = { id: string; status: RunStatus; trigger: string; scheduled_at: string; duration_ms: number }

export type CalendarPage = {
  from: string
  to: string
  items: { job_id: string; job_name: string; at: string; skipped?: string }[]
  capped: string[]
  windows: { expr: string; from: string; to: string; tags: string[]; pause?: boolean }[]
}

export type BulkResult = { done: string[]; failed: Record<string, string> }

export type TestRun = {
  status: RunStatus
  error: string
  output: string
  output_truncated: boolean
  result: string
  result_code: number | null
  duration_ms: number
}

export type Problem = { line: number; column: number; message: string }

export type AuditEntry = {
  id: string
  at: string
  actor: string
  via: string
  action: string
  job_id: string
  job_name: string
  run_id: string
  detail: Record<string, unknown>
}

export type BuildInfo = {
  version: string
  commit: string
  date: string
  go_version: string
  os: string
  arch: string
  repository: string
}

export type SchedulerInfo = {
  poll_interval: string
  max_concurrent: number
  max_output: number
  shutdown_timeout: string
  node_timeout: string
}

export type Info = {
  name: string
  version: string
  build: BuildInfo
  node: string
  executors: string[]
  scheduler: SchedulerInfo
  user: string
  user_header: string
  secrets: string[]
  mcp: { enabled: boolean; read_only: boolean }
  environment: { name: string; color: string }
  retention: { runs: string; runs_per_job: number; audit: string; at: string; zone: string; notifications?: string }
  settings: { editable: boolean }
  paused: boolean
  metrics: { enabled: boolean; collector: boolean }
}

export type SettingType = 'bool' | 'int' | 'string' | 'duration' | 'list' | 'time' | 'choice'

export type SettingEntry = {
  key: string
  group: string
  label: string
  description: string
  type: SettingType
  choices?: string[]
  min?: number
  max?: number
  placeholder?: string
  danger?: boolean
  unit?: string
  value: unknown
  base: unknown
  overridden: boolean
  updated_at?: string
  updated_by?: string
}

export type SettingsPage = {
  editable: boolean
  groups: { id: string; label: string; description: string }[]
  entries: SettingEntry[]
}

export type RunQuery = {
  job_id?: string
  status?: string
  q?: string
  user?: string
  mine?: string
  from?: string
  to?: string
  before?: string
  limit?: number
}

export type Stats = {
  jobs: number
  enabled: number
  running: number
  queued: number
  failed_24h: number
  succeeded_24h: number
}

export type Node = { name: string; version: string; started_at: string; seen_at: string; running: number }

export type Preview = { times: string[]; error?: string }

function qs(params: Record<string, string | number | undefined>) {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') q.set(k, String(v))
  const s = q.toString()
  return s ? `?${s}` : ''
}

const enc = encodeURIComponent

type S = AbortSignal | undefined

export const api = {
  info: (s?: S) => request<Info>('GET', '/info', undefined, s),
  stats: (s?: S) => request<Stats>('GET', '/stats', undefined, s),
  nodes: (s?: S) => request<Node[]>('GET', '/nodes', undefined, s),
  tags: (s?: S) => request<string[]>('GET', '/tags', undefined, s),
  next: (schedule: string, timezone: string, n = 5, s?: S) =>
    request<Preview>('GET', '/schedule/next' + qs({ schedule, timezone, n }), undefined, s),

  jobs: (search = '', tag = '', s?: S) => request<Job[]>('GET', '/jobs' + qs({ search, tag }), undefined, s),
  job: (id: string, s?: S) => request<Job>('GET', `/jobs/${enc(id)}`, undefined, s),
  createJob: (j: JobInput) => request<Job>('POST', '/jobs', j),
  updateJob: (id: string, j: JobInput) => request<Job>('PUT', `/jobs/${enc(id)}`, j),
  deleteJob: (id: string) => request<void>('DELETE', `/jobs/${enc(id)}`),
  setEnabled: (id: string, on: boolean) => request<Job>('POST', `/jobs/${enc(id)}/${on ? 'enable' : 'disable'}`),
  trigger: (id: string, params?: Record<string, string>) =>
    request<{ run_id: string }>('POST', `/jobs/${enc(id)}/trigger`, params && Object.keys(params).length ? { params } : undefined),
  newHook: (id: string) => request<{ token: string }>('POST', `/jobs/${enc(id)}/hook`),
  removeHook: (id: string) => request<void>('DELETE', `/jobs/${enc(id)}/hook`),
  jobStats: (id: string, n = 100, s?: S) => request<RunPoint[]>('GET', `/jobs/${enc(id)}/stats` + qs({ n }), undefined, s),
  jobHistory: (id: string, s?: S) => request<AuditEntry[]>('GET', `/jobs/${enc(id)}/history`, undefined, s),
  bulk: (action: 'pause' | 'resume' | 'delete' | 'trigger', ids: string[]) => request<BulkResult>('POST', '/jobs/bulk', { action, ids }),
  calendar: (from: string, to: string, tag = '', s?: S) => request<CalendarPage>('GET', '/calendar' + qs({ from, to, tag }), undefined, s),

  runs: (p: RunQuery = {}, s?: S) => request<Run[]>('GET', '/runs' + qs(p), undefined, s),
  run: (id: string, s?: S) => request<Run>('GET', `/runs/${enc(id)}`, undefined, s),
  cancelRun: (id: string) => request<void>('POST', `/runs/${enc(id)}/cancel`),

  testJob: (j: JobInput & { params?: Record<string, string> }, s?: S) => request<TestRun>('POST', '/jobs/test', j, s),
  checkScript: (script: string, s?: S) => request<{ problems: Problem[] }>('POST', '/script/check', { script }, s),
  audit: (p: { job_id?: string; user?: string; before?: string; limit?: number } = {}, s?: S) =>
    request<AuditEntry[]>('GET', '/audit' + qs(p), undefined, s),

  settings: (s?: S) => request<SettingsPage>('GET', '/settings', undefined, s),
  putSettings: (c: { values?: Record<string, unknown>; reset?: string[] }) => request<SettingsPage>('PUT', '/settings', c),
  cleanup: () => request<{ runs: number; audit: number }>('POST', '/settings/cleanup', {}),
  checkExecutor: (name: string) => request<{ ok: boolean; message: string }>('POST', `/settings/check/${enc(name)}`, {}),

  secrets: (s?: S) => request<SecretsPage>('GET', '/secrets', undefined, s),
  putSecret: (name: string, v: { description: string; value?: string }) => request<SecretsPage>('PUT', `/secrets/${enc(name)}`, v),
  deleteSecret: (name: string) => request<SecretsPage>('DELETE', `/secrets/${enc(name)}`),
  changeSecretsKey: (key: string) => request<SecretsPage>('PUT', '/secret-key', { key }),

  channels: (s?: S) => request<Channel[]>('GET', '/channels', undefined, s),
  createChannel: (c: ChannelInput) => request<Channel>('POST', '/channels', c),
  updateChannel: (id: string, c: ChannelInput) => request<Channel>('PUT', `/channels/${enc(id)}`, c),
  deleteChannel: (id: string) => request<void>('DELETE', `/channels/${enc(id)}`),
  testChannel: (id: string) => request<{ ok: boolean; message: string }>('POST', `/channels/${enc(id)}/test`, {}),
  notifications: (p: { job_id?: string; limit?: number } = {}, s?: S) =>
    request<Notification[]>('GET', '/notifications' + qs(p), undefined, s),
}

// runEvents follows a run as the server streams it; onRun gets the run on
// every change. It returns a function that stops following.
export function runEvents(id: string, onRun: (r: Run) => void, onError: () => void): () => void {
  const es = new EventSource(`${base}/runs/${enc(id)}/events`)
  es.addEventListener('run', (e) => {
    try {
      onRun(JSON.parse((e as MessageEvent).data))
    } catch {}
  })
  es.onerror = () => onError()
  return () => es.close()
}
