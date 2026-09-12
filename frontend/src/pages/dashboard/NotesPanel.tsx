import { useState, type FormEvent } from 'react'

import { IconNote, IconPencil, IconPlus, IconSearch, IconTrash, IconX } from '../../components/icons'
import {
  Button,
  ConfirmDialog,
  EmptyState,
  ErrorBanner,
  Field,
  IconButton,
  Input,
  LoadMore,
  LoadingBlock,
  Textarea,
  cx,
} from '../../components/ui'
import { ApiError, errorMessage } from '../../lib/api'
import { notes as notesApi } from '../../lib/endpoints'
import { parseTags, relativeTime } from '../../lib/format'
import type { Note, NoteInput } from '../../lib/types'
import { useDebounced } from '../../lib/useDebounced'
import { usePaged } from '../../lib/usePaged'

export function NotesPanel() {
  const [search, setSearch] = useState('')
  const [tag, setTag] = useState('')
  const q = useDebounced(search.trim())
  const list = usePaged(
    (limit, offset) =>
      notesApi
        .list({ limit, offset, q: q || undefined, tag: tag || undefined, sort: '-updated_at' })
        .then((p) => ({ items: p.notes, count: p.count })),
    `${q}|${tag}`,
  )
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<Note | null>(null)

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center gap-3">
        <div className="relative w-64">
          <IconSearch size={14} className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-ink-faint" />
          <Input value={search} onChange={(e) => setSearch(e.target.value)} placeholder="Search notes" className="h-8 pl-8" maxLength={200} aria-label="Search notes" />
        </div>
        {tag && (
          <button
            type="button"
            onClick={() => setTag('')}
            className="inline-flex items-center gap-1 rounded-md bg-accent-soft px-2 py-1 text-xs font-medium text-accent"
          >
            #{tag} <IconX size={12} />
          </button>
        )}
        <div className="ml-auto">
          {!creating && (
            <Button variant="primary" icon={<IconPlus size={15} />} onClick={() => setCreating(true)}>
              New note
            </Button>
          )}
        </div>
      </div>

      {creating && (
        <div className="rounded-xl border border-line bg-surface p-4 shadow-xs">
          <NoteForm
            submitLabel="Save note"
            onCancel={() => setCreating(false)}
            onSubmit={async (input) => {
              const note = await notesApi.create(input)
              list.setItems((items) => [note, ...items])
              setCreating(false)
            }}
          />
        </div>
      )}

      {list.error && <ErrorBanner message={list.error} onRetry={list.reload} />}

      {list.loading && list.items.length === 0 ? (
        <LoadingBlock label="Loading notes…" />
      ) : !list.error && list.items.length === 0 ? (
        <EmptyState title={q || tag ? 'No matching notes' : 'No notes yet'} icon={<IconNote size={22} />}>
          {q || tag ? 'Try a different search.' : 'Notes are searchable by the assistant — the three most recent are part of every answer’s context.'}
        </EmptyState>
      ) : (
        <div className={cx('grid gap-3 sm:grid-cols-2', list.loading && 'opacity-60')}>
          {list.items.map((note) =>
            editing === note.id ? (
              <div key={note.id} className="rounded-xl border border-accent/40 bg-surface p-4 sm:col-span-2">
                <NoteForm
                  note={note}
                  submitLabel="Save"
                  onCancel={() => setEditing(null)}
                  onSubmit={async (input) => {
                    const updated = await notesApi.update(note.id, input)
                    list.setItems((items) => items.map((n) => (n.id === updated.id ? updated : n)))
                    setEditing(null)
                  }}
                />
              </div>
            ) : (
              <article key={note.id} className="group flex flex-col rounded-xl border border-line bg-surface p-4">
                <div className="flex items-start gap-2">
                  <h3 className="min-w-0 flex-1 font-medium">{note.title}</h3>
                  <div className="-mt-1 -mr-1 flex gap-0.5 opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
                    <IconButton label="Edit note" onClick={() => setEditing(note.id)}>
                      <IconPencil size={14} />
                    </IconButton>
                    <IconButton label="Delete note" tone="danger" onClick={() => setDeleting(note)}>
                      <IconTrash size={14} />
                    </IconButton>
                  </div>
                </div>
                {note.content ? (
                  <p className="mt-1.5 line-clamp-4 text-sm whitespace-pre-line text-ink-muted">{note.content}</p>
                ) : (
                  <p className="mt-1.5 text-sm text-ink-faint italic">Empty</p>
                )}
                <div className="mt-auto flex flex-wrap items-center gap-1.5 pt-3 text-xs text-ink-faint">
                  {note.tags.map((t) => (
                    <button key={t} type="button" onClick={() => setTag(t)} className="text-accent hover:underline">
                      #{t}
                    </button>
                  ))}
                  <span className="ml-auto">Edited {relativeTime(note.updated_at)}</span>
                </div>
              </article>
            ),
          )}
        </div>
      )}
      <LoadMore hasMore={list.hasMore} loading={list.loadingMore} onClick={list.loadMore} />

      <ConfirmDialog
        open={deleting !== null}
        onClose={() => setDeleting(null)}
        title="Delete note?"
        confirmLabel="Delete note"
        onConfirm={async () => {
          if (!deleting) return
          await notesApi.remove(deleting.id)
          list.setItems((items) => items.filter((n) => n.id !== deleting.id))
        }}
      >
        <p>“{deleting?.title}” will be permanently deleted.</p>
      </ConfirmDialog>
    </div>
  )
}

function NoteForm({
  note,
  submitLabel,
  onSubmit,
  onCancel,
}: {
  note?: Note
  submitLabel: string
  onSubmit: (input: NoteInput) => Promise<void>
  onCancel: () => void
}) {
  const [title, setTitle] = useState(note?.title ?? '')
  const [content, setContent] = useState(note?.content ?? '')
  const [tags, setTags] = useState(note?.tags.join(', ') ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | string | null>(null)
  const fe = (f: string) => (error instanceof ApiError ? error.fieldError(f) : undefined)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await onSubmit({ title: title.trim(), content, tags: parseTags(tags) })
    } catch (err) {
      setError(err instanceof ApiError ? err : errorMessage(err))
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="space-y-3">
      <Field label="Title" error={fe('title')}>
        {(id) => <Input id={id} value={title} onChange={(e) => setTitle(e.target.value)} maxLength={500} autoFocus required />}
      </Field>
      <Field label="Content" error={fe('content')}>
        {(id) => <Textarea id={id} rows={6} value={content} onChange={(e) => setContent(e.target.value)} maxLength={40000} />}
      </Field>
      <Field label="Tags" hint="Comma-separated" error={fe('tags')}>
        {(id) => <Input id={id} value={tags} onChange={(e) => setTags(e.target.value)} placeholder="meeting, ideas" />}
      </Field>
      {error && <ErrorBanner message={typeof error === 'string' ? error : errorMessage(error)} />}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" loading={busy} disabled={!title.trim()}>
          {submitLabel}
        </Button>
      </div>
    </form>
  )
}
