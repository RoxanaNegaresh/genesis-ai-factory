package factory

import (
	"context"
	"fmt"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// FrontendAgent generates the React + TypeScript client against the design
// system and the API contract.
type FrontendAgent struct{}

func (a *FrontendAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleFrontend)
	return Charter{
		Role: domain.RoleFrontend, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactDesignSystem, domain.ArtifactArchSpec},
		Outputs: []domain.ArtifactKind{domain.ArtifactCodeFrontend},
		Tools:   []string{"fs.write", "fs.read", "exec.run"}, ModelClass: "code",
		Budget: DefaultBudget(), Temperature: 0.2,
	}
}

func (a *FrontendAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	if !bb.Has(domain.ArtifactDesignSystem) {
		return nil, fmt.Errorf("frontend engineer requires the design system")
	}
	bp := bb.Blueprint

	files := map[string]string{
		"web/package.json":                 frontendPackageJSON(bb.Project.Slug),
		"web/tsconfig.json":                frontendTSConfig(),
		"web/vite.config.ts":               frontendViteConfig(),
		"web/index.html":                   frontendIndexHTML(bb.Project.Name),
		"web/src/main.tsx":                 frontendMain(),
		"web/src/App.tsx":                  frontendApp(bp),
		"web/src/lib/api.ts":               frontendAPIClient(),
		"web/src/lib/types.ts":             frontendTypes(bp),
		"web/src/lib/format.ts":            frontendFormat(),
		"web/src/components/DataTable.tsx": frontendDataTable(),
		"web/src/components/Layout.tsx":    frontendLayout(bp),
		"web/src/styles.css":               frontendStyles(),
	}
	for _, s := range bp.Screens {
		files["web/src/pages/"+pascal(s.Name)+".tsx"] = frontendPage(s, bp)
	}

	for path, content := range files {
		if err := tb.WriteFile(ctx, path, content); err != nil {
			return nil, err
		}
	}

	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Generated %d TypeScript files covering %d screens", len(files), len(bp.Screens)),
		map[string]any{"files": len(files), "screens": len(bp.Screens)})

	var sb strings.Builder
	sb.WriteString("# Frontend Generation Manifest\n\n")
	fmt.Fprintf(&sb, "**Stack:** React 18, TypeScript, Vite, TailwindCSS  \n**Files:** %d  \n**Screens:** %d\n\n",
		len(files), len(bp.Screens))
	sb.WriteString("## Routes\n\n| Route | Component | Data |\n|---|---|---|\n")
	for _, s := range bp.Screens {
		fmt.Fprintf(&sb, "| `%s` | `%s` | %s |\n", s.Route, pascal(s.Name), s.PrimaryData)
	}
	sb.WriteString("\n## Shared modules\n\n")
	sb.WriteString("- `lib/api.ts` — typed fetch client with token refresh and one error shape\n")
	sb.WriteString("- `lib/types.ts` — types mirroring the API schema\n")
	sb.WriteString("- `components/DataTable.tsx` — sortable, paginated table with explicit empty and error states\n")
	sb.WriteString("- `components/Layout.tsx` — shell with navigation derived from the screen map\n")

	body := sb.String()
	return []*domain.Artifact{
		artifact(bb, domain.ArtifactCodeFrontend, "frontend-manifest.md", "text/markdown", body),
	}, nil
}

func pascal(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '/'
	})
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

func camel(s string) string {
	p := pascal(s)
	if p == "" {
		return p
	}
	return strings.ToLower(p[:1]) + p[1:]
}

func frontendPackageJSON(slug string) string {
	return fmt.Sprintf(`{
  "name": "%s-web",
  "private": true,
  "version": "0.1.0",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "preview": "vite preview",
    "typecheck": "tsc --noEmit",
    "test": "vitest run"
  },
  "dependencies": {
    "react": "^18.3.1",
    "react-dom": "^18.3.1",
    "react-router-dom": "^6.26.2"
  },
  "devDependencies": {
    "@types/react": "^18.3.11",
    "@types/react-dom": "^18.3.0",
    "@vitejs/plugin-react": "^4.3.2",
    "autoprefixer": "^10.4.20",
    "postcss": "^8.4.47",
    "tailwindcss": "^3.4.13",
    "typescript": "^5.6.2",
    "vite": "^5.4.8",
    "vitest": "^2.1.2"
  }
}
`, slug)
}

func frontendTSConfig() string {
	return `{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "module": "ESNext",
    "moduleResolution": "bundler",
    "jsx": "react-jsx",
    "strict": true,
    "noUncheckedIndexedAccess": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "noFallthroughCasesInSwitch": true,
    "skipLibCheck": true,
    "isolatedModules": true,
    "resolveJsonModule": true,
    "allowImportingTsExtensions": false,
    "noEmit": true,
    "baseUrl": ".",
    "paths": { "@/*": ["./src/*"] }
  },
  "include": ["src"]
}
`
}

func frontendViteConfig() string {
	return `import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'node:path'

export default defineConfig({
  plugins: [react()],
  resolve: { alias: { '@': path.resolve(__dirname, './src') } },
  server: {
    port: 5173,
    // Proxy in development so the browser sees one origin and cookies behave
    // the same locally as they do behind a reverse proxy in production.
    proxy: { '/api': { target: 'http://localhost:8080', changeOrigin: true } },
  },
})
`
}

func frontendIndexHTML(title string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en" class="dark">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>%s</title>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" src="/src/main.tsx"></script>
  </body>
</html>
`, title)
}

func frontendMain() string {
	return `import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import App from './App'
import './styles.css'

const root = document.getElementById('root')
if (!root) throw new Error('root element missing')

ReactDOM.createRoot(root).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>,
)
`
}

func frontendApp(bp Blueprint) string {
	var imports strings.Builder
	var routes strings.Builder
	for _, s := range bp.Screens {
		name := pascal(s.Name)
		fmt.Fprintf(&imports, "import %s from './pages/%s'\n", name, name)
		fmt.Fprintf(&routes, "        <Route path=%q element={<%s />} />\n", s.Route, name)
	}
	return fmt.Sprintf(`import { Routes, Route } from 'react-router-dom'
import Layout from './components/Layout'
%s
export default function App() {
  return (
    <Layout>
      <Routes>
%s      </Routes>
    </Layout>
  )
}
`, imports.String(), routes.String())
}

func frontendAPIClient() string {
	return `/**
 * Typed API client.
 *
 * Two decisions worth noting:
 *  - The access token is held in memory, not localStorage. A token in
 *    localStorage is readable by any injected script; an in-memory token dies
 *    with the tab and is restored via the httpOnly refresh cookie.
 *  - A 401 triggers exactly one refresh attempt, and concurrent requests share
 *    that single attempt rather than stampeding the refresh endpoint.
 */

export interface ApiError {
  code: string
  message: string
  request_id?: string
  fields?: Record<string, string>
}

export class ApiException extends Error {
  readonly status: number
  readonly error: ApiError

  constructor(status: number, error: ApiError) {
    super(error.message)
    this.name = 'ApiException'
    this.status = status
    this.error = error
  }
}

let accessToken: string | null = null
let refreshInFlight: Promise<boolean> | null = null

export function setAccessToken(token: string | null): void {
  accessToken = token
}

async function refreshSession(): Promise<boolean> {
  if (!refreshInFlight) {
    refreshInFlight = fetch('/api/v1/auth/refresh', {
      method: 'POST',
      credentials: 'include',
    })
      .then(async (res) => {
        if (!res.ok) return false
        const body = (await res.json()) as { access_token?: string }
        if (!body.access_token) return false
        accessToken = body.access_token
        return true
      })
      .catch(() => false)
      .finally(() => {
        refreshInFlight = null
      })
  }
  return refreshInFlight
}

async function request<T>(path: string, init: RequestInit = {}, retry = true): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Content-Type', 'application/json')
  if (accessToken) headers.set('Authorization', ` + "`Bearer ${accessToken}`" + `)

  const res = await fetch(` + "`/api/v1${path}`" + `, { ...init, headers, credentials: 'include' })

  if (res.status === 401 && retry && (await refreshSession())) {
    return request<T>(path, init, false)
  }

  if (res.status === 204) return undefined as T

  const text = await res.text()
  const body = text ? JSON.parse(text) : {}

  if (!res.ok) {
    const err: ApiError = body?.error ?? { code: 'unknown_error', message: res.statusText }
    throw new ApiException(res.status, err)
  }
  return body as T
}

export interface Page<T> {
  data: T[]
  next_cursor: string | null
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body: unknown) =>
    request<T>(path, { method: 'POST', body: JSON.stringify(body) }),
  patch: <T>(path: string, body: unknown) =>
    request<T>(path, { method: 'PATCH', body: JSON.stringify(body) }),
  delete: (path: string) => request<void>(path, { method: 'DELETE' }),

  list: <T>(resource: string, params: Record<string, string | number | undefined> = {}) => {
    const query = new URLSearchParams()
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== '') query.set(key, String(value))
    }
    const qs = query.toString()
    return request<Page<T>>(` + "`/${resource}${qs ? `?${qs}` : ''}`" + `)
  },
}
`
}

func frontendTypes(bp Blueprint) string {
	var sb strings.Builder
	sb.WriteString("// Types mirroring the API schema. Generated from the product blueprint;\n")
	sb.WriteString("// v0.2 regenerates these directly from openapi.yaml so drift is impossible.\n\n")
	for _, e := range bp.Entities {
		fmt.Fprintf(&sb, "/** %s */\nexport interface %s {\n", e.Description, e.Name)
		for _, f := range e.Fields {
			if f.Name == "password_hash" {
				continue
			}
			optional := ""
			if !f.Required && !isSystemField(f.Name) {
				optional = "?"
			}
			fmt.Fprintf(&sb, "  %s%s: %s\n", f.Name, optional, tsType(f))
		}
		sb.WriteString("  deleted_at?: string | null\n}\n\n")
	}
	return sb.String()
}

func tsType(f Field) string {
	switch f.Type {
	case "int":
		return "number"
	case "decimal":
		return "string" // preserve precision; never parse money into a JS number
	case "bool":
		return "boolean"
	case "json":
		return "Record<string, unknown>"
	case "enum":
		quoted := make([]string, len(f.Enum))
		for i, v := range f.Enum {
			quoted[i] = "'" + v + "'"
		}
		return strings.Join(quoted, " | ")
	}
	return "string"
}

func frontendFormat() string {
	return `/** Presentation helpers shared across screens. */

const dateFormatter = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: '2-digit',
})

export function formatDate(value: string | null | undefined): string {
  if (!value) return '—'
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? '—' : dateFormatter.format(parsed)
}

/**
 * Money arrives as a decimal string so precision survives the wire. Formatting
 * parses it once, at the edge, purely for display.
 */
export function formatMoney(value: string | null | undefined, currency = 'USD'): string {
  if (!value) return '—'
  const amount = Number.parseFloat(value)
  if (Number.isNaN(amount)) return '—'
  return new Intl.NumberFormat(undefined, { style: 'currency', currency }).format(amount)
}

export function relativeTime(value: string | null | undefined): string {
  if (!value) return '—'
  const then = new Date(value).getTime()
  if (Number.isNaN(then)) return '—'
  const seconds = Math.round((then - Date.now()) / 1000)
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ['year', 31536000], ['month', 2592000], ['day', 86400],
    ['hour', 3600], ['minute', 60], ['second', 1],
  ]
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })
  for (const [unit, size] of units) {
    if (Math.abs(seconds) >= size || unit === 'second') {
      return rtf.format(Math.round(seconds / size), unit)
    }
  }
  return '—'
}

export function titleCase(value: string): string {
  return value.replace(/[_-]/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase())
}
`
}

func frontendDataTable() string {
	return `import { useMemo, useState } from 'react'

export interface Column<T> {
  key: keyof T & string
  header: string
  render?: (row: T) => React.ReactNode
  sortable?: boolean
  width?: string
}

interface DataTableProps<T> {
  rows: T[]
  columns: Column<T>[]
  loading?: boolean
  error?: string | null
  emptyMessage?: string
  onRowClick?: (row: T) => void
  rowKey: (row: T) => string
}

/**
 * A table that treats loading, error and empty as first-class states rather
 * than as an afterthought — those three cases are most of what a user actually
 * experiences when something is wrong.
 */
export default function DataTable<T>({
  rows,
  columns,
  loading = false,
  error = null,
  emptyMessage = 'Nothing here yet.',
  onRowClick,
  rowKey,
}: DataTableProps<T>) {
  const [sortKey, setSortKey] = useState<string | null>(null)
  const [sortAsc, setSortAsc] = useState(true)

  const sorted = useMemo(() => {
    if (!sortKey) return rows
    const copy = [...rows]
    copy.sort((a, b) => {
      const av = String((a as Record<string, unknown>)[sortKey] ?? '')
      const bv = String((b as Record<string, unknown>)[sortKey] ?? '')
      return sortAsc ? av.localeCompare(bv) : bv.localeCompare(av)
    })
    return copy
  }, [rows, sortKey, sortAsc])

  function toggleSort(key: string) {
    if (sortKey === key) setSortAsc(!sortAsc)
    else {
      setSortKey(key)
      setSortAsc(true)
    }
  }

  if (loading) {
    return (
      <div className="space-y-2 p-4" role="status" aria-busy="true">
        {Array.from({ length: 5 }).map((_, i) => (
          <div key={i} className="h-9 animate-pulse rounded bg-surface-alt" />
        ))}
      </div>
    )
  }

  if (error) {
    return (
      <div className="rounded border border-danger/40 bg-danger/10 p-4 text-sm text-danger" role="alert">
        {error}
      </div>
    )
  }

  if (sorted.length === 0) {
    return <div className="rounded border border-border p-8 text-center text-sm text-muted">{emptyMessage}</div>
  }

  return (
    <div className="overflow-x-auto rounded border border-border">
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr className="border-b border-border bg-surface-alt text-left">
            {columns.map((col) => (
              <th
                key={col.key}
                style={col.width ? { width: col.width } : undefined}
                className="px-3 py-2 font-medium text-muted"
                aria-sort={sortKey === col.key ? (sortAsc ? 'ascending' : 'descending') : 'none'}
              >
                {col.sortable ? (
                  <button type="button" className="hover:text-text" onClick={() => toggleSort(col.key)}>
                    {col.header}
                    {sortKey === col.key ? (sortAsc ? ' ↑' : ' ↓') : ''}
                  </button>
                ) : (
                  col.header
                )}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sorted.map((row) => (
            <tr
              key={rowKey(row)}
              onClick={onRowClick ? () => onRowClick(row) : undefined}
              className={
                'border-b border-border last:border-0 ' +
                (onRowClick ? 'cursor-pointer hover:bg-surface-alt' : '')
              }
            >
              {columns.map((col) => (
                <td key={col.key} className="px-3 py-2">
                  {col.render ? col.render(row) : String((row as Record<string, unknown>)[col.key] ?? '—')}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
`
}

func frontendLayout(bp Blueprint) string {
	var nav strings.Builder
	for _, s := range bp.Screens {
		// Only top-level routes belong in the sidebar; detail routes are
		// reached from their list.
		if strings.Contains(s.Route, ":") {
			continue
		}
		fmt.Fprintf(&nav, "  { label: %q, to: %q },\n", s.Name, s.Route)
	}
	return fmt.Sprintf(`import { NavLink } from 'react-router-dom'
import type { ReactNode } from 'react'

const navigation = [
%s]

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <div className="flex h-screen bg-bg text-text">
      <aside className="hidden w-56 shrink-0 border-r border-border bg-surface lg:block">
        <div className="px-4 py-4 text-sm font-semibold tracking-tight">%s</div>
        <nav className="space-y-0.5 px-2">
          {navigation.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === '/'}
              className={({ isActive }) =>
                'block rounded px-3 py-1.5 text-sm transition-colors ' +
                (isActive ? 'bg-primary/12 text-primary' : 'text-muted hover:bg-surface-alt hover:text-text')
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
      </aside>
      <main className="flex-1 overflow-auto">
        <div className="mx-auto max-w-7xl px-6 py-6">{children}</div>
      </main>
    </div>
  )
}
`, nav.String(), bp.Name)
}

func frontendPage(s Screen, bp Blueprint) string {
	name := pascal(s.Name)
	entity, ok := findEntity(bp, s.PrimaryData)
	if !ok {
		return fmt.Sprintf(`export default function %s() {
  return (
    <section>
      <h1 className="text-lg font-semibold">%s</h1>
      <p className="mt-1 text-sm text-muted">%s</p>
    </section>
  )
}
`, name, s.Name, s.Purpose)
	}

	// Choose up to four informative columns, skipping ids and system fields so
	// the default table is immediately useful.
	var columns []string
	for _, f := range entity.Fields {
		if isSystemField(f.Name) || f.Type == "ref" || f.Name == "password_hash" {
			continue
		}
		columns = append(columns, fmt.Sprintf(
			"  { key: %q, header: %q, sortable: true },", f.Name, titleize(f.Name)))
		if len(columns) >= 4 {
			break
		}
	}
	columns = append(columns, fmt.Sprintf(
		"  { key: 'created_at', header: 'Created', render: (row: %s) => formatDate(row.created_at) },", entity.Name))

	return fmt.Sprintf(`import { useEffect, useState } from 'react'
import { api, ApiException } from '@/lib/api'
import type { %s } from '@/lib/types'
import DataTable, { type Column } from '@/components/DataTable'
import { formatDate } from '@/lib/format'

const columns: Column<%s>[] = [
%s
]

/** %s */
export default function %s() {
  const [rows, setRows] = useState<%s[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [query, setQuery] = useState('')

  useEffect(() => {
    let cancelled = false
    const timer = setTimeout(async () => {
      setLoading(true)
      try {
        const page = await api.list<%s>('%s', { q: query, limit: 50 })
        if (!cancelled) {
          setRows(page.data)
          setError(null)
        }
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof ApiException ? err.error.message : 'Failed to load data')
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    }, query ? 200 : 0)

    // Debounce search and abandon stale responses so fast typing cannot render
    // results from an earlier query over a later one.
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [query])

  return (
    <section>
      <header className="mb-4 flex items-center justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">%s</h1>
          <p className="text-sm text-muted">%s</p>
        </div>
        <div className="flex items-center gap-2">
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search…"
            aria-label="Search %s"
            className="h-8 w-56 rounded border border-border bg-surface px-2 text-sm outline-none focus:border-primary"
          />
          <button
            type="button"
            className="h-8 rounded bg-primary px-3 text-sm font-medium text-white hover:opacity-90"
          >
            New
          </button>
        </div>
      </header>

      <DataTable
        rows={rows}
        columns={columns}
        loading={loading}
        error={error}
        rowKey={(row) => row.id}
        emptyMessage={query ? 'No matches for this search.' : 'No records yet — create the first one.'}
      />
    </section>
  )
}
`, entity.Name, entity.Name, strings.Join(columns, "\n"), s.Purpose, name, entity.Name,
		entity.Name, toSnake(entity.Plural), s.Name, s.Purpose, strings.ToLower(s.Name))
}

func titleize(field string) string {
	parts := strings.Split(field, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func frontendStyles() string {
	return `@tailwind base;
@tailwind components;
@tailwind utilities;

/* Tokens are declared as CSS variables so the same values drive Tailwind
   utilities and any raw CSS, and so theme switching costs nothing at runtime. */
:root {
  --bg: #ffffff;
  --surface: #f7f8fa;
  --surface-alt: #eef0f4;
  --border: #e2e5ea;
  --text: #0b0d10;
  --muted: #5b6472;
  --primary: #4f46e5;
  --danger: #d93a3a;
}

.dark {
  --bg: #0b0d10;
  --surface: #14171c;
  --surface-alt: #1b1f26;
  --border: #262b33;
  --text: #e8eaed;
  --muted: #98a1af;
  --primary: #7c74f5;
  --danger: #f2645f;
}

@layer base {
  html, body, #root { height: 100%; }
  body {
    background: var(--bg);
    color: var(--text);
    font-family: Inter, -apple-system, 'Segoe UI', sans-serif;
    font-size: 13px;
    -webkit-font-smoothing: antialiased;
  }
  :focus-visible {
    outline: 2px solid var(--primary);
    outline-offset: 2px;
  }
}

@layer utilities {
  .bg-bg { background-color: var(--bg); }
  .bg-surface { background-color: var(--surface); }
  .bg-surface-alt { background-color: var(--surface-alt); }
  .border-border { border-color: var(--border); }
  .text-text { color: var(--text); }
  .text-muted { color: var(--muted); }
  .text-primary { color: var(--primary); }
  .bg-primary { background-color: var(--primary); }
  .text-danger { color: var(--danger); }
  .border-danger\/40 { border-color: color-mix(in srgb, var(--danger) 40%, transparent); }
  .bg-danger\/10 { background-color: color-mix(in srgb, var(--danger) 10%, transparent); }
  .bg-primary\/12 { background-color: color-mix(in srgb, var(--primary) 12%, transparent); }
}
`
}
