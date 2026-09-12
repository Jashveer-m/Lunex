// A proposed write (or a search the assistant already ran), with the only
// buttons in the app that can make a proposal happen.
//
// The card is the source of truth for whether something was done. The model's
// prose can be wrong about that (docs/api.md, known gap 39); the status here
// comes from the actions API and nothing else.
import { useState } from 'react'

import { ApiError, errorMessage } from '../lib/api'
import { actions as actionsApi } from '../lib/endpoints'
import { announceActionsChanged } from '../lib/events'
import { formatDate, humanize, relativeTime } from '../lib/format'
import { Link } from '../lib/router'
import type { Action } from '../lib/types'
import { IconCheck, IconSearch, IconShield, IconX } from './icons'
import { Button, Spinner, cx } from './ui'

const TOOL_LABELS: Record<string, string> = {
  create_task: 'Create task',
  update_task: 'Update task',
  create_goal: 'Create goal',
  create_note: 'Create note',
  search_tasks: 'Searched tasks',
  search_goals: 'Searched goals',
  search_notes: 'Searched notes',
  search_documents: 'Searched documents',
}

const RESULT_LINKS: Record<string, { to: string; label: string }> = {
  create_task: { to: '/', label: 'View in tasks' },
  update_task: { to: '/', label: 'View in tasks' },
  create_goal: { to: '/?tab=goals', label: 'View in goals' },
  create_note: { to: '/?tab=notes', label: 'View in notes' },
}

export function ActionCard({
  action,
  onChange,
  showContext,
}: {
  action: Action
  onChange: (action: Action) => void
  /** Show when it was proposed and which conversation it came from. */
  showContext?: boolean
}) {
  const [busy, setBusy] = useState<'approve' | 'reject' | null>(null)
  const [error, setError] = useState<string | null>(null)

  if (action.permission_level === 'read') return <ReadAction action={action} />

  const decide = async (kind: 'approve' | 'reject') => {
    setBusy(kind)
    setError(null)
    try {
      const updated = kind === 'approve' ? await actionsApi.approve(action.id) : await actionsApi.reject(action.id)
      onChange(updated)
    } catch (e) {
      setError(errorMessage(e))
      // 409: decided elsewhere (another tab, the Approvals page). Show the
      // state it is really in rather than leaving stale buttons up.
      if (e instanceof ApiError && e.code === 'action_not_pending') {
        actionsApi.get(action.id).then(onChange, () => undefined)
      }
    } finally {
      setBusy(null)
      announceActionsChanged()
    }
  }

  const tone = {
    proposed: 'border-warn/40 bg-warn-soft/60',
    approved: 'border-accent/30 bg-accent-soft/50',
    executed: 'border-ok/30 bg-ok-soft/60',
    rejected: 'border-line bg-sunken/60',
    failed: 'border-danger/30 bg-danger-soft/60',
  }[action.status]

  const link = RESULT_LINKS[action.tool_name]

  return (
    <div className={cx('rounded-xl border p-4', tone)} data-action-id={action.id}>
      <div className="flex items-center gap-2 text-xs font-medium">
        <StatusLine action={action} />
        <span className="ml-auto text-ink-faint">{TOOL_LABELS[action.tool_name] ?? humanize(action.tool_name)}</span>
      </div>

      <p className="mt-2 text-sm font-medium text-ink">{action.summary}</p>

      <InputDetails input={action.input} />

      {showContext && (
        <p className="mt-2 text-xs text-ink-faint">
          Proposed {relativeTime(action.created_at)}
          {action.conversation_id ? (
            <>
              {' '}
              in{' '}
              <Link to={`/chat/${action.conversation_id}`} className="text-accent hover:underline">
                a conversation
              </Link>
            </>
          ) : (
            ' in a conversation that has since been deleted'
          )}
        </p>
      )}

      {action.status === 'failed' && action.error_message && <p className="mt-2 text-sm text-danger">{action.error_message}</p>}
      {error && <p className="mt-2 text-sm text-danger">{error}</p>}

      {action.status === 'proposed' && (
        <div className="mt-3 flex items-center gap-2">
          <Button variant="primary" size="sm" icon={<IconCheck size={13} />} loading={busy === 'approve'} disabled={busy !== null} onClick={() => void decide('approve')}>
            Approve
          </Button>
          <Button variant="ghost" size="sm" icon={<IconX size={13} />} loading={busy === 'reject'} disabled={busy !== null} onClick={() => void decide('reject')}>
            Reject
          </Button>
          <span className="ml-auto text-xs text-ink-faint">Nothing is written until you approve.</span>
        </div>
      )}

      {action.status === 'executed' && link && (
        <Link to={link.to} className="mt-2 inline-block text-xs font-medium text-accent hover:underline">
          {link.label} →
        </Link>
      )}
    </div>
  )
}

function StatusLine({ action }: { action: Action }) {
  switch (action.status) {
    case 'proposed':
      return (
        <span className="inline-flex items-center gap-1.5 text-warn">
          <IconShield size={14} /> Awaiting your approval — not done yet
        </span>
      )
    case 'approved':
      return (
        <span className="inline-flex items-center gap-1.5 text-accent">
          <Spinner size={12} /> Running…
        </span>
      )
    case 'executed':
      return (
        <span className="inline-flex items-center gap-1.5 text-ok">
          <IconCheck size={14} /> Approved and done
        </span>
      )
    case 'rejected':
      return (
        <span className="inline-flex items-center gap-1.5 text-ink-muted">
          <IconX size={14} /> Rejected — nothing was changed
        </span>
      )
    case 'failed':
      return (
        <span className="inline-flex items-center gap-1.5 text-danger">
          <IconX size={14} /> Approved, but it failed
        </span>
      )
  }
}

function formatValue(key: string, value: unknown): string {
  if (value === null || value === undefined || value === '') return '—'
  if (Array.isArray(value)) return value.length ? value.join(', ') : '—'
  if (typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(value) && /date|deadline/.test(key)) return formatDate(value)
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}

/** The canonical input — exactly what runs on approval. */
function InputDetails({ input }: { input: Record<string, unknown> }) {
  const entries = Object.entries(input ?? {}).filter(([k, v]) => v !== null && v !== undefined && v !== '' && !k.endsWith('_id'))
  if (entries.length === 0) return null
  return (
    <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 rounded-lg bg-surface/70 px-3 py-2 text-xs">
      {entries.map(([key, value]) => (
        <div key={key} className="contents">
          <dt className="text-ink-faint">{humanize(key)}</dt>
          <dd className="break-words text-ink">{formatValue(key, value)}</dd>
        </div>
      ))}
    </dl>
  )
}

function ReadAction({ action }: { action: Action }) {
  return (
    <div className="flex items-start gap-2 rounded-lg border border-line bg-sunken/50 px-3 py-2 text-xs text-ink-muted">
      <IconSearch size={13} className="mt-0.5 shrink-0" />
      <span className="min-w-0 flex-1">
        {action.summary}
        {action.status === 'failed' && action.error_message && <span className="text-danger"> — {action.error_message}</span>}
      </span>
    </div>
  )
}
