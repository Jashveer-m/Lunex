// One conversation: its history, the turn being streamed, and the composer.
//
// A turn's life, as the backend streams it (docs/api.md):
//   waiting    POST sent; routing + retrieval run before anything is written
//   streaming  `sources` arrived, then `token`s, rendered as they come
//   (tail)     after the last token the extractors run; `action` frames arrive
//              here and can be approved immediately, before `done`
//   done       the persisted user + assistant messages replace the optimistic ones
//   error      an `error` frame, a pre-stream 4xx/5xx, or Stop. The backend does
//              not persist an unfinished turn, so the question is offered back.
import {
  forwardRef,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from 'react'

import { ActionCard } from '../../components/ActionCard'
import { IconBrain, IconGraph, IconSend, IconStop } from '../../components/icons'
import { Mark } from '../../components/Shell'
import { Button, ErrorBanner, LoadingBlock, cx } from '../../components/ui'
import { ApiError, errorMessage, isAbort } from '../../lib/api'
import { actions as actionsApi, conversations as convApi } from '../../lib/endpoints'
import { announceActionsChanged } from '../../lib/events'
import { plural } from '../../lib/format'
import { Link } from '../../lib/router'
import { streamMessage } from '../../lib/sse'
import type { Action, Conversation, ConversationDetail, DoneFrame, Message, Source } from '../../lib/types'
import { AnswerText, SourceList, citedLabels } from './Sources'

const MAX_CONTENT = 8000

type Pending = {
  content: string
  phase: 'waiting' | 'streaming' | 'error'
  sources: Source[] | null
  text: string
  actionIds: string[]
  error?: string
  /** Set while we check with the server whether an interrupted turn was saved. */
  reconciling?: boolean
  startedAt: number
  lastTokenAt: number | null
}

type TurnNotes = Pick<DoneFrame, 'remembered' | 'linked'>

export function ConversationView({
  conversationId,
  onCreated,
  onTurnFinished,
}: {
  conversationId: string | null
  onCreated: (conversation: Conversation) => void
  onTurnFinished: () => void
}) {
  const [detail, setDetail] = useState<ConversationDetail | null>(null)
  const [loadState, setLoadState] = useState<{ status: 'loading' | 'ready' } | { status: 'error'; message: string; notFound: boolean }>(
    conversationId ? { status: 'loading' } : { status: 'ready' },
  )
  const [actions, setActions] = useState<Action[]>([])
  const [pending, setPending] = useState<Pending | null>(null)
  const [notes, setNotes] = useState<Record<string, TurnNotes>>({})
  const [input, setInput] = useState('')
  const [reloadCount, setReloadCount] = useState(0)

  const createdHere = useRef<string | null>(null)
  const abortRef = useRef<AbortController | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  const stickToBottom = useRef(true)
  const composerRef = useRef<HTMLTextAreaElement>(null)

  // --- load ----------------------------------------------------------------

  const fetchConversation = useCallback(async (id: string, signal?: AbortSignal) => {
    const [conv, acts] = await Promise.all([
      convApi.get(id, signal),
      actionsApi.list({ conversation_id: id, limit: 200 }, signal),
    ])
    return { conv, acts: acts.actions }
  }, [])

  useEffect(() => {
    // A conversation this view created mid-send is already on screen.
    if (!conversationId || conversationId === createdHere.current) return
    const ctrl = new AbortController()
    setLoadState({ status: 'loading' })
    fetchConversation(conversationId, ctrl.signal)
      .then(({ conv, acts }) => {
        setDetail(conv)
        setActions(acts)
        setLoadState({ status: 'ready' })
        stickToBottom.current = true
      })
      .catch((e: unknown) => {
        if (isAbort(e)) return
        setLoadState({
          status: 'error',
          message: errorMessage(e),
          notFound: e instanceof ApiError && e.status === 404,
        })
      })
    return () => ctrl.abort()
  }, [conversationId, fetchConversation, reloadCount])

  // --- scrolling -----------------------------------------------------------

  const onScroll = () => {
    const el = scrollRef.current
    if (el) stickToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
  }
  useLayoutEffect(() => {
    const el = scrollRef.current
    if (el && stickToBottom.current) el.scrollTop = el.scrollHeight
  }, [detail?.messages.length, pending?.text, pending?.phase, pending?.sources, actions.length, loadState.status])

  // --- send ----------------------------------------------------------------

  const upsertAction = (a: Action) =>
    setActions((list) => (list.some((x) => x.id === a.id) ? list.map((x) => (x.id === a.id ? a : x)) : [...list, a]))

  /** After an interrupted turn, ask the server what was actually saved. */
  const reconcile = async (id: string, countBefore: number) => {
    setPending((p) => p && { ...p, reconciling: true })
    try {
      const { conv, acts } = await fetchConversation(id)
      setDetail(conv)
      setActions(acts)
      if (conv.message_count > countBefore) {
        setPending(null) // it was saved after all; the history now shows it
        onTurnFinished()
        return
      }
    } catch {
      // keep the error on screen; the user can retry
    }
    setPending((p) => p && { ...p, reconciling: false })
  }

  const send = async (content: string) => {
    const text = content.trim()
    if (!text || text.length > MAX_CONTENT || (pending && pending.phase !== 'error')) return
    setInput('')
    stickToBottom.current = true
    setPending({ content: text, phase: 'waiting', sources: null, text: '', actionIds: [], startedAt: Date.now(), lastTokenAt: null })

    let id = conversationId
    const countBefore = detail?.message_count ?? 0
    try {
      if (!id) {
        const conv = await convApi.create()
        id = conv.id
        createdHere.current = conv.id
        setDetail({ ...conv, messages: [] })
        onCreated(conv)
      }
      const ctrl = new AbortController()
      abortRef.current = ctrl
      await streamMessage(
        id,
        text,
        {
          onSources: (sources) => setPending((p) => p && { ...p, sources, phase: 'streaming' }),
          onToken: (t) => setPending((p) => p && { ...p, text: p.text + t, phase: 'streaming', lastTokenAt: Date.now() }),
          onAction: (action) => {
            upsertAction(action)
            setPending((p) => p && { ...p, actionIds: [...p.actionIds, action.id] })
            announceActionsChanged()
          },
          onDone: (done) => {
            setDetail((d) =>
              d
                ? { ...d, messages: [...d.messages, done.user_message, done.message], message_count: d.message_count + 2 }
                : d,
            )
            // An action may already have been approved while the extractors
            // ran; keep the newer local copy.
            setActions((list) => [...list, ...done.actions.filter((a) => !list.some((x) => x.id === a.id))])
            if (done.remembered.length || done.linked.length) {
              setNotes((n) => ({ ...n, [done.message.id]: { remembered: done.remembered, linked: done.linked } }))
            }
            setPending(null)
            onTurnFinished()
            // The first message names an untitled conversation server-side.
            if (countBefore === 0 && id) {
              convApi.get(id).then((c) => setDetail((d) => (d ? { ...d, title: c.title } : d)), () => undefined)
            }
          },
          onStreamError: (err) => {
            setPending((p) => p && { ...p, phase: 'error', error: err.message })
            if (err.error === 'stream_closed' && id) void reconcile(id, countBefore)
          },
        },
        ctrl.signal,
      )
    } catch (e) {
      if (isAbort(e)) {
        setPending((p) => p && { ...p, phase: 'error', error: 'Stopped.' })
        if (id) void reconcile(id, countBefore)
      } else {
        setPending((p) => p && { ...p, phase: 'error', error: errorMessage(e) })
      }
    } finally {
      abortRef.current = null
    }
  }

  const stop = () => abortRef.current?.abort()

  const editPending = () => {
    if (!pending) return
    setInput(pending.content)
    setPending(null)
    composerRef.current?.focus()
  }

  // --- render --------------------------------------------------------------

  if (loadState.status === 'loading') return <LoadingBlock label="Loading conversation…" />
  if (loadState.status === 'error') {
    return (
      <div className="mx-auto max-w-md px-6 py-20">
        <ErrorBanner
          message={loadState.notFound ? 'This conversation does not exist, or it was deleted.' : loadState.message}
          onRetry={loadState.notFound ? undefined : () => setReloadCount((n) => n + 1)}
        />
        {loadState.notFound && (
          <Link to="/chat" className="mt-4 inline-block text-sm text-accent hover:underline">
            Start a new conversation →
          </Link>
        )}
      </div>
    )
  }

  const messages = detail?.messages ?? []
  const grouped = groupActions(messages, actions, new Set(pending?.actionIds ?? []))
  const busy = pending !== null && pending.phase !== 'error'

  return (
    <div className="flex h-full flex-col">
      <header className="flex h-16 shrink-0 items-center border-b border-line px-8">
        <div className="min-w-0">
          <h2 className="truncate text-sm font-medium">{detail?.title ?? 'New conversation'}</h2>
          {detail && <p className="text-xs text-ink-faint">{plural(detail.message_count, 'message')}</p>}
        </div>
      </header>

      <div ref={scrollRef} onScroll={onScroll} className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto w-full max-w-3xl px-8 py-8">
          {messages.length === 0 && !pending ? (
            <Welcome onPick={(s) => setInput(s)} />
          ) : (
            <div className="space-y-8">
              {messages.map((m) =>
                m.role === 'user' ? (
                  <UserBubble key={m.id} content={m.content} />
                ) : (
                  <AssistantMessage key={m.id} messageKey={m.id} text={m.content} sources={m.sources}>
                    <ActionList actions={grouped.get(m.id) ?? []} onChange={upsertAction} />
                    {notes[m.id] && <TurnNotesView notes={notes[m.id]} />}
                  </AssistantMessage>
                ),
              )}

              {pending && (
                <>
                  <UserBubble content={pending.content} faded={pending.phase === 'error'} />
                  {pending.phase === 'error' ? (
                    <TurnError pending={pending} onRetry={() => void send(pending.content)} onEdit={editPending} />
                  ) : (
                    <AssistantMessage
                      messageKey="pending"
                      text={pending.text}
                      sources={(pending.sources ?? []).map((s) => ({ ...s, cited: citedLabels(pending.text).has(s.label) }))}
                      streaming
                    >
                      {pending.text === '' && <StreamStatus pending={pending} />}
                      <ActionList actions={actions.filter((a) => pending.actionIds.includes(a.id))} onChange={upsertAction} />
                      {pending.text !== '' && <StreamStatus pending={pending} />}
                    </AssistantMessage>
                  )}
                </>
              )}
            </div>
          )}
        </div>
      </div>

      <Composer
        ref={composerRef}
        value={input}
        onChange={setInput}
        onSend={() => void send(input)}
        onStop={stop}
        busy={busy}
      />
    </div>
  )
}

/**
 * Put each action under the answer that produced it. Actions are written in the
 * same transaction as the turn's messages, just after them, so an action
 * belongs to the last assistant message created at or before it.
 */
function groupActions(messages: Message[], actions: Action[], pendingIds: Set<string>): Map<string, Action[]> {
  const out = new Map<string, Action[]>()
  const assistant = messages.filter((m) => m.role === 'assistant')
  const sorted = [...actions].sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at))
  for (const action of sorted) {
    if (pendingIds.has(action.id)) continue
    const t = Date.parse(action.created_at)
    let owner: Message | undefined
    for (const m of assistant) {
      if (Date.parse(m.created_at) <= t) owner = m
      else break
    }
    owner ??= assistant[assistant.length - 1]
    if (!owner) continue
    out.set(owner.id, [...(out.get(owner.id) ?? []), action])
  }
  return out
}

// --- pieces ----------------------------------------------------------------

function UserBubble({ content, faded }: { content: string; faded?: boolean }) {
  return (
    <div className="flex justify-end">
      <div
        className={cx(
          'max-w-[85%] rounded-2xl rounded-br-md bg-sunken px-4 py-2.5 text-sm leading-relaxed whitespace-pre-wrap text-ink',
          faded && 'opacity-60',
        )}
      >
        {content}
      </div>
    </div>
  )
}

function AssistantMessage({
  messageKey,
  text,
  sources,
  streaming,
  children,
}: {
  messageKey: string
  text: string
  sources: Source[]
  streaming?: boolean
  children?: ReactNode
}) {
  return (
    <div className="flex gap-3.5">
      <div className="mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-full border border-line bg-surface">
        <Mark className="size-3.5" />
      </div>
      <div className="min-w-0 flex-1">
        {text && (
          <div className={cx('text-[15px] leading-relaxed whitespace-pre-wrap text-ink', streaming && 'caret')}>
            <AnswerText text={text} sources={sources} messageKey={messageKey} />
          </div>
        )}
        {children}
        <SourceList sources={sources} messageKey={messageKey} streaming={streaming} />
      </div>
    </div>
  )
}

function ActionList({ actions, onChange }: { actions: Action[]; onChange: (a: Action) => void }) {
  if (actions.length === 0) return null
  return (
    <div className="mt-4 space-y-2">
      {actions.map((a) => (
        <ActionCard key={a.id} action={a} onChange={onChange} />
      ))}
    </div>
  )
}

/** The current time, refreshed twice a second while mounted. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 500)
    return () => clearInterval(t)
  }, [])
  return now
}

function StreamStatus({ pending }: { pending: Pending }) {
  const now = useNow()
  const seconds = Math.max(0, Math.floor((now - pending.startedAt) / 1000))
  let label: string | null = null
  if (pending.phase === 'waiting') {
    label = seconds < 4 ? 'Thinking…' : `Reading your documents, memories and tasks… ${seconds}s`
  } else if (pending.text === '') {
    label = 'Writing…'
  } else if (pending.lastTokenAt && now - pending.lastTokenAt > 1500) {
    // The answer is complete; the memory and relationship extractors run
    // before the stream's final frame.
    label = 'Answer complete · updating memory and connections…'
  }
  if (!label) return null
  return (
    <p className="mt-2 flex items-center gap-2 text-xs text-ink-faint" role="status">
      <span className="flex gap-0.5" aria-hidden="true">
        {[0, 1, 2].map((i) => (
          <span key={i} className="size-1 animate-pulse rounded-full bg-ink-faint" style={{ animationDelay: `${i * 150}ms` }} />
        ))}
      </span>
      {label}
    </p>
  )
}

function TurnError({ pending, onRetry, onEdit }: { pending: Pending; onRetry: () => void; onEdit: () => void }) {
  return (
    <div className="ml-10 rounded-xl border border-danger/25 bg-danger-soft px-4 py-3 text-sm">
      <p className="font-medium text-danger">{pending.error}</p>
      {pending.text && (
        <p className="mt-2 line-clamp-3 text-xs whitespace-pre-wrap text-ink-muted italic">Partial answer: {pending.text}</p>
      )}
      <p className="mt-1 text-xs text-ink-muted">
        {pending.reconciling
          ? 'Checking whether the turn was saved…'
          : 'This turn was not saved — neither the question nor any partial answer. Send it again to retry.'}
      </p>
      <div className="mt-3 flex gap-2">
        <Button size="sm" variant="primary" onClick={onRetry} disabled={pending.reconciling}>
          Retry
        </Button>
        <Button size="sm" variant="ghost" onClick={onEdit} disabled={pending.reconciling}>
          Edit message
        </Button>
      </div>
    </div>
  )
}

function TurnNotesView({ notes }: { notes: TurnNotes }) {
  return (
    <div className="mt-3 space-y-1.5 text-xs text-ink-muted">
      {notes.remembered.map((m) => (
        <p key={m.id} className="flex items-start gap-1.5">
          <IconBrain size={13} className="mt-px shrink-0 text-accent" />
          <span>
            Remembered: “{m.content}”{' '}
            <Link to="/memories" className="text-accent hover:underline">
              Manage
            </Link>
          </span>
        </p>
      ))}
      {notes.linked.length > 0 && (
        <p className="flex items-center gap-1.5">
          <IconGraph size={13} className="shrink-0 text-accent" />
          Added {plural(notes.linked.length, 'connection')} to your graph ({notes.linked.map((l) => l.relationship).join(', ')}).{' '}
          <Link to="/graph" className="text-accent hover:underline">
            View
          </Link>
        </p>
      )}
    </div>
  )
}

const SUGGESTIONS = [
  'What do my documents say about…',
  'What should I focus on this week?',
  'Add a task to renew my passport by next Friday, it’s urgent.',
  'Which of my tasks mention the report?',
]

function Welcome({ onPick }: { onPick: (s: string) => void }) {
  return (
    <div className="flex flex-col items-center pt-16 text-center">
      <Mark className="size-8" />
      <h2 className="mt-5 font-display text-3xl tracking-tight">What’s on your mind?</h2>
      <p className="mt-2 max-w-md text-sm text-ink-muted">
        Answers are grounded in your documents, memories, tasks, goals and notes, with numbered citations. Ask for a change
        and you’ll get a proposal to approve — nothing is written without you.
      </p>
      <div className="mt-8 grid w-full max-w-lg gap-2 sm:grid-cols-2">
        {SUGGESTIONS.map((s) => (
          <button
            key={s}
            type="button"
            onClick={() => onPick(s)}
            className="rounded-xl border border-line bg-surface px-3.5 py-3 text-left text-sm text-ink-muted transition-colors hover:border-line-strong hover:text-ink"
          >
            {s}
          </button>
        ))}
      </div>
    </div>
  )
}

// --- composer --------------------------------------------------------------

const Composer = forwardRef<
  HTMLTextAreaElement,
  { value: string; onChange: (v: string) => void; onSend: () => void; onStop: () => void; busy: boolean }
>(function Composer({ value, onChange, onSend, onStop, busy }, ref) {
  const localRef = useRef<HTMLTextAreaElement | null>(null)
  const setRefs = (el: HTMLTextAreaElement | null) => {
    localRef.current = el
    if (typeof ref === 'function') ref(el)
    else if (ref) ref.current = el
  }
  // Grow with the text, up to a cap.
  useLayoutEffect(() => {
    const el = localRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`
  }, [value])

  const over = value.length > MAX_CONTENT
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      if (!busy) onSend()
    }
  }

  return (
    <div className="shrink-0 px-8 pb-6">
      <div className="mx-auto max-w-3xl">
        <div className="flex items-end gap-2 rounded-2xl border border-line-strong bg-surface p-2 shadow-sm focus-within:border-accent focus-within:ring-3 focus-within:ring-accent/15">
          <textarea
            ref={setRefs}
            value={value}
            onChange={(e) => onChange(e.target.value)}
            onKeyDown={onKeyDown}
            rows={1}
            placeholder={busy ? 'Waiting for the answer…' : 'Ask anything, or ask for a change…'}
            aria-label="Message"
            className="max-h-[200px] min-h-9 flex-1 resize-none bg-transparent px-2 py-2 text-sm text-ink placeholder:text-ink-faint focus:outline-none"
          />
          {busy ? (
            <Button key="stop" variant="secondary" onClick={onStop} icon={<IconStop size={14} />} aria-label="Stop generating">
              Stop
            </Button>
          ) : (
            <Button key="send" variant="primary" onClick={onSend} disabled={!value.trim() || over} icon={<IconSend size={15} />} aria-label="Send">
              Send
            </Button>
          )}
        </div>
        <p className={cx('mt-1.5 px-2 text-[11px]', over ? 'text-danger' : 'text-ink-faint')}>
          {over
            ? `${value.length.toLocaleString()} / ${MAX_CONTENT.toLocaleString()} characters — too long to send.`
            : 'Enter to send · Shift+Enter for a new line'}
        </p>
      </div>
    </div>
  )
})
