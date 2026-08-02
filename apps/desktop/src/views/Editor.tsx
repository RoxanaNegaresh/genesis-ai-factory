/**
 * The code editor.
 *
 * This view exists to satisfy invariant I2 — human sovereignty. A factory that
 * generates code the user cannot read or change is a black box, and a black box
 * that writes your production system is not something anyone should trust. Every
 * generated file is openable, editable and saveable here.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Editor, { type OnMount } from '@monaco-editor/react'
import {
  ChevronRight, ChevronDown, File as FileIcon, Folder, FolderOpen,
  History, Save, Search as SearchIcon, Undo2, GitBranch, Loader2,
  Download,
} from 'lucide-react'
import {
  api, ApiError,
  type Commit, type FileContent, type FileNode, type SearchHit, type VCSStatus,
} from '@/lib/api'
import { Badge, Button, EmptyState, Input } from '@/components/ui/primitives'
import { cn, formatBytes, timeAgo } from '@/lib/utils'

type Panel = 'files' | 'search' | 'history'

interface OpenTab {
  path: string
  file: FileContent
  /** Draft content, present only while it differs from what was loaded. */
  draft?: string
}

export default function CodeEditor({ projectId }: { projectId: string }) {
  const [tree, setTree] = useState<FileNode[]>([])
  const [tabs, setTabs] = useState<OpenTab[]>([])
  const [activePath, setActivePath] = useState<string | null>(null)
  const [panel, setPanel] = useState<Panel>('files')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)

  const [query, setQuery] = useState('')
  const [hits, setHits] = useState<SearchHit[]>([])
  const [searching, setSearching] = useState(false)

  const [commits, setCommits] = useState<Commit[]>([])
  const [vcs, setVcs] = useState<VCSStatus | null>(null)

  const active = tabs.find((tab) => tab.path === activePath) ?? null
  const dirty = useMemo(() => tabs.some((tab) => tab.draft !== undefined), [tabs])

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      setLoading(true)
      try {
        const [files, status] = await Promise.all([
          api.files(projectId),
          api.vcsStatus(projectId).catch(() => null),
        ])
        if (!cancelled) {
          setTree(files.data)
          setVcs(status)
          setError(null)
        }
      } catch (err) {
        if (!cancelled) setError(err instanceof ApiError ? err.body.message : 'Could not load the workspace')
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    void load()
    return () => {
      cancelled = true
    }
  }, [projectId])

  const openFile = useCallback(
    async (path: string) => {
      const existing = tabs.find((tab) => tab.path === path)
      if (existing) {
        setActivePath(path)
        return
      }
      try {
        const file = await api.readFile(projectId, path)
        setTabs((prev) => [...prev, { path, file }])
        setActivePath(path)
      } catch (err) {
        setError(err instanceof ApiError ? err.body.message : 'Could not open the file')
      }
    },
    [projectId, tabs],
  )

  const closeTab = useCallback(
    (path: string) => {
      setTabs((prev) => {
        const next = prev.filter((tab) => tab.path !== path)
        if (path === activePath) {
          setActivePath(next.length > 0 ? next[next.length - 1].path : null)
        }
        return next
      })
    },
    [activePath],
  )

  const save = useCallback(async () => {
    if (!active || active.draft === undefined || saving) return

    setSaving(true)
    try {
      const saved = await api.writeFile(projectId, active.path, active.draft, active.file.sha256)
      setTabs((prev) =>
        prev.map((tab) => (tab.path === active.path ? { path: tab.path, file: saved } : tab)),
      )
      setError(null)
      // A save changes the working tree, so the git indicator must follow.
      void api.vcsStatus(projectId).then(setVcs).catch(() => undefined)
    } catch (err) {
      // A 409 means an agent changed the file while it was open. Telling the
      // user plainly is far better than silently overwriting either version.
      setError(
        err instanceof ApiError && err.status === 409
          ? 'This file changed on disk while you were editing. Close and reopen it to see the current version.'
          : err instanceof ApiError
            ? err.body.message
            : 'Could not save the file',
      )
    } finally {
      setSaving(false)
    }
  }, [active, projectId, saving])

  // Cmd/Ctrl+S is muscle memory; not binding it makes the editor feel broken.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 's') {
        event.preventDefault()
        void save()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [save])

  // Warn before discarding unsaved work.
  useEffect(() => {
    if (!dirty) return
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  }, [dirty])

  useEffect(() => {
    const trimmed = query.trim()
    if (trimmed.length < 2) {
      setHits([])
      return
    }
    let cancelled = false
    const timer = window.setTimeout(async () => {
      setSearching(true)
      try {
        const result = await api.searchWorkspace(projectId, trimmed)
        if (!cancelled) setHits(result.data)
      } catch {
        if (!cancelled) setHits([])
      } finally {
        if (!cancelled) setSearching(false)
      }
    }, 250)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [query, projectId])

  useEffect(() => {
    if (panel !== 'history') return
    void api.history(projectId).then((result) => setCommits(result.data)).catch(() => setCommits([]))
  }, [panel, projectId])

  const rollback = useCallback(
    async (sha: string) => {
      try {
        await api.rollback(projectId, sha)
        // Everything on screen may now be stale.
        const files = await api.files(projectId)
        setTree(files.data)
        setTabs([])
        setActivePath(null)
        setVcs(await api.vcsStatus(projectId).catch(() => null))
      } catch (err) {
        setError(err instanceof ApiError ? err.body.message : 'Rollback failed')
      }
    },
    [projectId],
  )

  const editorRef = useRef<Parameters<OnMount>[0] | null>(null)

  if (loading) {
    return (
      <div className="flex h-full items-center justify-center">
        <Loader2 className="h-5 w-5 animate-spin text-accent" />
      </div>
    )
  }

  return (
    <div className="flex h-full overflow-hidden">
      <aside className="flex w-64 shrink-0 flex-col border-r border-border bg-surface-1">
        <nav className="flex shrink-0 border-b border-border" role="tablist">
          {([
            ['files', 'Files', <FileIcon key="f" className="h-3.5 w-3.5" />],
            ['search', 'Search', <SearchIcon key="s" className="h-3.5 w-3.5" />],
            ['history', 'History', <History key="h" className="h-3.5 w-3.5" />],
          ] as const).map(([key, label, icon]) => (
            <button
              key={key}
              role="tab"
              aria-selected={panel === key}
              title={label}
              onClick={() => setPanel(key)}
              className={cn(
                'flex flex-1 items-center justify-center gap-1.5 py-2 text-[11px] transition-colors',
                panel === key ? 'bg-surface-2 text-fg' : 'text-fg-subtle hover:text-fg-muted',
              )}
            >
              {icon}
              {label}
            </button>
          ))}
        </nav>

        <div className="min-h-0 flex-1 overflow-y-auto py-1">
          {panel === 'files' && (
            <FileTree nodes={tree} activePath={activePath} onOpen={openFile} />
          )}

          {panel === 'search' && (
            <div className="px-2">
              <Input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Search files…"
                className="mb-2"
              />
              {searching && <p className="px-1 text-[11px] text-fg-subtle">Searching…</p>}
              {!searching && query.trim().length >= 2 && hits.length === 0 && (
                <p className="px-1 text-[11px] text-fg-subtle">No matches.</p>
              )}
              {hits.map((hit, index) => (
                <button
                  key={`${hit.path}:${hit.line}:${index}`}
                  onClick={() => void openFile(hit.path)}
                  className="block w-full rounded px-1.5 py-1 text-left transition-colors hover:bg-surface-2"
                >
                  <span className="block truncate text-[11px] text-accent">
                    {hit.path}:{hit.line}
                  </span>
                  <span className="block truncate font-mono text-[10px] text-fg-muted">{hit.text}</span>
                </button>
              ))}
            </div>
          )}

          {panel === 'history' && (
            <div className="px-2">
              {commits.length === 0 && (
                <p className="px-1 text-[11px] text-fg-subtle">No history yet.</p>
              )}
              {commits.map((commit) => (
                <div key={commit.sha} className="group rounded px-1.5 py-1.5 hover:bg-surface-2">
                  <p className="truncate text-[11px] text-fg">{commit.subject}</p>
                  <div className="mt-0.5 flex items-center gap-2 text-[10px] text-fg-subtle">
                    <span className="font-mono">{commit.short}</span>
                    <span>{commit.author}</span>
                    <span>{timeAgo(commit.when)}</span>
                    {commit.added > 0 && <span className="text-success">+{commit.added}</span>}
                    {commit.removed > 0 && <span className="text-danger">−{commit.removed}</span>}
                  </div>
                  <button
                    onClick={() => void rollback(commit.sha)}
                    className="mt-1 hidden items-center gap-1 text-[10px] text-warning group-hover:inline-flex"
                    title="Restore the workspace to this commit"
                  >
                    <Undo2 className="h-3 w-3" />
                    Roll back to here
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>

        {vcs && (
          <div className="flex shrink-0 items-center gap-1.5 border-t border-border px-3 py-1.5 text-[10px] text-fg-subtle">
            <GitBranch className="h-3 w-3" />
            <span>{vcs.branch || 'main'}</span>
            {!vcs.clean && <Badge tone="warning">uncommitted</Badge>}
          </div>
        )}
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <div className="flex shrink-0 items-center border-b border-border bg-surface-1">
          <div className="flex min-w-0 flex-1 overflow-x-auto">
            {tabs.map((tab) => (
              <button
                key={tab.path}
                onClick={() => setActivePath(tab.path)}
                className={cn(
                  'group flex shrink-0 items-center gap-1.5 border-r border-border px-3 py-2 text-xs transition-colors',
                  tab.path === activePath ? 'bg-bg text-fg' : 'text-fg-subtle hover:text-fg-muted',
                )}
              >
                <span className="max-w-[160px] truncate">{tab.path.split('/').pop()}</span>
                {tab.draft !== undefined && (
                  <span className="h-1.5 w-1.5 rounded-full bg-warning" title="Unsaved changes" />
                )}
                <span
                  role="button"
                  tabIndex={-1}
                  onClick={(event) => {
                    event.stopPropagation()
                    closeTab(tab.path)
                  }}
                  className="ml-0.5 opacity-0 transition-opacity hover:text-danger group-hover:opacity-100"
                >
                  ×
                </span>
              </button>
            ))}
          </div>

          <div className="flex shrink-0 items-center gap-2 px-3">
            {/* Always reachable, whether or not a file is open. */}
            <Button
              size="sm"
              variant="ghost"
              onClick={() => void api.exportProject(projectId, 'project')}
              title="Download the whole project as a zip"
            >
              <Download className="h-3.5 w-3.5" />
              Download
            </Button>
          </div>

          {active && (
            <div className="flex shrink-0 items-center gap-2 px-3">
              <span className="text-[10px] text-fg-subtle">{formatBytes(active.file.size)}</span>
              <Button
                size="sm"
                variant={active.draft !== undefined ? 'primary' : 'ghost'}
                onClick={() => void save()}
                disabled={active.draft === undefined || active.file.read_only}
                loading={saving}
              >
                <Save className="h-3.5 w-3.5" />
                Save
              </Button>
            </div>
          )}
        </div>

        {error && (
          <div className="shrink-0 border-b border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger" role="alert">
            {error}
          </div>
        )}

        <div className="min-h-0 flex-1">
          {!active ? (
            <EmptyState
              icon={<FileIcon className="h-7 w-7" />}
              title="No file open"
              description="Choose a file from the tree. Everything the factory generated is yours to edit."
            />
          ) : active.file.binary ? (
            <EmptyState
              icon={<FileIcon className="h-7 w-7" />}
              title="This file cannot be edited here"
              description="It is binary or too large for the editor."
            />
          ) : (
            <Editor
              key={active.path}
              height="100%"
              theme="vs-dark"
              language={active.file.language}
              value={active.draft ?? active.file.content}
              onMount={(editor) => {
                editorRef.current = editor
              }}
              onChange={(value) => {
                setTabs((prev) =>
                  prev.map((tab) =>
                    tab.path !== active.path
                      ? tab
                      : {
                          ...tab,
                          // Reverting to the saved content clears the dirty
                          // marker, so undoing an edit does not leave a phantom
                          // unsaved indicator.
                          draft: value === tab.file.content ? undefined : (value ?? ''),
                        },
                  ),
                )
              }}
              options={{
                readOnly: active.file.read_only,
                fontSize: 13,
                fontFamily: 'JetBrains Mono, SF Mono, Menlo, monospace',
                minimap: { enabled: false },
                scrollBeyondLastLine: false,
                renderWhitespace: 'selection',
                tabSize: 2,
                automaticLayout: true,
                padding: { top: 12 },
              }}
            />
          )}
        </div>
      </div>
    </div>
  )
}

/** FileTree renders the workspace hierarchy. */
function FileTree({
  nodes,
  activePath,
  onOpen,
  depth = 0,
}: {
  nodes: FileNode[]
  activePath: string | null
  onOpen: (path: string) => void
  depth?: number
}) {
  // Top-level directories start open; deeper ones do not, or a large project
  // buries its own structure on first render.
  const [expanded, setExpanded] = useState<Record<string, boolean>>(() =>
    depth === 0 ? Object.fromEntries(nodes.filter((n) => n.dir).map((n) => [n.path, true])) : {},
  )

  return (
    <div>
      {nodes.map((node) => {
        const isOpen = expanded[node.path]
        return (
          <div key={node.path}>
            <button
              onClick={() =>
                node.dir
                  ? setExpanded((prev) => ({ ...prev, [node.path]: !prev[node.path] }))
                  : onOpen(node.path)
              }
              style={{ paddingLeft: 8 + depth * 12 }}
              className={cn(
                'flex w-full items-center gap-1 py-[3px] pr-2 text-left text-xs transition-colors',
                node.path === activePath
                  ? 'bg-accent/12 text-accent'
                  : 'text-fg-muted hover:bg-surface-2 hover:text-fg',
              )}
            >
              {node.dir ? (
                <>
                  {isOpen ? (
                    <ChevronDown className="h-3 w-3 shrink-0" />
                  ) : (
                    <ChevronRight className="h-3 w-3 shrink-0" />
                  )}
                  {isOpen ? (
                    <FolderOpen className="h-3.5 w-3.5 shrink-0 text-accent/70" />
                  ) : (
                    <Folder className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />
                  )}
                </>
              ) : (
                <>
                  <span className="w-3 shrink-0" />
                  <FileIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />
                </>
              )}
              <span className="truncate">{node.name}</span>
            </button>

            {node.dir && isOpen && node.children && node.children.length > 0 && (
              <FileTree
                nodes={node.children}
                activePath={activePath}
                onOpen={onOpen}
                depth={depth + 1}
              />
            )}
          </div>
        )
      })}
    </div>
  )
}
