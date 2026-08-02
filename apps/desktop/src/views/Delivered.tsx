import { useCallback, useEffect, useState } from 'react'
import {
  CheckCircle2,
  Code2,
  Database,
  Download,
  FileText,
  FolderOpen,
  Globe,
  Layers,
  Server,
} from 'lucide-react'
import { api, type Artifact, type Project } from '@/lib/api'
import { Button } from '@/components/ui/primitives'
import { tryInvoke, isDesktop } from '@/lib/shell'

/**
 * What the user sees the moment a build finishes.
 *
 * The previous flow ended on a log of agent activity with a small "Open code"
 * button. That is a developer's view of the event, not an answer to the
 * question the user actually has, which is "what did I get, and what do I do
 * with it?". Someone non-technical reads a wall of green ticks and has no idea
 * whether anything usable exists.
 *
 * This screen answers three things in order: what was built, where it is, and
 * how to run it. Downloading is the primary action because it is the one that
 * makes the result theirs.
 */
export default function Delivered({
  project,
  onOpenEditor,
  onBack,
}: {
  project: Project
  onOpenEditor: () => void
  onBack: () => void
}) {
  const [artifacts, setArtifacts] = useState<Artifact[]>([])
  const [fileCount, setFileCount] = useState<number | null>(null)
  const [downloading, setDownloading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const load = async () => {
      try {
        const [docs, files] = await Promise.all([
          api.projectArtifacts(project.id),
          api.files(project.id),
        ])
        setArtifacts(docs.data)

        // Count leaves, not nodes: "156 files" is meaningful, "12 folders and
        // some files" is not.
        const count = (nodes: typeof files.data): number =>
          nodes.reduce((n, node) => n + (node.children ? count(node.children) : 1), 0)
        setFileCount(count(files.data))
      } catch {
        // The summary is decoration; its absence must not hide the actions.
      }
    }
    void load()
  }, [project.id])

  const download = useCallback(async () => {
    setDownloading(true)
    setError(null)
    try {
      await api.exportProject(project.id, project.slug)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'The download failed.')
    } finally {
      setDownloading(false)
    }
  }, [project.id, project.slug])

  const openFolder = useCallback(() => {
    void tryInvoke('open_workspace', { path: project.workspace_path })
  }, [project.workspace_path])

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-3xl px-8 py-10">
        <div className="flex items-start gap-4">
          <div className="flex h-12 w-12 shrink-0 items-center justify-center rounded-2xl bg-success/12">
            <CheckCircle2 className="h-6 w-6 text-success" />
          </div>
          <div className="min-w-0 pt-0.5">
            <h1 className="text-xl font-semibold tracking-tight text-fg">
              {project.name} is ready
            </h1>
            <p className="mt-1 text-[13px] leading-relaxed text-fg-muted">
              A complete, working project — not a sketch. You can open the code,
              download it, or hand it to a developer as it is.
            </p>
          </div>
        </div>

        {/* What you got, in numbers a non-technical reader can use. */}
        <div className="mt-7 grid grid-cols-3 gap-3">
          <Stat icon={<Code2 className="h-4 w-4" />} value={fileCount ?? '—'} label="source files" />
          <Stat icon={<FileText className="h-4 w-4" />} value={artifacts.length} label="documents" />
          <Stat icon={<Layers className="h-4 w-4" />} value={project.category.toUpperCase()} label="blueprint" />
        </div>

        {/* Primary actions. Download first: it is what makes the result theirs. */}
        <div className="mt-7 rounded-xl border border-border bg-surface-1 p-5">
          <h2 className="text-[13px] font-semibold text-fg">Take it with you</h2>
          <p className="mt-1 text-xs leading-relaxed text-fg-muted">
            The download is a normal folder of code in a zip file. Nothing in it
            is locked to Genesis.
          </p>
          <div className="mt-4 flex flex-wrap gap-2">
            <Button variant="primary" size="lg" onClick={() => void download()} loading={downloading}>
              <Download className="h-4 w-4" />
              Download project
            </Button>
            <Button variant="secondary" size="lg" onClick={onOpenEditor}>
              <Code2 className="h-4 w-4" />
              View the code
            </Button>
            {isDesktop() && project.workspace_path && (
              <Button variant="ghost" size="lg" onClick={openFolder}>
                <FolderOpen className="h-4 w-4" />
                Open folder
              </Button>
            )}
          </div>
          {error && (
            <p className="mt-3 text-xs text-danger" role="alert">
              {error}
            </p>
          )}
        </div>

        {/* Running it. Written for someone who has never used a terminal. */}
        <div className="mt-4 rounded-xl border border-border bg-surface-1 p-5">
          <h2 className="text-[13px] font-semibold text-fg">Run it on your computer</h2>
          <p className="mt-1 text-xs leading-relaxed text-fg-muted">
            Your project has two halves. Both need to be started once.
          </p>

          <div className="mt-4 space-y-3">
            <RunStep
              icon={<Server className="h-3.5 w-3.5" />}
              title="The server"
              note="Handles data and business rules."
              commands={['cd api', 'go run ./cmd/server']}
            />
            <RunStep
              icon={<Globe className="h-3.5 w-3.5" />}
              title="The website"
              note="What people see in a browser."
              commands={['cd web', 'npm install', 'npm run dev']}
            />
            <RunStep
              icon={<Database className="h-3.5 w-3.5" />}
              title="The database"
              note="Run once, before the server."
              commands={['createdb app', 'psql -d app -f migrations/0001_init.up.sql']}
            />
          </div>

          <p className="mt-4 text-[11px] leading-relaxed text-fg-subtle">
            The full instructions are in <span className="font-medium text-fg-muted">README.md</span>{' '}
            inside the download. If you are handing this to a developer, that file
            is all they need.
          </p>
        </div>

        {/* Documents the agents produced, worth surfacing rather than burying. */}
        {artifacts.length > 0 && (
          <div className="mt-4 rounded-xl border border-border bg-surface-1 p-5">
            <h2 className="text-[13px] font-semibold text-fg">Documents written for you</h2>
            <p className="mt-1 text-xs text-fg-muted">
              Requirements, architecture and test plans — the paperwork a team
              would normally produce.
            </p>
            <div className="mt-3 flex flex-wrap gap-1.5">
              {artifacts.slice(0, 12).map((artifact) => (
                <span
                  key={artifact.id}
                  className="rounded-md bg-surface-2 px-2 py-1 text-[11px] text-fg-muted"
                  title={artifact.kind}
                >
                  {artifact.name}
                </span>
              ))}
            </div>
          </div>
        )}

        <button
          onClick={onBack}
          className="mt-6 text-xs text-fg-subtle underline-offset-2 transition-colors hover:text-fg-muted hover:underline"
        >
          ← Back to projects
        </button>
      </div>
    </div>
  )
}

function Stat({
  icon,
  value,
  label,
}: {
  icon: React.ReactNode
  value: React.ReactNode
  label: string
}) {
  return (
    <div className="rounded-xl border border-border bg-surface-1 px-4 py-3">
      <div className="flex items-center gap-1.5 text-fg-subtle">{icon}</div>
      <p className="mt-1.5 truncate text-lg font-semibold tabular-nums text-fg">{value}</p>
      <p className="text-[11px] text-fg-subtle">{label}</p>
    </div>
  )
}

/** One half of the application, with the exact commands to start it. */
function RunStep({
  icon,
  title,
  note,
  commands,
}: {
  icon: React.ReactNode
  title: string
  note: string
  commands: string[]
}) {
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(commands.join('\n'))
      setCopied(true)
      setTimeout(() => setCopied(false), 1600)
    } catch {
      // Clipboard access can be refused; the commands are visible regardless.
    }
  }

  return (
    <div className="rounded-lg border border-border bg-surface-2 p-3">
      <div className="flex items-center gap-2">
        <span className="text-accent">{icon}</span>
        <span className="text-xs font-medium text-fg">{title}</span>
        <span className="text-[11px] text-fg-subtle">{note}</span>
        <button
          onClick={() => void copy()}
          className="ml-auto rounded px-1.5 py-0.5 text-[10px] text-fg-subtle transition-colors hover:bg-surface-3 hover:text-fg"
        >
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre className="mt-2 overflow-x-auto rounded bg-bg px-2.5 py-1.5 font-mono text-[11px] leading-relaxed text-fg-muted">
        {commands.join('\n')}
      </pre>
    </div>
  )
}
