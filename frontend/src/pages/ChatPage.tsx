import { useState } from 'react'
import { flushSync } from 'react-dom'

import { IconPlus, IconTrash } from '../components/icons'
import { Button, ConfirmDialog, ErrorBanner, IconButton, LoadMore, Spinner, cx } from '../components/ui'
import { conversations as convApi } from '../lib/endpoints'
import { relativeTime } from '../lib/format'
import { Link, navigate } from '../lib/router'
import type { Conversation } from '../lib/types'
import { usePaged } from '../lib/usePaged'
import { ConversationView } from './chat/ConversationView'

export function ChatPage({ conversationId }: { conversationId: string | null }) {
  const list = usePaged(
    (limit, offset) => convApi.list({ limit, offset }).then((p) => ({ items: p.conversations, count: p.count })),
    'conversations',
  )
  const [draftKey, setDraftKey] = useState(0)
  // A draft that has just created its conversation keeps its component (and
  // the stream it is reading) when the URL moves to /chat/:id.
  const [adopted, setAdopted] = useState<{ id: string; key: string } | null>(null)
  const [deleting, setDeleting] = useState<Conversation | null>(null)

  const viewKey =
    conversationId === null ? `draft-${draftKey}` : adopted?.id === conversationId ? adopted.key : conversationId

  const newChat = () => {
    setDraftKey((k) => k + 1)
    navigate('/chat')
  }

  return (
    <div className="flex h-full">
      <aside className="flex w-72 shrink-0 flex-col border-r border-line">
        <div className="flex items-center justify-between gap-2 px-4 pt-6 pb-3">
          <h1 className="font-display text-2xl tracking-tight">Assistant</h1>
          <Button size="sm" variant="secondary" icon={<IconPlus size={13} />} onClick={newChat}>
            New chat
          </Button>
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
          {list.error && <ErrorBanner className="mx-2 my-2" message={list.error} onRetry={list.reload} />}
          {list.loading && list.items.length === 0 ? (
            <div className="flex items-center gap-2 px-3 py-6 text-sm text-ink-faint" role="status">
              <Spinner size={14} /> Loading conversations…
            </div>
          ) : !list.error && list.items.length === 0 ? (
            <p className="px-3 py-6 text-sm text-ink-faint">No conversations yet. Ask something to start one.</p>
          ) : (
            <ul className="space-y-0.5">
              {list.items.map((c) => {
                const active = c.id === conversationId
                return (
                  <li key={c.id} className="group relative">
                    <Link
                      to={`/chat/${c.id}`}
                      aria-current={active ? 'page' : undefined}
                      className={cx(
                        'block rounded-lg py-2 pr-9 pl-3 transition-colors',
                        active ? 'bg-surface shadow-xs ring-1 ring-line' : 'hover:bg-sunken',
                      )}
                    >
                      <p className={cx('truncate text-sm', active ? 'font-medium text-ink' : 'text-ink-muted')}>{c.title}</p>
                      <p className="text-xs text-ink-faint">
                        {relativeTime(c.updated_at)} · {c.message_count} message{c.message_count === 1 ? '' : 's'}
                      </p>
                    </Link>
                    <IconButton
                      label="Delete conversation"
                      tone="danger"
                      onClick={() => setDeleting(c)}
                      className="absolute top-1/2 right-1.5 -translate-y-1/2 opacity-0 group-focus-within:opacity-100 group-hover:opacity-100"
                    >
                      <IconTrash size={13} />
                    </IconButton>
                  </li>
                )
              })}
            </ul>
          )}
          <LoadMore hasMore={list.hasMore} loading={list.loadingMore} onClick={list.loadMore} />
        </div>
      </aside>

      <section className="min-w-0 flex-1">
        <ConversationView
          key={viewKey}
          conversationId={conversationId}
          onCreated={(conv) => {
            flushSync(() => setAdopted({ id: conv.id, key: viewKey }))
            list.setItems((items) => [conv, ...items])
            navigate(`/chat/${conv.id}`)
          }}
          onTurnFinished={list.reload}
        />
      </section>

      <ConfirmDialog
        open={deleting !== null}
        onClose={() => setDeleting(null)}
        title="Delete conversation?"
        confirmLabel="Delete conversation"
        onConfirm={async () => {
          if (!deleting) return
          await convApi.remove(deleting.id)
          list.setItems((items) => items.filter((c) => c.id !== deleting.id))
          if (deleting.id === conversationId) newChat()
        }}
      >
        <p>
          “{deleting?.title}” and its messages will be deleted. Memories and connections learned from it are kept, and any
          proposals it made can still be decided on the Approvals page.
        </p>
      </ConfirmDialog>
    </div>
  )
}
