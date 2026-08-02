/**
 * Typed client for the Genesis control plane.
 *
 * The desktop app talks to a loopback sidecar process, so there is no CORS
 * dance and no cookie handling: the Tauri shell supplies the token it read from
 * the server's session file at startup.
 */

export const API_BASE =
  (import.meta.env.VITE_GENESIS_API as string | undefined) ?? 'http://127.0.0.1:8787'

export interface ApiErrorBody {
  code: string
  message: string
  request_id?: string
  fields?: Record<string, string>
}

export class ApiError extends Error {
  readonly status: number
  readonly body: ApiErrorBody

  constructor(status: number, body: ApiErrorBody) {
    super(body.message)
    this.name = 'ApiError'
    this.status = status
    this.body = body
  }
}

let accessToken: string | null = null
let refreshToken: string | null = null

export function setToken(token: string | null): void {
  accessToken = token
}

export function getToken(): string | null {
  return accessToken
}

export function setRefreshToken(token: string | null): void {
  refreshToken = token
}

/**
 * Called whenever a new access token is obtained, so the shell can persist it.
 * Set by the Tauri bootstrap; a no-op in the browser.
 */
let onTokenRenewed: ((access: string, refresh: string) => void) | null = null

export function setTokenRenewalHandler(
  handler: ((access: string, refresh: string) => void) | null,
): void {
  onTokenRenewed = handler
}

/**
 * Exchanges the refresh token for a new pair.
 *
 * Access tokens live 15 minutes by design — they cannot be revoked, so a short
 * life is the only bound on a stolen one. That makes silent renewal a
 * requirement rather than a nicety: without it the app simply stops working a
 * quarter of an hour after launch, which is what "token expired" was.
 *
 * The in-flight promise is shared. Several requests usually fail at once when a
 * token lapses, and letting each start its own refresh would spend the
 * single-use refresh token repeatedly — the server treats reuse as theft and
 * revokes the entire family, turning an expiry into a forced logout.
 */
let refreshInFlight: Promise<boolean> | null = null

async function renewSession(): Promise<boolean> {
  if (!refreshToken) return false

  refreshInFlight ??= (async () => {
    try {
      const res = await fetch(`${API_BASE}/api/v1/auth/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: refreshToken }),
      })
      if (!res.ok) return false

      const body = (await res.json()) as {
        access_token?: string
        refresh_token?: string
      }
      if (!body.access_token) return false

      accessToken = body.access_token
      // Refresh tokens rotate: keeping the old one would present a retired
      // token next time, which the server reads as a replay.
      if (body.refresh_token) refreshToken = body.refresh_token

      onTokenRenewed?.(accessToken, refreshToken ?? '')
      return true
    } catch {
      return false
    } finally {
      // Cleared on the next tick so concurrent callers all observe the same
      // outcome before another attempt can begin.
      queueMicrotask(() => {
        refreshInFlight = null
      })
    }
  })()

  return refreshInFlight
}

async function send(path: string, init: RequestInit): Promise<Response> {
  const headers = new Headers(init.headers)
  if (init.body) headers.set('Content-Type', 'application/json')
  if (accessToken) headers.set('Authorization', `Bearer ${accessToken}`)

  try {
    return await fetch(`${API_BASE}${path}`, { ...init, headers })
  } catch {
    // A network failure against a loopback sidecar almost always means the
    // server process is not running; say that rather than "Failed to fetch".
    throw new ApiError(0, {
      code: 'server_unreachable',
      message: 'Cannot reach the Genesis engine. Is the server running?',
    })
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  let res = await send(path, init)

  // One transparent retry on 401. The refresh endpoint itself is excluded, or
  // a failing refresh would recurse.
  if (res.status === 401 && !path.startsWith('/api/v1/auth/')) {
    if (await renewSession()) {
      res = await send(path, init)
    }
  }

  if (res.status === 204) return undefined as T

  const text = await res.text()
  const parsed: unknown = text ? JSON.parse(text) : {}

  if (!res.ok) {
    const body = (parsed as { error?: ApiErrorBody }).error ?? {
      code: 'unknown_error',
      message: res.statusText,
    }

    // A 401 that survived the retry means the session is genuinely finished.
    // The generic "token expired" is accurate and useless: it reads like a
    // licence problem when it is a local sign-in that lapsed.
    if (res.status === 401) {
      throw new ApiError(res.status, {
        ...body,
        code: 'session_expired',
        message:
          'Your local session has ended. Restart the app to sign in again — ' +
          'Genesis is free and offline; this is not a licence or billing issue.',
      })
    }
    throw new ApiError(res.status, body)
  }
  return parsed as T
}

// --- domain types ---------------------------------------------------------

export type ProjectStatus = 'draft' | 'building' | 'ready' | 'failed' | 'archived'
export type RunStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'canceled' | 'interrupted'
export type PhaseStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'skipped'
export type AgentState = 'idle' | 'working' | 'blocked' | 'done' | 'failed'
export type Level = 'debug' | 'info' | 'warn' | 'error'

export interface Project {
  id: string
  owner_id: string
  name: string
  slug: string
  prompt: string
  description: string
  category: string
  status: ProjectStatus
  workspace_path: string
  created_at: string
  updated_at: string
}

export interface Phase {
  id: string
  name: string
  title: string
  ordinal: number
  status: PhaseStatus
  summary: Record<string, unknown>
  started_at: string | null
  finished_at: string | null
}

export interface Run {
  id: string
  project_id: string
  kind: string
  status: RunStatus
  current_phase: string
  progress: number
  result: Record<string, unknown>
  error: { code: string; message: string; phase?: string } | null
  phases: Phase[]
  started_at: string | null
  finished_at: string | null
  created_at: string
}

export interface GenesisEvent {
  seq: number
  id: string
  run_id?: string
  type: string
  agent_role?: string
  agent_name?: string
  level: Level
  message: string
  payload?: Record<string, unknown>
  created_at: string
}

export interface AgentProfile {
  role: string
  name: string
  title: string
  mission: string
  phase: string
  produces: string[]
  model_class: string
  accent: string
}

export interface AgentStatus {
  profile: AgentProfile
  status: AgentState
  task?: string
  artifacts: number
  last_event?: string
}

export interface Artifact {
  id: string
  run_id: string
  kind: string
  name: string
  mime: string
  size_bytes: number
  body?: string
  created_at: string
}

export interface Classification {
  category: string
  confidence: number
  matched_signals: string[]
}

export interface BlueprintSummary {
  key: string
  name: string
  category: string
  description: string
  entities: number
  screens: number
  epics: string[]
}

export interface FileNode {
  name: string
  path: string
  dir: boolean
  size?: number
  language?: string
  children?: FileNode[]
}

export interface FileContent {
  path: string
  content: string
  language: string
  size: number
  sha256: string
  binary: boolean
  read_only: boolean
}

export interface SearchHit {
  path: string
  line: number
  text: string
}

export interface Commit {
  sha: string
  short: string
  subject: string
  author: string
  when: string
  files?: string[]
  added: number
  removed: number
}

export interface VCSStatus {
  branch: string
  head: string
  clean: boolean
  modified?: string[]
  untracked?: string[]
  deleted?: string[]
}

export interface Meta {
  name: string
  version: string
  commit: string
  capabilities: Record<string, unknown>
}

export interface ModelInfo {
  id: string
  classes: string[]
  context: number
}

export interface ModelStatus {
  enabled: boolean
  provider?: string
  reason?: string
  data: ModelInfo[]
}

// --- operations -----------------------------------------------------------

export const api = {
  meta: () => request<Meta>('/api/v1/meta'),

  login: (email: string, password: string) =>
    request<{ access_token: string; refresh_token: string; user: { id: string; email: string } }>(
      '/api/v1/auth/login',
      { method: 'POST', body: JSON.stringify({ email, password }) },
    ),

  projects: () => request<{ data: Project[]; total: number }>('/api/v1/projects?limit=100'),

  project: (id: string) => request<Project>(`/api/v1/projects/${id}`),

  createProject: (prompt: string, start = true) =>
    request<{ project: Project; run?: Run; run_error?: string }>('/api/v1/projects', {
      method: 'POST',
      body: JSON.stringify({ prompt, start }),
    }),

  deleteProject: (id: string) => request<void>(`/api/v1/projects/${id}`, { method: 'DELETE' }),

  classify: (prompt: string) =>
    request<{ classification: Classification; blueprint: BlueprintSummary; suggested_name: string }>(
      '/api/v1/classify',
      { method: 'POST', body: JSON.stringify({ prompt }) },
    ),

  blueprints: () => request<{ data: BlueprintSummary[] }>('/api/v1/blueprints'),

  agents: () => request<{ data: AgentProfile[] }>('/api/v1/agents'),

  models: () => request<ModelStatus>('/api/v1/models'),

  // --- workspace (IDE) ---

  files: (projectId: string) =>
    request<{ data: FileNode[] }>(`/api/v1/projects/${projectId}/files`),

  /**
   * Downloads the project as a zip.
   *
   * The browser's download machinery expects a URL, not a fetch response, but
   * the request needs an Authorization header — so the archive is fetched,
   * turned into an object URL and handed to a synthetic anchor. The object URL
   * is revoked afterwards or the blob stays resident for the life of the
   * window.
   */
  exportProject: async (projectId: string, suggestedName: string): Promise<void> => {
    const headers = new Headers()
    if (accessToken) headers.set('Authorization', `Bearer ${accessToken}`)

    let res = await fetch(`${API_BASE}/api/v1/projects/${projectId}/export`, { headers })
    if (res.status === 401) {
      // Reuse the shared renewal so a long editing session does not fail here.
      await api.projects().catch(() => undefined)
      const retry = new Headers()
      if (accessToken) retry.set('Authorization', `Bearer ${accessToken}`)
      res = await fetch(`${API_BASE}/api/v1/projects/${projectId}/export`, { headers: retry })
    }
    if (!res.ok) {
      throw new ApiError(res.status, {
        code: 'export_failed',
        message: 'Could not package the project for download.',
      })
    }

    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = `${suggestedName || 'project'}.zip`
    document.body.appendChild(anchor)
    anchor.click()
    anchor.remove()
    URL.revokeObjectURL(url)
  },

  readFile: (projectId: string, path: string) =>
    request<FileContent>(`/api/v1/projects/${projectId}/file?path=${encodeURIComponent(path)}`),

  writeFile: (projectId: string, path: string, content: string, baseSha: string) =>
    request<FileContent>(`/api/v1/projects/${projectId}/file`, {
      method: 'PUT',
      body: JSON.stringify({ path, content, base_sha256: baseSha }),
    }),

  searchWorkspace: (projectId: string, query: string) =>
    request<{ data: SearchHit[] }>(
      `/api/v1/projects/${projectId}/search?q=${encodeURIComponent(query)}`,
    ),

  history: (projectId: string) =>
    request<{ data: Commit[] }>(`/api/v1/projects/${projectId}/history`),

  diff: (projectId: string, ref = '') =>
    request<{ diff: string }>(`/api/v1/projects/${projectId}/diff?ref=${encodeURIComponent(ref)}`),

  vcsStatus: (projectId: string) =>
    request<VCSStatus>(`/api/v1/projects/${projectId}/vcs`),

  rollback: (projectId: string, ref: string) =>
    request<void>(`/api/v1/projects/${projectId}/rollback`, {
      method: 'POST',
      body: JSON.stringify({ ref }),
    }),

  runs: (projectId: string) => request<{ data: Run[] }>(`/api/v1/projects/${projectId}/runs`),

  startRun: (projectId: string) =>
    request<Run>(`/api/v1/projects/${projectId}/runs`, {
      method: 'POST',
      body: JSON.stringify({ kind: 'build' }),
    }),

  run: (id: string) => request<Run>(`/api/v1/runs/${id}`),

  cancelRun: (id: string) => request<Run>(`/api/v1/runs/${id}/cancel`, { method: 'POST' }),

  events: (runId: string, afterSeq = 0) =>
    request<{ data: GenesisEvent[]; next_seq: number }>(
      `/api/v1/runs/${runId}/events?after_seq=${afterSeq}&limit=500`,
    ),

  runAgents: (runId: string) => request<{ data: AgentStatus[] }>(`/api/v1/runs/${runId}/agents`),

  runArtifacts: (runId: string) => request<{ data: Artifact[] }>(`/api/v1/runs/${runId}/artifacts`),

  // Every document the project has accumulated, across all its runs.
  projectArtifacts: (projectId: string) =>
    request<{ data: Artifact[] }>(`/api/v1/projects/${projectId}/artifacts`),

  artifact: (id: string) => request<Artifact>(`/api/v1/artifacts/${id}`),
}
