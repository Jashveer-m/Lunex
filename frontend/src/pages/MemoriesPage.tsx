import { useState } from 'react'

import { IconBrain, IconPencil, IconTrash } from '../components/icons'
import { PageContainer } from '../components/Shell'
import {
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  ErrorBanner,
  IconButton,
  LoadMore,
  LoadingBlock,
  PageHeader,
  Select,
  Textarea,
  Toggle,
  cx,
} from '../components/ui'
import { errorMessage } from '../lib/api'
import { memories as memApi } from '../lib/endpoints'
import { humanize, pct, plural, relativeTime } from '../lib/format'
import { Link } from '../lib/router'
import { MEMORY_TYPES, type Memory, type MemoryType } from '../lib/types'
import { usePaged } from '../lib/usePaged'

const TYPE_HINT: Record<MemoryType, string> = {
  episodic: 'Something that happened',
  semantic: 'A standing fact',
  preference: 'How you like to work',
  project: 'Tied to a piece of work',
  goal: 'Tied to something you want to achieve',
}

export function MemoriesPage() {
  const [type, setType] = useState<MemoryType | ''>('')
  const [enabled, setEnabled] = useState<'' | 'true' | 'false'>('')
  const [sort, setSort] = useState('-created_at')
  const list = usePaged(
    (limit, offset) =>
      memApi
        .list({ limit, offset, type: type || undefined, enabled: enabled || undefined, sort })
        .then((p) => ({ items: p.memories, count: p.count })),
    `${type}|${enabled}|${sort}`,
  )
  const [deleting, setDeleting] = useState<Memory | null>(null)
  const [clearing, setClearing] = useState(false)
  const [cleared, setCleared] = useState<number | null>(null)

  const replace = (m: Memory) => list.setItems((items) => items.map((x) => (x.id === m.id ? m : x)))
  const filtered = type !== '' || enabled !== ''

  return (
    <PageContainer>
      <PageHeader
        title="Memories"
        description="What the assistant has learned about you from your conversations. Correct anything that is wrong, switch off what it should not use, or forget it entirely."
        actions={
          <Button variant="secondary" icon={<IconTrash size={14} />} onClick={() => setClearing(true)} className="text-danger">
            Forget everything
          </Button>
        }
      />

      <div className="mb-5 flex flex-wrap items-center gap-2">
        <Select value={type} onChange={(e) => setType(e.target.value as MemoryType | '')} className="h-8 w-40" aria-label="Filter by type">
          <option value="">All types</option>
          {MEMORY_TYPES.map((t) => (
            <option key={t} value={t}>
              {humanize(t)}
            </option>
          ))}
        </Select>
        <Select value={enabled} onChange={(e) => setEnabled(e.target.value as '' | 'true' | 'false')} className="h-8 w-48" aria-label="Filter by state">
          <option value="">Enabled & disabled</option>
          <option value="true">Enabled only</option>
          <option value="false">Disabled only</option>
        </Select>
        <Select value={sort} onChange={(e) => setSort(e.target.value)} className="ml-auto h-8 w-44" aria-label="Sort">
          <option value="-created_at">Newest first</option>
          <option value="created_at">Oldest first</option>
          <option value="-importance">Most important</option>
          <option value="-confidence">Most confident</option>
        </Select>
      </div>

      {cleared !== null && (
        <p className="mb-4 rounded-lg bg-ok-soft px-3.5 py-2.5 text-sm text-ok" role="status">
          Forgot {plural(cleared, 'memory', 'memories')}.
        </p>
      )}
      {list.error && <ErrorBanner className="mb-4" message={list.error} onRetry={list.reload} />}

      {list.loading && list.items.length === 0 ? (
        <LoadingBlock label="Loading memories…" />
      ) : !list.error && list.items.length === 0 ? (
        <EmptyState title={filtered ? 'No memories match' : 'Nothing remembered yet'} icon={<IconBrain size={22} />}>
          {filtered ? (
            'Try another filter.'
          ) : (
            <>
              Tell the assistant something durable about yourself — “I always study in the early morning” — and it will
              appear here. <Link to="/chat" className="text-accent hover:underline">Open the assistant →</Link>
            </>
          )}
        </EmptyState>
      ) : (
        <ul className={cx('space-y-2', list.loading && 'opacity-60')}>
          {list.items.map((m) => (
            <MemoryRow key={m.id} memory={m} onChange={replace} onDelete={() => setDeleting(m)} />
          ))}
        </ul>
      )}
      <LoadMore hasMore={list.hasMore} loading={list.loadingMore} onClick={list.loadMore} />

      <ConfirmDialog
        open={deleting !== null}
        onClose={() => setDeleting(null)}
        title="Delete this memory?"
        confirmLabel="Delete memory"
        onConfirm={async () => {
          if (!deleting) return
          await memApi.remove(deleting.id)
          list.setItems((items) => items.filter((x) => x.id !== deleting.id))
        }}
      >
        <p className="rounded-lg bg-sunken px-3 py-2 text-ink">“{deleting?.content}”</p>
        <p>It is gone for good — there is no archive. To stop the assistant using it without losing it, switch it off instead.</p>
      </ConfirmDialog>

      <ConfirmDialog
        open={clearing}
        onClose={() => setClearing(false)}
        title="Forget everything?"
        confirmLabel="Forget all memories"
        onConfirm={async () => {
          const res = await memApi.clearAll()
          setCleared(res.deleted)
          list.reload()
        }}
      >
        <p>
          This permanently deletes <strong className="text-ink">every memory</strong> the assistant has about you — enabled
          and disabled, of every type, not only the ones shown here. There is no undo.
        </p>
        <p>Your conversations, documents, tasks, goals and notes are not affected.</p>
      </ConfirmDialog>
    </PageContainer>
  )
}

function MemoryRow({ memory, onChange, onDelete }: { memory: Memory; onChange: (m: Memory) => void; onDelete: () => void }) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(memory.content)
  const [saving, setSaving] = useState(false)
  const [toggling, setToggling] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const save = async () => {
    const content = draft.trim()
    if (!content || content === memory.content) return setEditing(false)
    setSaving(true)
    setError(null)
    try {
      onChange(await memApi.update(memory.id, { content }))
      setEditing(false)
    } catch (e) {
      // 503 embedding_unavailable: changing the text re-embeds it, and nothing
      // is changed when that cannot happen. The message says so.
      setError(errorMessage(e))
    } finally {
      setSaving(false)
    }
  }

  const toggle = async (next: boolean) => {
    setToggling(true)
    setError(null)
    try {
      onChange(await memApi.update(memory.id, { enabled: next }))
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setToggling(false)
    }
  }

  return (
    <li className={cx('group rounded-xl border border-line bg-surface p-4 transition-opacity', !memory.enabled && 'bg-sunken/40')}>
      <div className="flex items-start gap-4">
        <div className="min-w-0 flex-1">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            <Badge tone={memory.enabled ? 'accent' : 'neutral'} className="capitalize">
              <span title={TYPE_HINT[memory.type]}>{memory.type}</span>
            </Badge>
            {!memory.enabled && <Badge>Disabled — never retrieved</Badge>}
          </div>
          {editing ? (
            <div className="space-y-2">
              <Textarea
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                rows={2}
                maxLength={1000}
                autoFocus
                aria-label="Memory content"
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    void save()
                  }
                  if (e.key === 'Escape') {
                    setDraft(memory.content)
                    setEditing(false)
                  }
                }}
              />
              <div className="flex items-center gap-2">
                <Button size="sm" variant="primary" loading={saving} onClick={() => void save()} disabled={!draft.trim()}>
                  Save
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    setDraft(memory.content)
                    setEditing(false)
                    setError(null)
                  }}
                >
                  Cancel
                </Button>
                <span className="text-xs text-ink-faint">Saving re-embeds it, so it is found by what it now says.</span>
              </div>
            </div>
          ) : (
            <p className={cx('text-sm leading-relaxed', memory.enabled ? 'text-ink' : 'text-ink-muted')}>{memory.content}</p>
          )}
          {error && <p className="mt-2 text-xs text-danger">{error}</p>}
          <p className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs text-ink-faint">
            <span title="How much the model thought this was worth keeping">Importance {pct(memory.importance)}</span>
            <span title="How sure the model was that it read this rather than inferred it">Confidence {pct(memory.confidence)}</span>
            <span>Learned {relativeTime(memory.created_at)}</span>
            {memory.source_conversation_id ? (
              <Link to={`/chat/${memory.source_conversation_id}`} className="text-accent hover:underline">
                From this conversation
              </Link>
            ) : (
              <span>Source conversation deleted</span>
            )}
          </p>
        </div>
        <div className="flex flex-col items-end gap-2">
          <Toggle checked={memory.enabled} onChange={(v) => void toggle(v)} disabled={toggling} label={memory.enabled ? 'Disable memory' : 'Enable memory'} />
          <div className="flex gap-0.5 opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
            {!editing && (
              <IconButton label="Edit memory" onClick={() => setEditing(true)}>
                <IconPencil size={14} />
              </IconButton>
            )}
            <IconButton label="Delete memory" tone="danger" onClick={onDelete}>
              <IconTrash size={14} />
            </IconButton>
          </div>
        </div>
      </div>
    </li>
  )
}
