import { useState, type FormEvent } from 'react'

import { IconCheck, IconChevron, IconPencil, IconPlus, IconSearch, IconTrash } from '../../components/icons'
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
import { tasks as tasksApi } from '../../lib/endpoints'
import { daysUntil, formatDate, fromDateInput, humanize, parseTags, toDateInput } from '../../lib/format'
import { PRIORITIES, TASK_STATUSES, type Task, type TaskInput } from '../../lib/types'
import { useDebounced } from '../../lib/useDebounced'
import { usePaged } from '../../lib/usePaged'

type StatusFilter = '' | Task['status']

export function TasksPanel() {
  const [status, setStatus] = useState<StatusFilter>('')
  const [search, setSearch] = useState('')
  const q = useDebounced(search.trim())
  const list = usePaged(
    (limit, offset) =>
      tasksApi.list({ limit, offset, status: status || undefined, q: q || undefined }).then((p) => ({ items: p.tasks, count: p.count })),
    `${status}|${q}`,
  )
  const [editing, setEditing] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<Task | null>(null)
  const [rowError, setRowError] = useState<string | null>(null)

  const replace = (task: Task) => list.setItems((items) => items.map((t) => (t.id === task.id ? task : t)))

  const toggleDone = async (task: Task) => {
    const next = task.status === 'completed' ? 'pending' : 'completed'
    replace({ ...task, status: next }) // optimistic; rolled back on failure
    try {
      replace(await tasksApi.update(task.id, { status: next }))
    } catch (e) {
      replace(task)
      setRowError(`Could not update “${task.title}”: ${errorMessage(e)}`)
    }
  }

  const titleOf = (id: string) => list.items.find((t) => t.id === id)?.title

  return (
    <div className="space-y-6">
      <QuickAdd onCreated={(task) => list.setItems((items) => [task, ...items])} parents={list.items} />

      <div className="flex flex-wrap items-center gap-3">
        <div className="flex rounded-lg border border-line bg-surface p-0.5 text-sm">
          {(['', ...TASK_STATUSES] as StatusFilter[]).map((s) => (
            <button
              key={s || 'all'}
              type="button"
              onClick={() => setStatus(s)}
              className={cx(
                'rounded-md px-2.5 py-1 transition-colors',
                status === s ? 'bg-sunken font-medium text-ink' : 'text-ink-faint hover:text-ink-muted',
              )}
            >
              {s ? humanize(s) : 'All'}
            </button>
          ))}
        </div>
        <div className="relative ml-auto w-56">
          <IconSearch size={14} className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-ink-faint" />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search tasks"
            className="h-8 pl-8"
            maxLength={200}
            aria-label="Search tasks"
          />
        </div>
      </div>

      {rowError && <ErrorBanner message={rowError} onDismiss={() => setRowError(null)} />}
      {list.error && <ErrorBanner message={list.error} onRetry={list.reload} />}

      {list.loading && list.items.length === 0 ? (
        <LoadingBlock label="Loading tasks…" />
      ) : !list.error && list.items.length === 0 ? (
        <EmptyState title={q || status ? 'No matching tasks' : 'Nothing on your list'}>
          {q || status ? 'Try a different filter.' : 'Add your first task above, or ask the assistant to propose one.'}
        </EmptyState>
      ) : (
        <ul className={cx('divide-y divide-line rounded-xl border border-line bg-surface', list.loading && 'opacity-60')}>
          {list.items.map((task) =>
            editing === task.id ? (
              <li key={task.id} className="p-4">
                <TaskEditor
                  task={task}
                  allTasks={list.items}
                  onSaved={(t) => {
                    replace(t)
                    setEditing(null)
                  }}
                  onCancel={() => setEditing(null)}
                />
              </li>
            ) : (
              <TaskRow
                key={task.id}
                task={task}
                parentTitle={task.parent_task_id ? titleOf(task.parent_task_id) : undefined}
                dependsOnTitles={task.depends_on.map((id) => titleOf(id) ?? 'another task')}
                onToggle={() => void toggleDone(task)}
                onEdit={() => setEditing(task.id)}
                onDelete={() => setDeleting(task)}
              />
            ),
          )}
        </ul>
      )}
      <LoadMore hasMore={list.hasMore} loading={list.loadingMore} onClick={list.loadMore} />

      <ConfirmDialog
        open={deleting !== null}
        onClose={() => setDeleting(null)}
        title="Delete task?"
        confirmLabel="Delete task"
        onConfirm={async () => {
          if (!deleting) return
          await tasksApi.remove(deleting.id)
          // Subtasks are deleted with their parent on the server.
          const gone = new Set([deleting.id])
          list.setItems((items) => items.filter((t) => !gone.has(t.id) && t.parent_task_id !== deleting.id))
        }}
      >
        <p>
          “{deleting?.title}” will be permanently deleted, along with any subtasks and dependency links. This cannot be
          undone.
        </p>
      </ConfirmDialog>
    </div>
  )
}

function TaskRow({
  task,
  parentTitle,
  dependsOnTitles,
  onToggle,
  onEdit,
  onDelete,
}: {
  task: Task
  parentTitle?: string
  dependsOnTitles: string[]
  onToggle: () => void
  onEdit: () => void
  onDelete: () => void
}) {
  const done = task.status === 'completed'
  const due = task.deadline ? daysUntil(task.deadline) : null
  return (
    <li className="group flex items-start gap-3 px-4 py-3">
      <button
        type="button"
        role="checkbox"
        aria-checked={done}
        aria-label={done ? `Mark “${task.title}” as pending` : `Complete “${task.title}”`}
        onClick={onToggle}
        className={cx(
          'mt-0.5 flex size-[18px] shrink-0 items-center justify-center rounded-full border transition-colors',
          done ? 'border-ok bg-ok text-white dark:text-canvas' : 'border-line-strong hover:border-ok',
        )}
      >
        {done && <IconCheck size={12} strokeWidth={2.5} />}
      </button>
      <div className="min-w-0 flex-1">
        <p className={cx('text-sm', done ? 'text-ink-faint line-through' : 'text-ink')}>{task.title}</p>
        {task.description && <p className="mt-0.5 line-clamp-2 text-xs text-ink-muted">{task.description}</p>}
        <div className="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs text-ink-faint">
          {task.priority === 'high' && <Badge tone="danger">High</Badge>}
          {task.priority === 'low' && <Badge>Low</Badge>}
          {task.status === 'in_progress' && <Badge tone="accent">In progress</Badge>}
          {task.deadline && (
            <span className={cx(!done && due !== null && due < 0 && 'font-medium text-danger', !done && due === 0 && 'font-medium text-warn')}>
              {due === 0 ? 'Due today' : `Due ${formatDate(task.deadline)}`}
            </span>
          )}
          {task.category && <span>· {task.category}</span>}
          {task.tags.map((tag) => (
            <span key={tag} className="text-accent">
              #{tag}
            </span>
          ))}
          {parentTitle && <span>· Subtask of “{parentTitle}”</span>}
          {dependsOnTitles.length > 0 && <span>· Waits on {dependsOnTitles.map((t) => `“${t}”`).join(', ')}</span>}
          {task.estimated_effort_minutes != null && <span>· ~{task.estimated_effort_minutes} min</span>}
        </div>
      </div>
      <div className="flex gap-0.5 opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
        <IconButton label="Edit task" onClick={onEdit}>
          <IconPencil size={14} />
        </IconButton>
        <IconButton label="Delete task" tone="danger" onClick={onDelete}>
          <IconTrash size={14} />
        </IconButton>
      </div>
    </li>
  )
}

// --- create ----------------------------------------------------------------

function QuickAdd({ onCreated, parents }: { onCreated: (task: Task) => void; parents: Task[] }) {
  const [values, setValues] = useState<FormValues>(emptyValues)
  const [details, setDetails] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | string | null>(null)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (!values.title.trim()) return
    setBusy(true)
    setError(null)
    try {
      onCreated(await tasksApi.create(toInput(values)))
      setValues(emptyValues)
      setDetails(false)
    } catch (e) {
      setError(e instanceof ApiError ? e : errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="rounded-xl border border-line bg-surface p-3 shadow-xs">
      <div className="flex flex-wrap items-center gap-2">
        <Input
          value={values.title}
          onChange={(e) => setValues({ ...values, title: e.target.value })}
          placeholder="Add a task…"
          aria-label="Task title"
          maxLength={500}
          className="min-w-48 flex-1 border-transparent shadow-none focus:border-accent"
        />
        <Select
          value={values.priority}
          onChange={(e) => setValues({ ...values, priority: e.target.value as Task['priority'] })}
          aria-label="Priority"
          className="w-28"
        >
          {PRIORITIES.map((p) => (
            <option key={p} value={p}>
              {humanize(p)}
            </option>
          ))}
        </Select>
        <Input
          type="date"
          value={values.deadline}
          onChange={(e) => setValues({ ...values, deadline: e.target.value })}
          aria-label="Deadline"
          className="w-36"
        />
        <Button variant="ghost" size="sm" onClick={() => setDetails((d) => !d)} aria-expanded={details}>
          <IconChevron size={13} className={cx('transition-transform', details && 'rotate-90')} /> Details
        </Button>
        <Button type="submit" variant="primary" loading={busy} disabled={!values.title.trim()} icon={<IconPlus size={15} />}>
          Add
        </Button>
      </div>
      {details && (
        <div className="mt-3 border-t border-line pt-3">
          <TaskFields values={values} onChange={setValues} parents={parents} error={error} compact />
        </div>
      )}
      {error && <FormError error={error} className="mt-3" />}
    </form>
  )
}

// --- edit ------------------------------------------------------------------

function TaskEditor({
  task,
  allTasks,
  onSaved,
  onCancel,
}: {
  task: Task
  allTasks: Task[]
  onSaved: (task: Task) => void
  onCancel: () => void
}) {
  const [values, setValues] = useState<FormValues>(() => fromTask(task))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | string | null>(null)
  const [current, setCurrent] = useState(task)
  const [dependency, setDependency] = useState('')
  const [depBusy, setDepBusy] = useState(false)
  const [depError, setDepError] = useState<string | null>(null)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const patch = diff(current, toInput(values), values)
    if (Object.keys(patch).length === 0) return onSaved(current)
    setBusy(true)
    setError(null)
    try {
      onSaved(await tasksApi.update(task.id, patch))
    } catch (e) {
      setError(e instanceof ApiError ? e : errorMessage(e))
      setBusy(false)
    }
  }

  const addDependency = async () => {
    if (!dependency) return
    setDepBusy(true)
    setDepError(null)
    try {
      const updated = await tasksApi.addDependency(task.id, dependency)
      setCurrent(updated)
      setDependency('')
    } catch (e) {
      setDepError(errorMessage(e))
    } finally {
      setDepBusy(false)
    }
  }

  const others = allTasks.filter((t) => t.id !== task.id)
  const candidates = others.filter((t) => !current.depends_on.includes(t.id))

  return (
    <form onSubmit={submit} className="space-y-4">
      <TaskFields values={values} onChange={setValues} parents={others} error={error} showStatus />

      <div className="rounded-lg bg-sunken/60 p-3">
        <p className="text-xs font-medium text-ink-muted">Waits on</p>
        {current.depends_on.length === 0 ? (
          <p className="mt-1 text-xs text-ink-faint">No dependencies.</p>
        ) : (
          <ul className="mt-1 space-y-0.5 text-sm">
            {current.depends_on.map((id) => (
              <li key={id}>{others.find((t) => t.id === id)?.title ?? 'A task not on this page'}</li>
            ))}
          </ul>
        )}
        <div className="mt-2 flex gap-2">
          <Select value={dependency} onChange={(e) => setDependency(e.target.value)} aria-label="Add a dependency" className="h-8 flex-1">
            <option value="">Add a task this one waits on…</option>
            {candidates.map((t) => (
              <option key={t.id} value={t.id}>
                {t.title}
              </option>
            ))}
          </Select>
          <Button size="sm" className="h-8" onClick={() => void addDependency()} loading={depBusy} disabled={!dependency}>
            Add
          </Button>
        </div>
        {depError && <p className="mt-1.5 text-xs text-danger">{depError}</p>}
      </div>

      {error && <FormError error={error} />}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={() => (current !== task ? onSaved(current) : onCancel())}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" loading={busy}>
          Save
        </Button>
      </div>
    </form>
  )
}

// --- shared form -----------------------------------------------------------

type FormValues = {
  title: string
  description: string
  priority: Task['priority']
  status: Task['status']
  category: string
  tags: string
  deadline: string
  parent_task_id: string
  estimated: string
  actual: string
}

const emptyValues: FormValues = {
  title: '',
  description: '',
  priority: 'medium',
  status: 'pending',
  category: '',
  tags: '',
  deadline: '',
  parent_task_id: '',
  estimated: '',
  actual: '',
}

function fromTask(t: Task): FormValues {
  return {
    title: t.title,
    description: t.description ?? '',
    priority: t.priority,
    status: t.status,
    category: t.category ?? '',
    tags: t.tags.join(', '),
    deadline: toDateInput(t.deadline),
    parent_task_id: t.parent_task_id ?? '',
    estimated: t.estimated_effort_minutes?.toString() ?? '',
    actual: t.actual_effort_minutes?.toString() ?? '',
  }
}

const minutes = (s: string) => (s.trim() === '' ? null : Number(s))

function toInput(v: FormValues): TaskInput {
  return {
    title: v.title.trim(),
    description: v.description.trim() || null,
    priority: v.priority,
    status: v.status,
    category: v.category.trim() || null,
    tags: parseTags(v.tags),
    deadline: fromDateInput(v.deadline),
    parent_task_id: v.parent_task_id || null,
    estimated_effort_minutes: minutes(v.estimated),
    actual_effort_minutes: minutes(v.actual),
  }
}

/**
 * Only the keys that changed: PATCH is a true partial update, and sending an
 * unchanged deadline would reset a time-of-day the date input cannot show.
 */
function diff(task: Task, next: TaskInput, values: FormValues): Partial<TaskInput> {
  const patch: Partial<TaskInput> = {}
  if (next.title !== task.title) patch.title = next.title
  if (next.description !== task.description) patch.description = next.description
  if (next.priority !== task.priority) patch.priority = next.priority
  if (next.status !== task.status) patch.status = next.status
  if (next.category !== task.category) patch.category = next.category
  if (next.tags.join(' ') !== task.tags.join(' ')) patch.tags = next.tags
  if (values.deadline !== toDateInput(task.deadline)) patch.deadline = next.deadline
  if (next.parent_task_id !== task.parent_task_id) patch.parent_task_id = next.parent_task_id
  if (next.estimated_effort_minutes !== task.estimated_effort_minutes) patch.estimated_effort_minutes = next.estimated_effort_minutes
  if (next.actual_effort_minutes !== task.actual_effort_minutes) patch.actual_effort_minutes = next.actual_effort_minutes
  return patch
}

function TaskFields({
  values,
  onChange,
  parents,
  error,
  compact,
  showStatus,
}: {
  values: FormValues
  onChange: (v: FormValues) => void
  parents: Task[]
  error: ApiError | string | null
  compact?: boolean
  showStatus?: boolean
}) {
  const fe = (f: string) => (error instanceof ApiError ? error.fieldError(f) : undefined)
  const set = <K extends keyof FormValues>(key: K, value: FormValues[K]) => onChange({ ...values, [key]: value })
  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
      {!compact && (
        <Field label="Title" error={fe('title')} className="col-span-2 sm:col-span-4">
          {(id) => <Input id={id} value={values.title} onChange={(e) => set('title', e.target.value)} maxLength={500} required />}
        </Field>
      )}
      <Field label="Description" error={fe('description')} className="col-span-2 sm:col-span-4">
        {(id) => (
          <Textarea id={id} rows={2} value={values.description} onChange={(e) => set('description', e.target.value)} maxLength={10000} />
        )}
      </Field>
      {!compact && (
        <Field label="Priority" error={fe('priority')}>
          {(id) => (
            <Select id={id} value={values.priority} onChange={(e) => set('priority', e.target.value as Task['priority'])}>
              {PRIORITIES.map((p) => (
                <option key={p} value={p}>
                  {humanize(p)}
                </option>
              ))}
            </Select>
          )}
        </Field>
      )}
      {showStatus && (
        <Field label="Status" error={fe('status')}>
          {(id) => (
            <Select id={id} value={values.status} onChange={(e) => set('status', e.target.value as Task['status'])}>
              {TASK_STATUSES.map((s) => (
                <option key={s} value={s}>
                  {humanize(s)}
                </option>
              ))}
            </Select>
          )}
        </Field>
      )}
      {!compact && (
        <Field label="Deadline" error={fe('deadline')}>
          {(id) => <Input id={id} type="date" value={values.deadline} onChange={(e) => set('deadline', e.target.value)} />}
        </Field>
      )}
      <Field label="Category" error={fe('category')}>
        {(id) => <Input id={id} value={values.category} onChange={(e) => set('category', e.target.value)} maxLength={100} />}
      </Field>
      <Field label="Tags" hint="Comma-separated" error={fe('tags')} className={compact ? 'col-span-1 sm:col-span-3' : 'col-span-2 sm:col-span-2'}>
        {(id) => <Input id={id} value={values.tags} onChange={(e) => set('tags', e.target.value)} placeholder="work, urgent" />}
      </Field>
      <Field label="Estimate (min)" error={fe('estimated_effort_minutes')}>
        {(id) => (
          <Input id={id} type="number" min={0} max={525600} value={values.estimated} onChange={(e) => set('estimated', e.target.value)} />
        )}
      </Field>
      <Field label="Actual (min)" error={fe('actual_effort_minutes')}>
        {(id) => <Input id={id} type="number" min={0} max={525600} value={values.actual} onChange={(e) => set('actual', e.target.value)} />}
      </Field>
      <Field label="Subtask of" error={fe('parent_task_id')} className="col-span-2">
        {(id) => (
          <Select id={id} value={values.parent_task_id} onChange={(e) => set('parent_task_id', e.target.value)}>
            <option value="">— None —</option>
            {parents.map((t) => (
              <option key={t.id} value={t.id}>
                {t.title}
              </option>
            ))}
          </Select>
        )}
      </Field>
    </div>
  )
}

function FormError({ error, className }: { error: ApiError | string; className?: string }) {
  // Field errors are rendered beside their fields; a banner still says why the
  // form did not save, so a field that is not on screen is not a silent failure.
  return <ErrorBanner className={className} message={typeof error === 'string' ? error : errorMessage(error)} />
}
