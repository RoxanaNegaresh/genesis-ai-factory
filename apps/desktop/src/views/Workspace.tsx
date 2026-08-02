/**
 * AI Workspace — the primary surface. The user states an outcome; the system
 * shows what it understood, then what it is doing about it.
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { motion, AnimatePresence } from 'framer-motion'
import { Sparkles, ArrowRight, FolderGit2, Clock, Layers, Download } from 'lucide-react'
import { api, ApiError, type BlueprintSummary, type Classification, type Project } from '@/lib/api'
import { Badge, Button, Card, EmptyState, Textarea, toneForStatus } from '@/components/ui/primitives'
import { categoryLabel, timeAgo } from '@/lib/utils'

interface WorkspaceProps {
  projects: Project[]
  onProjectCreated: (projectId: string, runId?: string) => void
  onOpenProject: (projectId: string) => void
  refreshProjects: () => void
}

const EXAMPLES = [
  'Build a Jira competitor with kanban boards and sprints',
  'Create an ERP system for a manufacturing company',
  'Build an Airbnb-like marketplace with listings and payments',
  'Build a CRM for a sales team with a pipeline and reporting',
]

export default function Workspace({ projects, onProjectCreated, onOpenProject, refreshProjects }: WorkspaceProps) {
  const [prompt, setPrompt] = useState('')
  const [classification, setClassification] = useState<Classification | null>(null)
  const [blueprint, setBlueprint] = useState<BlueprintSummary | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  useEffect(() => {
    textareaRef.current?.focus()
  }, [])

  // Classify as the user types so the interpretation is visible *before* they
  // commit. Discovering a misread brief after a full build is the expensive
  // failure this preview exists to prevent.
  useEffect(() => {
    const trimmed = prompt.trim()
    if (trimmed.length < 12) {
      setClassification(null)
      setBlueprint(null)
      return
    }
    let cancelled = false
    const timer = window.setTimeout(async () => {
      try {
        const result = await api.classify(trimmed)
        if (!cancelled) {
          setClassification(result.classification)
          setBlueprint(result.blueprint)
        }
      } catch {
        // A failed preview must never block the primary action.
      }
    }, 350)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [prompt])

  const submit = useCallback(async () => {
    const trimmed = prompt.trim()
    if (!trimmed || submitting) return

    setSubmitting(true)
    setError(null)
    try {
      const result = await api.createProject(trimmed, true)
      setPrompt('')
      setClassification(null)
      setBlueprint(null)
      refreshProjects()
      onProjectCreated(result.project.id, result.run?.id)
    } catch (err) {
      setError(err instanceof ApiError ? err.body.message : 'Could not start the build')
    } finally {
      setSubmitting(false)
    }
  }, [prompt, submitting, onProjectCreated, refreshProjects])

  return (
    <div className="mx-auto flex h-full w-full max-w-4xl flex-col gap-8 overflow-y-auto px-8 py-10">
      <header className="space-y-1.5">
        <h1 className="text-xl font-semibold tracking-tight text-fg">What should we build?</h1>
        <p className="text-[13px] text-fg-muted">
          Describe the product in plain language. The factory analyses it, designs the architecture and
          generates a real, editable repository.
        </p>
      </header>

      <Card className="overflow-hidden p-0">
        <Textarea
          ref={textareaRef}
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          onKeyDown={(e) => {
            // Enter inserts a newline; the deliberate action needs a modifier.
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
              e.preventDefault()
              void submit()
            }
          }}
          rows={4}
          placeholder="Build a project management system like Jira, with boards, sprints and issue tracking…"
          className="rounded-none border-0 bg-transparent px-4 py-3.5 text-sm focus:ring-0"
        />

        <AnimatePresence>
          {classification && blueprint && (
            <motion.div
              initial={{ opacity: 0, height: 0 }}
              animate={{ opacity: 1, height: 'auto' }}
              exit={{ opacity: 0, height: 0 }}
              transition={{ duration: 0.18, ease: [0.16, 1, 0.3, 1] }}
              className="border-t border-border bg-surface-2/60 px-4 py-3"
            >
              <div className="flex flex-wrap items-center gap-2 text-xs">
                <Sparkles className="h-3.5 w-3.5 text-accent" />
                <span className="text-fg-muted">Recognised as</span>
                <Badge tone="accent">{blueprint.name}</Badge>
                <span className="text-fg-subtle">
                  {Math.round(classification.confidence * 100)}% confidence
                </span>
                <span className="text-fg-subtle">·</span>
                <span className="text-fg-subtle">
                  {blueprint.entities} entities · {blueprint.screens} screens
                </span>
              </div>
              {blueprint.epics.length > 0 && (
                <div className="mt-2 flex flex-wrap gap-1">
                  {blueprint.epics.slice(0, 6).map((epic) => (
                    <span
                      key={epic}
                      className="rounded bg-surface-3 px-1.5 py-0.5 text-[10px] text-fg-muted"
                    >
                      {epic}
                    </span>
                  ))}
                </div>
              )}
            </motion.div>
          )}
        </AnimatePresence>

        <div className="flex items-center justify-between border-t border-border bg-surface-2/40 px-4 py-2.5">
          <span className="text-[11px] text-fg-subtle">
            <kbd className="rounded border border-border bg-surface-3 px-1 py-px font-mono">⌘</kbd>
            <span className="mx-0.5">+</span>
            <kbd className="rounded border border-border bg-surface-3 px-1 py-px font-mono">↵</kbd>
            <span className="ml-1.5">to build</span>
          </span>
          <Button variant="primary" onClick={() => void submit()} loading={submitting} disabled={!prompt.trim()}>
            {submitting ? 'Starting…' : 'Build it'}
            {!submitting && <ArrowRight className="h-3.5 w-3.5" />}
          </Button>
        </div>
      </Card>

      {error && (
        <div className="rounded-md border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger" role="alert">
          {error}
        </div>
      )}

      {projects.length === 0 && (
        <section className="space-y-2">
          <p className="text-[11px] font-medium uppercase tracking-wide text-fg-subtle">Try one of these</p>
          <div className="grid gap-2 sm:grid-cols-2">
            {EXAMPLES.map((example) => (
              <button
                key={example}
                onClick={() => {
                  setPrompt(example)
                  textareaRef.current?.focus()
                }}
                className="rounded-md border border-border bg-surface-1 px-3 py-2.5 text-left text-xs text-fg-muted transition-colors hover:border-border-strong hover:bg-surface-2 hover:text-fg"
              >
                {example}
              </button>
            ))}
          </div>
        </section>
      )}

      <section className="space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="text-[11px] font-medium uppercase tracking-wide text-fg-subtle">Projects</h2>
          {projects.length > 0 && <span className="text-[11px] text-fg-subtle">{projects.length}</span>}
        </div>

        {projects.length === 0 ? (
          <EmptyState
            icon={<FolderGit2 className="h-7 w-7" />}
            title="No projects yet"
            description="Describe a product above and the factory will build it."
          />
        ) : (
          <div className="grid gap-2">
            {projects.map((project) => (
              <button
                key={project.id}
                onClick={() => onOpenProject(project.id)}
                className="group flex items-center gap-3 rounded-lg border border-border bg-surface-1 px-3.5 py-3 text-left transition-colors hover:border-border-strong hover:bg-surface-2"
              >
                <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded bg-surface-3 text-fg-muted">
                  <Layers className="h-4 w-4" />
                </div>
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <p className="truncate text-[13px] font-medium text-fg">{project.name}</p>
                    <Badge tone={toneForStatus(project.status)}>{project.status}</Badge>
                  </div>
                  <p className="mt-0.5 truncate text-[11px] text-fg-subtle">{project.prompt}</p>
                </div>
                <div className="flex shrink-0 items-center gap-3 text-[11px] text-fg-subtle">
                  <span>{categoryLabel(project.category)}</span>
                  <span className="flex items-center gap-1">
                    <Clock className="h-3 w-3" />
                    {timeAgo(project.created_at)}
                  </span>
                  {/* Download without opening the project first. A finished
                      product should be one click away from the list, not two
                      screens deep. Nested in the row's click target, so the
                      propagation stop is required. */}
                  {project.status === 'ready' && (
                    <span
                      role="button"
                      tabIndex={0}
                      title="Download this project as a zip"
                      onClick={(event) => {
                        event.stopPropagation()
                        void api.exportProject(project.id, project.slug)
                      }}
                      onKeyDown={(event) => {
                        if (event.key === 'Enter' || event.key === ' ') {
                          event.preventDefault()
                          event.stopPropagation()
                          void api.exportProject(project.id, project.slug)
                        }
                      }}
                      className="flex h-7 w-7 items-center justify-center rounded-md text-fg-subtle opacity-0 transition-all hover:bg-surface-3 hover:text-accent focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring group-hover:opacity-100"
                    >
                      <Download className="h-3.5 w-3.5" />
                    </span>
                  )}
                </div>
              </button>
            ))}
          </div>
        )}
      </section>
    </div>
  )
}
