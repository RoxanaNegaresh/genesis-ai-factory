import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Boxes,
  Code2,
  Cpu,
  FileText,
  FolderOpen,
  LayoutGrid,
  Loader2,
  Moon,
  RefreshCw,
  ShieldCheck,
  Sun,
} from 'lucide-react'
import {
  api,
  setToken,
  setRefreshToken,
  setTokenRenewalHandler,
  type Meta,
  type ModelStatus,
  type Project,
} from '@/lib/api'
import Workspace from '@/views/Workspace'
import RunView from '@/views/RunView'
import CodeEditor from '@/views/Editor'
import Delivered from '@/views/Delivered'
import { Badge, Button } from '@/components/ui/primitives'
import { cn } from '@/lib/utils'
import { invoke, tryInvoke, listen, isDesktop } from '@/lib/shell'

type Screen =
  | { kind: 'workspace' }
  | { kind: 'run'; runId: string }
  | { kind: 'delivered'; projectId: string }
  | { kind: 'editor'; projectId: string }

export default function App() {
  const [meta, setMeta] = useState<Meta | null>(null)
  const [projects, setProjects] = useState<Project[]>([])
  const [screen, setScreen] = useState<Screen>({ kind: 'workspace' })
  const [models, setModels] = useState<ModelStatus | null>(null)
  const [booting, setBooting] = useState(true)
  const [bootError, setBootError] = useState<string | null>(null)
  const [bootAttempt, setBootAttempt] = useState(0)
  const [theme, setTheme] = useState<'dark' | 'light'>(
    () => (localStorage.getItem('genesis.theme') as 'dark' | 'light') ?? 'dark',
  )
  const editorSaveRef = useRef<(() => void) | null>(null)

  const refreshProjects = useCallback(async () => {
    try {
      const result = await api.projects()
      setProjects(result.data)
    } catch {
      // The project list is not worth an error screen; the boot check already
      // reports an unreachable server.
    }
  }, [])

  useEffect(() => {
    const boot = async () => {
      // In a packaged build the Tauri shell injects the loopback token it read
      // from the server's session file. In browser development the token is
      // supplied through Vite env so the same code path works either way.
      const shell = window as unknown as {
        __GENESIS_TOKEN__?: string
        __GENESIS_REFRESH__?: string
      }
      const injected =
        shell.__GENESIS_TOKEN__ ?? (import.meta.env.VITE_GENESIS_TOKEN as string | undefined)
      if (injected) setToken(injected)

      // The refresh token is what keeps the app usable past the access
      // token's 15-minute life. Without it the UI worked for a quarter of an
      // hour and then reported "token expired", which reads like a licence
      // problem and is in fact a lapsed local sign-in.
      const injectedRefresh =
        shell.__GENESIS_REFRESH__ ??
        (import.meta.env.VITE_GENESIS_REFRESH as string | undefined)
      if (injectedRefresh) setRefreshToken(injectedRefresh)

      // Renewal rotates both tokens; mirror them back onto the shell so a
      // reload picks up the current pair rather than the one from launch.
      setTokenRenewalHandler((access, refresh) => {
        shell.__GENESIS_TOKEN__ = access
        shell.__GENESIS_REFRESH__ = refresh
      })

      // The engine is a child process that takes a moment to bind its port.
      // A single attempt at startup is a race the UI loses on a cold machine,
      // which is what produced the "engine is not responding" screen on a
      // perfectly healthy install. Poll briefly instead of failing instantly.
      const deadline = Date.now() + 25_000
      let lastError: unknown = null

      for (;;) {
        try {
          setMeta(await api.meta())
          await refreshProjects()
          // Model status is informational: a failure here must not block boot.
          try {
            setModels(await api.models())
          } catch {
            setModels({ enabled: false, data: [] })
          }
          setBootError(null)
          setBooting(false)
          return
        } catch (err) {
          lastError = err
          if (Date.now() > deadline) break

          // Ask the shell to (re)start the engine, then pick up the session it
          // writes. Without re-reading, a token injected before the engine was
          // ready would stay stale for the life of the window.
          if (isDesktop()) {
            try {
              const status = await invoke<{ token?: string; refresh_token?: string }>(
                'engine_status',
              )
              if (status?.token) setToken(status.token)
              if (status?.refresh_token) setRefreshToken(status.refresh_token)
            } catch {
              // The shell is unavailable; the retry below still applies.
            }
          }
          await new Promise((resolve) => setTimeout(resolve, 900))
        }
      }

      setBootError(
        lastError instanceof Error ? lastError.message : 'Cannot reach the Genesis engine',
      )
      setBooting(false)
    }
    void boot()
  }, [refreshProjects, bootAttempt])

  // Theme is applied to <html> so tokens cascade to portals and overlays that
  // render outside the React tree.
  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark')
    document.documentElement.classList.toggle('light', theme === 'light')
    localStorage.setItem('genesis.theme', theme)
  }, [theme])

  // Opening a project jumps to its most recent run, which is what the user
  // almost always wants to see.
  const [lastProject, setLastProject] = useState<string | null>(null)

  const openProject = useCallback(async (projectId: string) => {
    setLastProject(projectId)
    try {
      const runs = await api.runs(projectId)
      if (runs.data.length > 0) {
        setScreen({ kind: 'run', runId: runs.data[0].id })
        return
      }
      const run = await api.startRun(projectId)
      setScreen({ kind: 'run', runId: run.id })
    } catch {
      setScreen({ kind: 'workspace' })
    }
  }, [])

  // Native menu commands. The shell performs only the actions needing OS
  // access and forwards the rest here, so adding an item is a frontend change.
  useEffect(() => {
    let dispose = () => {}
    void listen<string>('menu', (id) => {
      switch (id) {
        case 'new-project':
          setScreen({ kind: 'workspace' })
          window.dispatchEvent(new CustomEvent('genesis:new-project'))
          break
        case 'view-projects':
          setScreen({ kind: 'workspace' })
          break
        case 'view-build':
          setScreen((current) => (current.kind === 'run' ? current : current))
          break
        case 'view-editor':
          if (lastProject) setScreen({ kind: 'editor', projectId: lastProject })
          break
        case 'save':
          editorSaveRef.current?.()
          break
        case 'toggle-theme':
          setTheme((t) => (t === 'dark' ? 'light' : 'dark'))
          break
        case 'reload':
          window.location.reload()
          break
        case 'engine-restart':
          setBooting(true)
          void tryInvoke('restart_engine').then(() => setBootAttempt((n) => n + 1))
          break
        case 'engine-status':
          void tryInvoke<{ message: string }>('engine_status').then((s) => {
            if (s) window.alert(s.message)
          })
          break
        case 'open-workspace':
          if (lastProject) {
            const project = projects.find((p) => p.id === lastProject)
            if (project?.workspace_path) {
              void tryInvoke('open_workspace', { path: project.workspace_path })
            }
          }
          break
        case 'help-docs':
          window.dispatchEvent(new CustomEvent('genesis:help'))
          break
        case 'help-shortcuts':
          window.dispatchEvent(new CustomEvent('genesis:shortcuts'))
          break
      }
    }).then((off) => {
      dispose = off
    })
    return () => dispose()
  }, [lastProject, projects])

  if (booting) {
    return (
      <div className="flex h-screen flex-col items-center justify-center gap-6 bg-bg">
        <div className="relative">
          <div className="absolute inset-0 animate-ping rounded-2xl bg-accent/20" />
          <div className="relative flex h-14 w-14 items-center justify-center rounded-2xl bg-accent shadow-lg shadow-accent-ring">
            <Boxes className="h-7 w-7 text-accent-fg" />
          </div>
        </div>
        <div className="flex flex-col items-center gap-1.5">
          <p className="text-sm font-semibold tracking-tight text-fg">Genesis AI Factory</p>
          <p className="flex items-center gap-2 text-xs text-fg-subtle">
            <Loader2 className="h-3 w-3 animate-spin" />
            Starting the engine…
          </p>
        </div>
      </div>
    )
  }

  if (bootError) {
    return <EngineDownScreen detail={bootError} onRetry={() => {
      setBooting(true)
      setBootError(null)
      setBootAttempt((n) => n + 1)
    }} />
  }

  return (
    <div className="flex h-screen flex-col overflow-hidden bg-bg text-fg">
      <TitleBar
        meta={meta}
        models={models}
        theme={theme}
        onToggleTheme={() => setTheme((t) => (t === 'dark' ? 'light' : 'dark'))}
      />
      <div className="flex min-h-0 flex-1">
        <Sidebar
          active={screen.kind}
          projectCount={projects.length}
          canEdit={lastProject !== null}
          onNavigate={(kind) => {
            if (kind === 'workspace') setScreen({ kind: 'workspace' })
            if (kind === 'editor' && lastProject) setScreen({ kind: 'editor', projectId: lastProject })
          }}
        />
        <main className="min-w-0 flex-1 overflow-hidden">
          {screen.kind === 'workspace' && (
            <Workspace
              projects={projects}
              onProjectCreated={(_, runId) => runId && setScreen({ kind: 'run', runId })}
              onOpenProject={(id) => void openProject(id)}
              refreshProjects={() => void refreshProjects()}
            />
          )}
          {screen.kind === 'run' && (
            <RunView
              runId={screen.runId}
              onBack={() => {
                void refreshProjects()
                setScreen({ kind: 'workspace' })
              }}
              onOpenEditor={(projectId) => {
                setLastProject(projectId)
                // Land on the delivery summary, not the raw editor: the user
                // has just asked for a product, and a file tree is not an
                // answer to "is it done and what do I do now?".
                void refreshProjects()
                setScreen({ kind: 'delivered', projectId })
              }}
            />
          )}
          {screen.kind === 'delivered' &&
            (() => {
              const project = projects.find((p) => p.id === screen.projectId)
              if (!project) return null
              return (
                <Delivered
                  project={project}
                  onOpenEditor={() => {
                    setLastProject(project.id)
                    setScreen({ kind: 'editor', projectId: project.id })
                  }}
                  onBack={() => {
                    void refreshProjects()
                    setScreen({ kind: 'workspace' })
                  }}
                />
              )
            })()}
          {screen.kind === 'editor' && <CodeEditor projectId={screen.projectId} />}
        </main>
      </div>
    </div>
  )
}

function TitleBar({
  meta,
  models,
  theme,
  onToggleTheme,
}: {
  meta: Meta | null
  models: ModelStatus | null
  theme: 'dark' | 'light'
  onToggleTheme: () => void
}) {
  return (
    // The window frame is drawn by the operating system, so this is an
    // application toolbar rather than a title bar. Reimplementing minimise,
    // maximise, close, snapping and the system menu is work that only ever
    // approximates the platform; letting the OS draw them means the app
    // behaves like every other program on the machine.
    <header className="flex h-11 shrink-0 items-center gap-3 border-b border-border bg-surface-1 px-4 select-none">
      <div className="flex items-center gap-2.5">
        <div className="flex h-6 w-6 items-center justify-center rounded-md bg-accent shadow-sm shadow-accent-ring">
          <Boxes className="h-3.5 w-3.5 text-accent-fg" />
        </div>
        <div className="leading-none">
          <span className="text-[13px] font-semibold tracking-tight text-fg">Genesis</span>
          <span className="ml-1.5 text-[11px] text-fg-subtle">AI Factory</span>
        </div>
      </div>

      <div className="ml-auto flex items-center gap-2">
        {meta && <Badge tone="neutral">v{meta.version}</Badge>}
        {/* Whether reasoning is active changes how much to trust the output, so
            it is shown permanently rather than hidden in a settings pane. */}
        {models?.enabled ? (
          <Badge tone="success" title={models.data.map((m) => m.id).join(', ')}>
            reasoning on
          </Badge>
        ) : (
          <Badge tone="warning" title={models?.reason ?? 'no inference server configured'}>
            blueprint only
          </Badge>
        )}
        <button
          type="button"
          onClick={onToggleTheme}
          title={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
          aria-label={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
          className="flex h-7 w-7 items-center justify-center rounded-md text-fg-subtle transition-colors hover:bg-surface-3 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring"
        >
          {theme === 'dark' ? <Sun className="h-3.5 w-3.5" /> : <Moon className="h-3.5 w-3.5" />}
        </button>
      </div>
    </header>
  )
}

function Sidebar({
  active,
  projectCount,
  canEdit,
  onNavigate,
}: {
  active: string
  projectCount: number
  canEdit: boolean
  onNavigate: (kind: 'workspace' | 'editor') => void
}) {
  return (
    // Labelled navigation rather than bare icons. An icon-only rail forces the
    // user to hover and wait for a tooltip to learn what anything does, which
    // is a tax paid on every visit by anyone who is not already an expert.
    <aside className="flex w-[184px] shrink-0 flex-col gap-0.5 border-r border-border bg-surface-1 p-2.5">
      <p className="px-2 pb-1.5 pt-1 text-[10px] font-semibold uppercase tracking-wider text-fg-subtle">
        Workspace
      </p>
      <SidebarButton
        label="Projects"
        badge={projectCount || undefined}
        active={active === 'workspace'}
        onClick={() => onNavigate('workspace')}
      >
        <LayoutGrid className="h-4 w-4" />
      </SidebarButton>
      <SidebarButton
        label="Code editor"
        hint={canEdit ? undefined : 'Open a project first'}
        active={active === 'editor'}
        disabled={!canEdit}
        onClick={() => onNavigate('editor')}
      >
        <Code2 className="h-4 w-4" />
      </SidebarButton>

      <div className="mt-auto rounded-lg border border-border bg-surface-2 p-2.5">
        <p className="flex items-center gap-1.5 text-[10px] font-semibold text-fg">
          <ShieldCheck className="h-3 w-3 text-success" />
          Runs locally
        </p>
        <p className="mt-1 text-[10px] leading-relaxed text-fg-subtle">
          No account, no API key, no subscription.
        </p>
      </div>
    </aside>
  )
}

function SidebarButton({
  children,
  label,
  badge,
  hint,
  active,
  disabled,
  onClick,
}: {
  children: React.ReactNode
  label: string
  badge?: number
  hint?: string
  active: boolean
  disabled?: boolean
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      title={hint ?? label}
      aria-current={active ? 'page' : undefined}
      className={cn(
        'group flex items-center gap-2.5 rounded-lg px-2 py-1.5 text-left text-[13px] transition-colors',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring',
        active
          ? 'bg-accent-soft font-medium text-accent'
          : 'text-fg-muted hover:bg-surface-2 hover:text-fg',
        disabled && 'cursor-not-allowed opacity-40 hover:bg-transparent hover:text-fg-muted',
      )}
    >
      <span className={cn('shrink-0', active ? 'text-accent' : 'text-fg-subtle')}>{children}</span>
      <span className="min-w-0 flex-1 truncate">{label}</span>
      {badge !== undefined && (
        <span
          className={cn(
            'shrink-0 rounded-full px-1.5 py-px text-[10px] font-medium tabular-nums',
            active ? 'bg-accent text-accent-fg' : 'bg-surface-3 text-fg-subtle',
          )}
        >
          {badge}
        </span>
      )}
    </button>
  )
}

/**
 * Shown when the engine cannot be reached.
 *
 * The previous version told the user to "start it with genesis-server", which
 * is wrong for a packaged application — the engine ships inside the app and
 * there is no command to run. That instruction sent people looking for a
 * missing install that was never missing.
 *
 * This screen offers the three things that actually resolve it, in the order
 * they are likely to help, and states plainly that the product is free and
 * local so an authentication failure is never read as a billing problem.
 */
function EngineDownScreen({ detail, onRetry }: { detail: string; onRetry: () => void }) {
  const [busy, setBusy] = useState(false)

  const restart = async () => {
    setBusy(true)
    await tryInvoke('restart_engine')
    setBusy(false)
    onRetry()
  }

  return (
    <div className="flex h-screen items-center justify-center bg-bg px-6">
      <div className="w-full max-w-lg">
        <div className="rounded-2xl border border-border bg-surface-1 p-8 shadow-xl">
          <div className="flex items-start gap-4">
            <div className="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-danger/10">
              <Cpu className="h-5 w-5 text-danger" />
            </div>
            <div className="min-w-0 space-y-1">
              <h1 className="text-base font-semibold tracking-tight text-fg">
                The engine did not start
              </h1>
              <p className="text-xs leading-relaxed text-fg-muted">
                Genesis runs a local engine inside this application. It failed to
                answer within 25&nbsp;seconds.
              </p>
            </div>
          </div>

          <p className="mt-5 break-words rounded-lg bg-surface-2 px-3 py-2 font-mono text-[11px] leading-relaxed text-fg-subtle">
            {detail}
          </p>

          <div className="mt-6 flex flex-wrap gap-2">
            <Button variant="primary" onClick={restart} disabled={busy}>
              {busy ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <RefreshCw className="h-3.5 w-3.5" />
              )}
              Restart engine
            </Button>
            <Button variant="ghost" onClick={() => void tryInvoke('open_engine_log')}>
              <FileText className="h-3.5 w-3.5" />
              View log
            </Button>
            <Button variant="ghost" onClick={() => void tryInvoke('open_data_dir')}>
              <FolderOpen className="h-3.5 w-3.5" />
              Data folder
            </Button>
          </div>

          <div className="mt-6 flex items-start gap-2 border-t border-border pt-4">
            <ShieldCheck className="mt-0.5 h-3.5 w-3.5 shrink-0 text-success" />
            <p className="text-[11px] leading-relaxed text-fg-subtle">
              Genesis is free and runs entirely on this machine. There is no account,
              no API key and no subscription — this is a local process that failed to
              start, never a licensing problem.
            </p>
          </div>
        </div>
      </div>
    </div>
  )
}
