// The knowledge graph as two plain lists. Deliberately no graph renderer.
import { useCallback, useEffect, useMemo, useState } from 'react'

import { IconGraph, IconTrash } from '../components/icons'
import { PageContainer } from '../components/Shell'
import { Badge, ConfirmDialog, EmptyState, ErrorBanner, IconButton, LoadingBlock, PageHeader, Select, cx } from '../components/ui'
import { errorMessage } from '../lib/api'
import { graph as graphApi } from '../lib/endpoints'
import { humanize, pct, plural } from '../lib/format'
import { Link } from '../lib/router'
import type { Graph, GraphEdge, GraphNode } from '../lib/types'

const NODE_TYPES = ['task', 'goal', 'note', 'document', 'skill', 'person', 'project'] as const

export function GraphPage() {
  const [type, setType] = useState('')
  const [data, setData] = useState<Graph | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [deleting, setDeleting] = useState<{ kind: 'node'; node: GraphNode } | { kind: 'edge'; edge: GraphEdge } | null>(null)

  const load = useCallback(() => {
    setLoading(true)
    setError(null)
    graphApi
      .get({ type: type || undefined })
      .then(setData)
      .catch((e) => setError(errorMessage(e)))
      .finally(() => setLoading(false))
  }, [type])
  useEffect(load, [load])

  const nodes = useMemo(() => new Map((data?.nodes ?? []).map((n) => [n.id, n])), [data])
  const label = (id: string) => nodes.get(id)?.label ?? 'unknown'

  return (
    <PageContainer>
      <PageHeader
        title="Connections"
        description="Your tasks, goals, notes and documents, the skills, people and projects your conversations named, and how they relate. A question that names one of these gets its connections as context."
        actions={
          <Select value={type} onChange={(e) => setType(e.target.value)} className="h-8 w-40" aria-label="Filter nodes by type">
            <option value="">All types</option>
            {NODE_TYPES.map((t) => (
              <option key={t} value={t}>
                {humanize(t)}
              </option>
            ))}
          </Select>
        }
      />

      {error && <ErrorBanner className="mb-4" message={error} onRetry={load} />}
      {loading && !data ? (
        <LoadingBlock label="Loading connections…" />
      ) : data && data.nodes.length === 0 ? (
        <EmptyState title="No connections yet" icon={<IconGraph size={22} />}>
          Create a task or upload a document and it appears here. Tell the assistant how things relate — “I’m learning Rust
          for the compiler project” — and the link does too.
        </EmptyState>
      ) : data ? (
        <div className={cx('grid gap-8 lg:grid-cols-[1fr_1.3fr]', loading && 'opacity-60')}>
          <section>
            <h2 className="mb-3 text-xs font-medium tracking-wide text-ink-faint uppercase">{plural(data.node_count, 'node')}</h2>
            <ul className="divide-y divide-line rounded-xl border border-line bg-surface">
              {data.nodes.map((n) => (
                <li key={n.id} className="group flex items-center gap-2 px-3.5 py-2">
                  <Badge tone={n.extracted ? 'accent' : 'neutral'}>{n.type}</Badge>
                  <span className="min-w-0 flex-1 truncate text-sm">{n.label}</span>
                  {n.extracted ? (
                    <IconButton
                      label="Delete node"
                      tone="danger"
                      onClick={() => setDeleting({ kind: 'node', node: n })}
                      className="opacity-0 group-focus-within:opacity-100 group-hover:opacity-100"
                    >
                      <IconTrash size={13} />
                    </IconButton>
                  ) : (
                    <span className="text-[11px] text-ink-faint" title="Mirrors a record; delete the record to remove it">
                      mirrored
                    </span>
                  )}
                </li>
              ))}
            </ul>
          </section>

          <section>
            <h2 className="mb-3 text-xs font-medium tracking-wide text-ink-faint uppercase">{plural(data.edge_count, 'relationship')}</h2>
            {data.edges.length === 0 ? (
              <p className="rounded-xl border border-dashed border-line-strong px-4 py-8 text-center text-sm text-ink-faint">
                {type ? 'No relationships among nodes of this type.' : 'No relationships yet.'}
              </p>
            ) : (
              <ul className="divide-y divide-line rounded-xl border border-line bg-surface">
                {data.edges.map((e) => (
                  <li key={e.id} className="group flex items-center gap-2 px-3.5 py-2.5 text-sm">
                    <span className="min-w-0 flex-1">
                      <span className="font-medium">{label(e.from_node_id)}</span>
                      <span className="mx-2 font-mono text-[11px] text-accent">{e.relationship}</span>
                      <span className="font-medium">{label(e.to_node_id)}</span>
                      <span className="ml-2 text-xs text-ink-faint">{pct(e.confidence)}</span>
                      {e.source_conversation_id && (
                        <Link to={`/chat/${e.source_conversation_id}`} className="ml-2 text-xs text-accent hover:underline">
                          source
                        </Link>
                      )}
                    </span>
                    <IconButton
                      label="Delete relationship"
                      tone="danger"
                      onClick={() => setDeleting({ kind: 'edge', edge: e })}
                      className="opacity-0 group-focus-within:opacity-100 group-hover:opacity-100"
                    >
                      <IconTrash size={13} />
                    </IconButton>
                  </li>
                ))}
              </ul>
            )}
          </section>
        </div>
      ) : null}

      <ConfirmDialog
        open={deleting !== null}
        onClose={() => setDeleting(null)}
        title={deleting?.kind === 'node' ? 'Delete node?' : 'Delete relationship?'}
        confirmLabel="Delete"
        onConfirm={async () => {
          if (!deleting) return
          if (deleting.kind === 'node') await graphApi.removeNode(deleting.node.id)
          else await graphApi.removeEdge(deleting.edge.id)
          load()
        }}
      >
        {deleting?.kind === 'node' ? (
          <p>“{deleting.node.label}” and all of its relationships will be deleted.</p>
        ) : deleting?.kind === 'edge' ? (
          <p>
            “{label(deleting.edge.from_node_id)} {deleting.edge.relationship} {label(deleting.edge.to_node_id)}” will be deleted.
            Both nodes stay. A later conversation that states it again will recreate it.
          </p>
        ) : null}
      </ConfirmDialog>
    </PageContainer>
  )
}
