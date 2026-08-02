/**
 * Run view — the agent monitoring dashboard, live pipeline and event stream.
 *
 * Every element here is a projection of the server's event log. Nothing is
 * inferred client-side, so what the operator sees is exactly what happened.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { motion } from 'framer-motion'
import { Activity, Ban, CheckCircle2, ChevronRight, CircleDashed, Code2, FileText, Loader2, XCircle } from 'lucide-react'
import {
  api,
  type AgentStatus,
  type Artifact,
  type GenesisEvent,
  type Run,
} from '@/lib/api'
import { streamRun, type StreamState } from '@/lib/stream'
import { Badge, Button, Card, ProgressBar, Skeleton, toneForStatus } from '@/components/ui/primitives'
import { cn, clockTime, formatBytes } from '@/lib/utils'

type Tab = 'stream' | 'agents' | 'artifacts'

export default function RunView({
  runId,
  onBack,
  onOpenEditor,
}: {
  runId: string
  onBack: () => void
  onOpenEditor?: (projectId: string) => void
}) {
  const [run, setRun] = useState<Run | null>(null)
  const [events, setEvents] = useState<GenesisEvent[]>([])
  const [agents, setAgents] = useState<AgentStatus[]>([])
  const [artifacts, setArtifacts] = useState<Artifact[]>([])
  const [selected, setSelected] = useState<Artifact | null>(null)
  const [tab, setTab] = useState<Tab>('stream')
  const [connection, setConnection] = useState<StreamState>('connecting')

  const refreshRun = useCallback(async () => {
    try {
      const [current, board] = await Promise.all([api.run(runId), api.runAgents(runId)])
      setRun(current)
      setAgents(board.data)
      if (current.status === 'succeeded' || current.status === 'failed') {
        const list = await api.runArtifacts(runId)
        setArtifacts(list.data)
      }
    } catch {
      // Transient failures are expected while a build starts; the stream and
      // the next poll will reconcile.
    }
  }, [runId])

  useEffect(() => {
    setEvents([])
    setRun(null)
    void refreshRun()

    const stream = streamRun(runId, 0, {
      onEvent: (event) => setEvents((prev) => [...prev, event]),
      onState: setConnection,
    })
    return () => stream.close()
  }, [runId, refreshRun])

  // Poll run state while active. The event stream carries the narrative; the
  // run record carries authoritative phase status, and they are refreshed
  // separately so a stream hiccup cannot freeze the pipeline display.
  useEffect(() => {
    if (run && (run.status === 'succeeded' || run.status === 'failed' || run.status === 'canceled')) return
    const timer = window.setInterval(() => void refreshRun(), 900)
    return () => clearInterval(timer)
  }, [run, refreshRun])

  const cancel = async () => {
    try {
      await api.cancelRun(runId)
      void refreshRun()
    } catch {
      /* the button simply has no effect if the run already finished */
    }
  }

  const active = run ? !['succeeded', 'failed', 'canceled', 'interrupted'].includes(run.status) : true

  return (
    <div className="flex h-full flex-col overflow-hidden">
      <header className="shrink-0 border-b border-border bg-surface-1 px-6 py-3">
        <div className="flex items-center gap-3">
          <button onClick={onBack} className="text-xs text-fg-subtle transition-colors hover:text-fg">
            Projects
          </button>
          <ChevronRight className="h-3 w-3 text-fg-subtle" />
          <span className="font-mono text-xs text-fg-muted">{runId.slice(0, 8)}</span>
          {run && <Badge tone={toneForStatus(run.status)}>{run.status}</Badge>}
          <span
            className={cn(
              'ml-auto flex items-center gap-1.5 text-[11px]',
              connection === 'live' ? 'text-success' : connection === 'polling' ? 'text-warning' : 'text-fg-subtle',
            )}
            title={connection === 'polling' ? 'Websocket unavailable; polling the event log' : undefined}
          >
            <span
              className={cn(
                'h-1.5 w-1.5 rounded-full',
                connection === 'live' ? 'animate-pulse bg-success' : connection === 'polling' ? 'bg-warning' : 'bg-fg-subtle',
              )}
            />
            {connection === 'live' ? 'live' : connection}
          </span>
          {run && !active && onOpenEditor && (
            <Button size="sm" variant="secondary" onClick={() => onOpenEditor(run.project_id)}>
              <Code2 className="h-3.5 w-3.5" />
              See what was built
            </Button>
          )}
          {active && (
            <Button size="sm" variant="ghost" onClick={() => void cancel()}>
              <Ban className="h-3.5 w-3.5" />
              Cancel
            </Button>
          )}
        </div>

        {run && (
          <div className="mt-3 space-y-2">
            <ProgressBar value={run.progress} />
            {/* Say what is happening in words, not just as a bar.
                A build spends most of its time in `npm install` and `go build`
                — real work that cannot be made faster — but with no
                explanation the user reads the wait as the program hanging.
                Naming the step, and saying it is normal, changes the same
                duration from a fault into progress. */}
            {active && (
              <p className="flex items-center gap-2 text-[11px] text-fg-muted">
                <Loader2 className="h-3 w-3 animate-spin text-accent" />
                <span>{describeProgress(run.phases)}</span>
              </p>
            )}
            <div className="flex flex-wrap gap-1.5">
              {run.phases.map((phase) => (
                <PhaseChip key={phase.id} title={phase.title} status={phase.status} />
              ))}
            </div>
          </div>
        )}
      </header>

      <nav className="flex shrink-0 gap-1 border-b border-border bg-surface-1 px-6" role="tablist">
        {([
          ['stream', 'Event stream', events.length],
          ['agents', 'Agents', agents.filter((a) => a.status === 'done').length],
          ['artifacts', 'Artifacts', artifacts.length],
        ] as const).map(([key, label, count]) => (
          <button
            key={key}
            role="tab"
            aria-selected={tab === key}
            onClick={() => setTab(key)}
            className={cn(
              'relative px-3 py-2 text-xs font-medium transition-colors',
              tab === key ? 'text-fg' : 'text-fg-subtle hover:text-fg-muted',
            )}
          >
            {label}
            {count > 0 && <span className="ml-1.5 text-[10px] text-fg-subtle">{count}</span>}
            {tab === key && (
              <motion.div layoutId="run-tab" className="absolute inset-x-0 -bottom-px h-0.5 bg-accent" />
            )}
          </button>
        ))}
      </nav>

      <div className="min-h-0 flex-1 overflow-hidden">
        {tab === 'stream' && <EventStream events={events} loading={!run} />}
        {tab === 'agents' && <AgentBoard agents={agents} />}
        {tab === 'artifacts' && (
          <ArtifactBrowser
            artifacts={artifacts}
            selected={selected}
            onSelect={async (artifact) => setSelected(await api.artifact(artifact.id))}
          />
        )}
      </div>
    </div>
  )
}

function PhaseChip({ title, status }: { title: string; status: string }) {
  const icon =
    status === 'succeeded' ? (
      <CheckCircle2 className="h-3 w-3 text-success" />
    ) : status === 'running' ? (
      <Loader2 className="h-3 w-3 animate-spin text-info" />
    ) : status === 'failed' ? (
      <XCircle className="h-3 w-3 text-danger" />
    ) : (
      <CircleDashed className="h-3 w-3 text-fg-subtle" />
    )

  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded border px-2 py-1 text-[11px]',
        status === 'succeeded' && 'border-success/25 bg-success/8 text-success',
        status === 'running' && 'border-info/30 bg-info/10 text-info',
        status === 'failed' && 'border-danger/30 bg-danger/10 text-danger',
        (status === 'pending' || status === 'skipped') && 'border-border bg-surface-2 text-fg-subtle',
      )}
    >
      {icon}
      {title}
    </span>
  )
}

function EventStream({ events, loading }: { events: GenesisEvent[]; loading: boolean }) {
  const bottomRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const [pinned, setPinned] = useState(true)

  // Auto-scroll, but only while the user is already at the bottom. Yanking the
  // viewport away from someone reading history is a genuinely hostile default.
  useEffect(() => {
    if (pinned) bottomRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' })
  }, [events, pinned])

  const onScroll = () => {
    const el = containerRef.current
    if (!el) return
    setPinned(el.scrollHeight - el.scrollTop - el.clientHeight < 80)
  }

  if (loading && events.length === 0) {
    return (
      <div className="space-y-2 p-6">
        {Array.from({ length: 6 }).map((_, i) => (
          <Skeleton key={i} className="h-5 w-full" />
        ))}
      </div>
    )
  }

  return (
    <div ref={containerRef} onScroll={onScroll} className="h-full overflow-y-auto px-6 py-4 font-mono text-xs">
      {events.map((event) => (
        <EventLine key={`${event.seq}-${event.id}`} event={event} />
      ))}
      <div ref={bottomRef} />
      {!pinned && (
        <button
          onClick={() => {
            setPinned(true)
            bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
          }}
          className="sticky bottom-2 left-1/2 -translate-x-1/2 rounded-full border border-border bg-surface-3 px-3 py-1 text-[11px] text-fg-muted shadow-lg"
        >
          Jump to latest
        </button>
      )}
    </div>
  )
}

function EventLine({ event }: { event: GenesisEvent }) {
  const isFile = event.type === 'file.written'
  const isPhase = event.type === 'phase.started'

  if (isPhase) {
    return (
      <div className="mt-4 mb-1.5 flex items-center gap-2 first:mt-0">
        <Activity className="h-3.5 w-3.5 text-accent" />
        <span className="text-[13px] font-semibold text-fg">{event.message}</span>
        <div className="h-px flex-1 bg-border" />
      </div>
    )
  }

  return (
    <div className={cn('flex gap-2.5 py-[3px] leading-relaxed', isFile && 'opacity-55')}>
      <span className="shrink-0 text-fg-subtle">{clockTime(event.created_at)}</span>
      {event.agent_name && event.agent_role !== 'system' ? (
        <span className="w-[52px] shrink-0 truncate text-accent">{event.agent_name}</span>
      ) : (
        <span className="w-[52px] shrink-0" />
      )}
      <span
        className={cn(
          'min-w-0 flex-1 break-words',
          event.level === 'error' && 'text-danger',
          event.level === 'warn' && 'text-warning',
          event.level === 'debug' && 'text-fg-subtle',
          event.level === 'info' && 'text-fg-muted',
          event.type === 'artifact.created' && 'text-fg',
        )}
      >
        {isFile && <span className="mr-1 text-success">+</span>}
        {event.message}
      </span>
    </div>
  )
}

function AgentBoard({ agents }: { agents: AgentStatus[] }) {
  const grouped = useMemo(() => {
    const map = new Map<string, AgentStatus[]>()
    for (const agent of agents) {
      const list = map.get(agent.profile.phase) ?? []
      list.push(agent)
      map.set(agent.profile.phase, list)
    }
    return [...map.entries()]
  }, [agents])

  return (
    <div className="h-full space-y-5 overflow-y-auto px-6 py-4">
      {grouped.map(([phase, members]) => (
        <section key={phase} className="space-y-2">
          <h3 className="text-[11px] font-medium uppercase tracking-wide text-fg-subtle">{phase}</h3>
          <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
            {members.map((agent) => (
              <Card key={agent.profile.role} className="p-3">
                <div className="flex items-start gap-2.5">
                  <span
                    className="mt-1 h-2 w-2 shrink-0 rounded-full"
                    style={{ backgroundColor: agent.profile.accent }}
                    aria-hidden
                  />
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <p className="truncate text-[13px] font-medium text-fg">{agent.profile.name}</p>
                      <Badge tone={toneForStatus(agent.status)}>{agent.status}</Badge>
                    </div>
                    <p className="mt-0.5 text-[11px] text-fg-subtle">{agent.profile.title}</p>
                    {agent.task && (
                      <p className="mt-1.5 line-clamp-2 text-[11px] leading-relaxed text-fg-muted">{agent.task}</p>
                    )}
                    <div className="mt-2 flex items-center gap-3 text-[10px] text-fg-subtle">
                      <span>{agent.profile.model_class}</span>
                      {agent.artifacts > 0 && <span>{agent.artifacts} output</span>}
                    </div>
                  </div>
                </div>
              </Card>
            ))}
          </div>
        </section>
      ))}
    </div>
  )
}

function ArtifactBrowser({
  artifacts,
  selected,
  onSelect,
}: {
  artifacts: Artifact[]
  selected: Artifact | null
  onSelect: (artifact: Artifact) => void
}) {
  if (artifacts.length === 0) {
    return (
      <div className="flex h-full items-center justify-center">
        <p className="text-xs text-fg-subtle">Artifacts appear as agents produce them.</p>
      </div>
    )
  }

  return (
    <div className="flex h-full overflow-hidden">
      <aside className="w-64 shrink-0 overflow-y-auto border-r border-border bg-surface-1 py-2">
        {artifacts.map((artifact) => (
          <button
            key={artifact.id}
            onClick={() => onSelect(artifact)}
            className={cn(
              'flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs transition-colors',
              selected?.id === artifact.id ? 'bg-accent/12 text-accent' : 'text-fg-muted hover:bg-surface-2 hover:text-fg',
            )}
          >
            <FileText className="h-3.5 w-3.5 shrink-0" />
            <span className="min-w-0 flex-1 truncate">{artifact.name}</span>
            <span className="shrink-0 text-[10px] text-fg-subtle">{formatBytes(artifact.size_bytes)}</span>
          </button>
        ))}
      </aside>
      <div className="min-w-0 flex-1 overflow-y-auto bg-bg px-6 py-4">
        {selected ? (
          <pre className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed text-fg-muted">
            {selected.body}
          </pre>
        ) : (
          <p className="text-xs text-fg-subtle">Select a document to read it.</p>
        )}
      </div>
    </div>
  )
}

/**
 * Turns the current phase into a sentence a non-technical user can act on.
 *
 * Each line says what is happening and, where the step is genuinely slow, that
 * the wait is expected. "Installing the website's building blocks — this is
 * the slowest step" is the difference between a user waiting and a user
 * force-quitting.
 */
function describeProgress(phases: { title: string; status: string }[]): string {
  const running = phases.find((phase) => phase.status === 'running')
  if (!running) return 'Starting…'

  const narration: Record<string, string> = {
    'Product Analysis': 'Working out what to build from your description.',
    'Design & Architecture': 'Designing the screens and the data model.',
    'Task Planning': 'Breaking the work into tasks.',
    'Code Generation': 'Writing the code — usually the longest step.',
    'Testing & Review': 'Compiling and running it to check it actually works. This takes a moment.',
    'Self Healing': 'Fixing anything that did not work first time.',
    'Packaging & Deployment': 'Adding the setup files and documentation.',
  }
  return narration[running.title] ?? running.title
}
