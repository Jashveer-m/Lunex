import { useEffect, useState, type FormEvent } from 'react'

import { IconTrash } from '../../components/icons'
import { Button, ConfirmDialog, Dialog, ErrorBanner, Field, Input, Textarea, Toggle } from '../../components/ui'
import { ApiError, errorMessage } from '../../lib/api'
import { calendar, goals, tasks } from '../../lib/endpoints'
import { formatStamp } from '../../lib/format'
import { Link } from '../../lib/router'
import type { CalendarEvent, CalendarEventInput } from '../../lib/types'
import { allDayDates, allDayInterval, fromLocalInput, shiftLocalInput, toLocalInput } from '../../lib/week'

/** Create an event (event = null) or edit and delete an existing one. */
export function EventDialog({
  open,
  event,
  defaultDay,
  onClose,
  onSaved,
  onDeleted,
}: {
  open: boolean
  event: CalendarEvent | null
  defaultDay: string
  onClose: () => void
  onSaved: (event: CalendarEvent) => void
  onDeleted: (id: string) => void
}) {
  const [confirming, setConfirming] = useState(false)

  return (
    <>
      <Dialog open={open} onClose={onClose} title={event ? 'Edit event' : 'New event'}>
        {/* Keyed so reopening on another event starts from that event's values. */}
        <EventForm
          key={event?.id ?? `new-${defaultDay}`}
          event={event}
          defaultDay={defaultDay}
          onCancel={onClose}
          onDelete={() => setConfirming(true)}
          onSubmit={async (input) => {
            const saved = event ? await calendar.update(event.id, input) : await calendar.create(createBody(input))
            onSaved(saved)
            onClose()
          }}
        />
      </Dialog>
      <ConfirmDialog
        open={confirming && event !== null}
        onClose={() => setConfirming(false)}
        title="Delete event?"
        confirmLabel="Delete event"
        onConfirm={async () => {
          if (!event) return
          await calendar.remove(event.id)
          onDeleted(event.id)
          onClose()
        }}
      >
        <p>“{event?.title}” will be permanently removed from your calendar.</p>
      </ConfirmDialog>
    </>
  )
}

/** A create body leaves empty optional fields out rather than sending null. */
function createBody(input: CalendarEventInput) {
  return {
    title: input.title,
    start_time: input.start_time,
    end_time: input.end_time,
    all_day: input.all_day,
    description: input.description ?? undefined,
    location: input.location ?? undefined,
  }
}

function initialTimes(event: CalendarEvent | null, defaultDay: string) {
  if (event && !event.all_day) {
    const start = toLocalInput(event.start_time)
    const end = toLocalInput(event.end_time)
    return { start, end, first: start.slice(0, 10), last: end.slice(0, 10) }
  }
  if (event) {
    const { first, last } = allDayDates(event)
    return { start: `${first}T09:00`, end: `${first}T10:00`, first, last }
  }
  return { start: `${defaultDay}T09:00`, end: `${defaultDay}T10:00`, first: defaultDay, last: defaultDay }
}

function EventForm({
  event,
  defaultDay,
  onSubmit,
  onCancel,
  onDelete,
}: {
  event: CalendarEvent | null
  defaultDay: string
  onSubmit: (input: CalendarEventInput) => Promise<void>
  onCancel: () => void
  onDelete: () => void
}) {
  const init = initialTimes(event, defaultDay)
  const [title, setTitle] = useState(event?.title ?? '')
  const [allDay, setAllDay] = useState(event?.all_day ?? false)
  const [start, setStart] = useState(init.start)
  const [end, setEnd] = useState(init.end)
  const [first, setFirst] = useState(init.first)
  const [last, setLast] = useState(init.last)
  const [location, setLocation] = useState(event?.location ?? '')
  const [description, setDescription] = useState(event?.description ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | string | null>(null)
  const fe = (f: string) => (error instanceof ApiError ? error.fieldError(f) : undefined)

  const toggleAllDay = (next: boolean) => {
    // Carry the dates across so switching modes does not lose the day picked.
    if (next) {
      setFirst(start.slice(0, 10))
      setLast(end.slice(0, 10) < start.slice(0, 10) ? start.slice(0, 10) : end.slice(0, 10))
    } else {
      const nextStart = `${first}${start.slice(10)}`
      const nextEnd = `${last}${end.slice(10)}`
      setStart(nextStart)
      setEnd(nextEnd < nextStart ? shiftLocalInput(nextStart, 60) : nextEnd)
    }
    setAllDay(next)
  }

  const changeStart = (value: string) => {
    // Moving the start past the end drags the end along, keeping an hour.
    if (value && end && value >= end) setEnd(shiftLocalInput(value, 60))
    setStart(value)
  }

  const changeFirst = (value: string) => {
    if (value && last && value > last) setLast(value)
    setFirst(value)
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    let interval: { start_time: string; end_time: string }
    if (allDay) {
      if (!first || !last) return setError('Pick a start and end date.')
      if (last < first) return setError('The end date is before the start date.')
      interval = allDayInterval(first, last)
    } else {
      const s = fromLocalInput(start)
      const en = fromLocalInput(end)
      if (!s || !en) return setError('Pick a start and end time.')
      if (en < s) return setError('The event ends before it starts.')
      interval = { start_time: s, end_time: en }
    }
    setBusy(true)
    setError(null)
    try {
      await onSubmit({
        title: title.trim(),
        ...interval,
        all_day: allDay,
        location: location.trim() || null,
        description: description.trim() || null,
      })
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

      <label className="flex items-center gap-2.5 text-sm text-ink">
        <Toggle checked={allDay} onChange={toggleAllDay} label="All day" />
        All day
      </label>

      {allDay ? (
        <div className="grid grid-cols-2 gap-3">
          <Field label="Starts" error={fe('start_time')}>
            {(id) => <Input id={id} type="date" value={first} onChange={(e) => changeFirst(e.target.value)} required />}
          </Field>
          <Field label="Ends" error={fe('end_time')}>
            {(id) => <Input id={id} type="date" value={last} min={first} onChange={(e) => setLast(e.target.value)} required />}
          </Field>
        </div>
      ) : (
        <div className="grid grid-cols-2 gap-3">
          <Field label="Starts" error={fe('start_time')}>
            {(id) => <Input id={id} type="datetime-local" value={start} onChange={(e) => changeStart(e.target.value)} required />}
          </Field>
          <Field label="Ends" error={fe('end_time')}>
            {(id) => <Input id={id} type="datetime-local" value={end} min={start} onChange={(e) => setEnd(e.target.value)} required />}
          </Field>
        </div>
      )}

      <Field label="Location" error={fe('location')}>
        {(id) => <Input id={id} value={location} onChange={(e) => setLocation(e.target.value)} maxLength={500} placeholder="Optional" />}
      </Field>
      <Field label="Description" error={fe('description')}>
        {(id) => (
          <Textarea id={id} rows={3} value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Optional" />
        )}
      </Field>

      {event && <Related event={event} />}

      {error && <ErrorBanner message={typeof error === 'string' ? error : errorMessage(error)} />}

      <div className="flex items-center gap-2 pt-1">
        {event && (
          <Button variant="ghost" className="text-danger hover:bg-danger-soft hover:text-danger" icon={<IconTrash size={14} />} onClick={onDelete}>
            Delete
          </Button>
        )}
        <div className="ml-auto flex gap-2">
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!title.trim()}>
            {event ? 'Save' : 'Add event'}
          </Button>
        </div>
      </div>
    </form>
  )
}

/** What the event is linked to, plus the read-only facts the form does not edit. */
function Related({ event }: { event: CalendarEvent }) {
  const [taskTitle, setTaskTitle] = useState<string | null>(null)
  const [goalTitle, setGoalTitle] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    // A title is a nicety: if the lookup fails the link still says what it is.
    if (event.related_task_id) {
      tasks.get(event.related_task_id).then((t) => !cancelled && setTaskTitle(t.title), () => undefined)
    }
    if (event.related_goal_id) {
      goals.get(event.related_goal_id).then((g) => !cancelled && setGoalTitle(g.title), () => undefined)
    }
    return () => {
      cancelled = true
    }
  }, [event.related_task_id, event.related_goal_id])

  return (
    <div className="space-y-1.5 rounded-lg bg-sunken px-3 py-2.5 text-xs text-ink-muted">
      {event.related_task_id && (
        <p>
          For task{' '}
          <Link to="/" className="font-medium text-accent hover:underline">
            {taskTitle ?? 'view tasks'}
          </Link>
        </p>
      )}
      {event.related_goal_id && (
        <p>
          For goal{' '}
          <Link to="/?tab=goals" className="font-medium text-accent hover:underline">
            {goalTitle ?? 'view goals'}
          </Link>
        </p>
      )}
      {event.recurrence_rule && (
        <p>
          Repeats (<code className="font-mono">{event.recurrence_rule}</code>). Only this occurrence is shown; repeats are not
          expanded yet.
        </p>
      )}
      <p className="text-ink-faint">Last updated {formatStamp(event.updated_at)}</p>
    </div>
  )
}
