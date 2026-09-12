import { useState, type FormEvent } from 'react'

import { IconCheck, IconChevron, IconFlag, IconPencil, IconPlus, IconTrash } from '../../components/icons'
import {
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  ErrorBanner,
  Field,
  IconButton,
  Input,
  LoadMore,
  LoadingBlock,
  Select,
  Textarea,
  cx,
} from '../../components/ui'
import { ApiError, errorMessage } from '../../lib/api'
import { goals as goalsApi } from '../../lib/endpoints'
import { daysUntil, formatDate, fromDateInput, humanize, toDateInput } from '../../lib/format'
import { GOAL_STATUSES, GOAL_TYPES, type Goal, type GoalInput, type GoalStatus, type GoalType, type Milestone } from '../../lib/types'
import { usePaged } from '../../lib/usePaged'

export function GoalsPanel() {
  const [status, setStatus] = useState<GoalStatus | ''>('active')
  const list = usePaged(
    (limit, offset) =>
      goalsApi.list({ limit, offset, status: status || undefined, sort: 'deadline' }).then((p) => ({ items: p.goals, count: p.count })),
    status,
  )
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<Goal | null>(null)

  const replace = (goal: Goal) => list.setItems((items) => items.map((g) => (g.id === goal.id ? goal : g)))

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center gap-3">
        <Select value={status} onChange={(e) => setStatus(e.target.value as GoalStatus | '')} className="h-8 w-40" aria-label="Filter by status">
          <option value="">All goals</option>
          {GOAL_STATUSES.map((s) => (
            <option key={s} value={s}>
              {humanize(s)}
            </option>
          ))}
        </Select>
        <div className="ml-auto">
          {!creating && (
            <Button variant="primary" icon={<IconPlus size={15} />} onClick={() => setCreating(true)}>
              New goal
            </Button>
          )}
        </div>
      </div>

      {creating && (
        <div className="rounded-xl border border-line bg-surface p-4 shadow-xs">
          <GoalForm
            submitLabel="Create goal"
            onCancel={() => setCreating(false)}
            onSubmit={async (input) => {
              const goal = await goalsApi.create(input)
              list.setItems((items) => [goal, ...items])
              setCreating(false)
            }}
          />
        </div>
      )}

      {list.error && <ErrorBanner message={list.error} onRetry={list.reload} />}

      {list.loading && list.items.length === 0 ? (
        <LoadingBlock label="Loading goals…" />
      ) : !list.error && list.items.length === 0 ? (
        <EmptyState title={status ? `No ${humanize(status).toLowerCase()} goals` : 'No goals yet'} icon={<IconFlag size={22} />}>
          Goals hold milestones and give the assistant context about what you are working towards.
        </EmptyState>
      ) : (
        <div className={cx('space-y-3', list.loading && 'opacity-60')}>
          {list.items.map((goal) => (
            <GoalCard key={goal.id} goal={goal} onChange={replace} onDelete={() => setDeleting(goal)} />
          ))}
        </div>
      )}
      <LoadMore hasMore={list.hasMore} loading={list.loadingMore} onClick={list.loadMore} />

      <ConfirmDialog
        open={deleting !== null}
        onClose={() => setDeleting(null)}
        title="Delete goal?"
        confirmLabel="Delete goal"
        onConfirm={async () => {
          if (!deleting) return
          await goalsApi.remove(deleting.id)
          list.setItems((items) => items.filter((g) => g.id !== deleting.id))
        }}
      >
        <p>“{deleting?.title}” and its {deleting?.milestones.length ?? 0} milestone(s) will be permanently deleted.</p>
      </ConfirmDialog>
    </div>
  )
}

const statusTone = { active: 'accent', completed: 'ok', abandoned: 'neutral' } as const

function GoalCard({ goal, onChange, onDelete }: { goal: Goal; onChange: (g: Goal) => void; onDelete: () => void }) {
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const done = goal.milestones.filter((m) => m.completed).length
  const total = goal.milestones.length
  const due = goal.deadline ? daysUntil(goal.deadline) : null

  const setMilestone = (m: Milestone) => onChange({ ...goal, milestones: goal.milestones.map((x) => (x.id === m.id ? m : x)) })

  const toggleMilestone = async (m: Milestone) => {
    setError(null)
    setMilestone({ ...m, completed: !m.completed })
    try {
      setMilestone(await goalsApi.updateMilestone(goal.id, m.id, { completed: !m.completed }))
    } catch (e) {
      setMilestone(m)
      setError(errorMessage(e))
    }
  }

  if (editing) {
    return (
      <div className="rounded-xl border border-line bg-surface p-4">
        <GoalForm
          goal={goal}
          submitLabel="Save"
          onCancel={() => setEditing(false)}
          onSubmit={async (input) => {
            onChange(await goalsApi.update(goal.id, input))
            setEditing(false)
          }}
        />
      </div>
    )
  }

  return (
    <div className="group rounded-xl border border-line bg-surface">
      <div className="flex items-start gap-3 p-4">
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          aria-expanded={open}
          aria-label={open ? 'Hide milestones' : 'Show milestones'}
          className="mt-0.5 rounded p-0.5 text-ink-faint hover:text-ink"
        >
          <IconChevron size={16} className={cx('transition-transform', open && 'rotate-90')} />
        </button>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <p className={cx('font-medium', goal.status === 'abandoned' && 'text-ink-faint line-through')}>{goal.title}</p>
            <Badge tone={statusTone[goal.status]}>{humanize(goal.status)}</Badge>
            <Badge>{humanize(goal.type)}</Badge>
          </div>
          {goal.description && <p className="mt-1 text-sm text-ink-muted">{goal.description}</p>}
          <div className="mt-3 flex items-center gap-3 text-xs text-ink-faint">
            <div className="h-1.5 w-32 overflow-hidden rounded-full bg-sunken" aria-hidden="true">
              <div className="h-full rounded-full bg-ok transition-all" style={{ width: total ? `${(done / total) * 100}%` : '0%' }} />
            </div>
            <span>{total ? `${done}/${total} milestones` : 'No milestones'}</span>
            {goal.deadline && (
              <span className={cx(goal.status === 'active' && due !== null && due < 0 && 'font-medium text-danger')}>
                · Due {formatDate(goal.deadline)}
              </span>
            )}
          </div>
        </div>
        <div className="flex gap-0.5 opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
          <IconButton label="Edit goal" onClick={() => setEditing(true)}>
            <IconPencil size={14} />
          </IconButton>
          <IconButton label="Delete goal" tone="danger" onClick={onDelete}>
            <IconTrash size={14} />
          </IconButton>
        </div>
      </div>

      {open && (
        <div className="border-t border-line px-4 py-3 pl-12">
          {error && <ErrorBanner className="mb-2" message={error} onDismiss={() => setError(null)} />}
          <ul className="space-y-1.5">
            {goal.milestones.map((m) => (
              <li key={m.id} className="flex items-center gap-2.5 text-sm">
                <button
                  type="button"
                  role="checkbox"
                  aria-checked={m.completed}
                  aria-label={`Toggle milestone “${m.title}”`}
                  onClick={() => void toggleMilestone(m)}
                  className={cx(
                    'flex size-4 shrink-0 items-center justify-center rounded border transition-colors',
                    m.completed ? 'border-ok bg-ok text-white dark:text-canvas' : 'border-line-strong hover:border-ok',
                  )}
                >
                  {m.completed && <IconCheck size={11} strokeWidth={2.5} />}
                </button>
                <span className={cx('flex-1', m.completed && 'text-ink-faint line-through')}>{m.title}</span>
                {m.target_date && <span className="text-xs text-ink-faint">{formatDate(m.target_date)}</span>}
              </li>
            ))}
          </ul>
          <AddMilestone
            onAdd={async (title, date) => {
              const m = await goalsApi.addMilestone(goal.id, { title, target_date: date })
              // Keep the server's order: dated first by date, undated last.
              const milestones = [...goal.milestones, m].sort(
                (a, b) =>
                  (a.target_date ? Date.parse(a.target_date) : Infinity) - (b.target_date ? Date.parse(b.target_date) : Infinity) ||
                  Date.parse(a.created_at) - Date.parse(b.created_at),
              )
              onChange({ ...goal, milestones })
            }}
          />
        </div>
      )}
    </div>
  )
}

function AddMilestone({ onAdd }: { onAdd: (title: string, date: string | null) => Promise<void> }) {
  const [title, setTitle] = useState('')
  const [date, setDate] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!title.trim()) return
    setBusy(true)
    setError(null)
    try {
      await onAdd(title.trim(), fromDateInput(date))
      setTitle('')
      setDate('')
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} className="mt-3">
      <div className="flex gap-2">
        <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Add a milestone…" className="h-8 flex-1" maxLength={500} aria-label="Milestone title" />
        <Input type="date" value={date} onChange={(e) => setDate(e.target.value)} className="h-8 w-36" aria-label="Target date" />
        <Button type="submit" size="sm" className="h-8" loading={busy} disabled={!title.trim()}>
          Add
        </Button>
      </div>
      {error && <p className="mt-1.5 text-xs text-danger">{error}</p>}
    </form>
  )
}

function GoalForm({
  goal,
  submitLabel,
  onSubmit,
  onCancel,
}: {
  goal?: Goal
  submitLabel: string
  onSubmit: (input: GoalInput) => Promise<void>
  onCancel: () => void
}) {
  const [title, setTitle] = useState(goal?.title ?? '')
  const [description, setDescription] = useState(goal?.description ?? '')
  const [type, setType] = useState<GoalType | ''>(goal?.type ?? '')
  const [status, setStatus] = useState<GoalStatus>(goal?.status ?? 'active')
  const [deadline, setDeadline] = useState(toDateInput(goal?.deadline))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | string | null>(null)
  const fe = (f: string) => (error instanceof ApiError ? error.fieldError(f) : undefined)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!type) {
      setError(new ApiError(400, 'validation_failed', '', [{ field: 'type', message: 'choose a type' }]))
      return
    }
    const input: GoalInput = {
      title: title.trim(),
      description: description.trim() || null,
      type,
      status,
      deadline: fromDateInput(deadline),
    }
    // Leave an unchanged deadline out so a time-of-day set elsewhere survives.
    const body: Partial<GoalInput> = { ...input }
    if (goal && deadline === toDateInput(goal.deadline)) delete body.deadline
    setBusy(true)
    setError(null)
    try {
      await onSubmit(body as GoalInput)
    } catch (err) {
      setError(err instanceof ApiError ? err : errorMessage(err))
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="space-y-3">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Field label="Title" error={fe('title')} className="col-span-2 sm:col-span-4">
          {(id) => <Input id={id} value={title} onChange={(e) => setTitle(e.target.value)} maxLength={500} autoFocus required />}
        </Field>
        <Field label="Description" error={fe('description')} className="col-span-2 sm:col-span-4">
          {(id) => <Textarea id={id} rows={2} value={description} onChange={(e) => setDescription(e.target.value)} maxLength={10000} />}
        </Field>
        <Field label="Type" error={fe('type')}>
          {(id) => (
            <Select id={id} value={type} onChange={(e) => setType(e.target.value as GoalType)} aria-invalid={!!fe('type')}>
              <option value="" disabled>
                Choose…
              </option>
              {GOAL_TYPES.map((t) => (
                <option key={t} value={t}>
                  {humanize(t)}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field label="Status" error={fe('status')}>
          {(id) => (
            <Select id={id} value={status} onChange={(e) => setStatus(e.target.value as GoalStatus)}>
              {GOAL_STATUSES.map((s) => (
                <option key={s} value={s}>
                  {humanize(s)}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field label="Deadline" error={fe('deadline')}>
          {(id) => <Input id={id} type="date" value={deadline} onChange={(e) => setDeadline(e.target.value)} />}
        </Field>
      </div>
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
