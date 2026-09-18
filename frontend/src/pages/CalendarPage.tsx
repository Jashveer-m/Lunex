import { useEffect, useMemo, useState } from 'react'

import { PageContainer } from '../components/Shell'
import { IconCalendar, IconChevron, IconPlus } from '../components/icons'
import { Badge, Button, EmptyState, ErrorBanner, IconButton, LoadingBlock, PageHeader, cx } from '../components/ui'
import { errorMessage, isAbort } from '../lib/api'
import { calendar } from '../lib/endpoints'
import { onActionsChanged } from '../lib/events'
import { navigate, useLocation } from '../lib/router'
import type { CalendarEvent } from '../lib/types'
import {
  addDays,
  eventDayKeys,
  formatWeekRange,
  localDayKey,
  parseDayKey,
  startOfWeek,
  timeLabelForDay,
  weekWindow,
} from '../lib/week'
import { EventDialog } from './calendar/EventDialog'

// The API caps a page at 500. A week holding more than that is not a calendar
// anyone reads, so one page is fetched and the overflow is said out loud.
const WEEK_LIMIT = 500

const weekdayFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short' })

type Editing = { event: CalendarEvent | null; day: string }

export function CalendarPage() {
  const { query } = useLocation()
  const weekParam = query.get('week')
  const monday = useMemo(() => startOfWeek(parseDayKey(weekParam ?? '') ?? new Date()), [weekParam])
  const mondayKey = localDayKey(monday)
  const todayKey = localDayKey(new Date())
  const days = useMemo(() => Array.from({ length: 7 }, (_, i) => addDays(monday, i)), [monday])

  const [events, setEvents] = useState<CalendarEvent[]>([])
  const [loadedWeek, setLoadedWeek] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [reloadKey, setReloadKey] = useState(0)
  const [editing, setEditing] = useState<Editing | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    setError(null)
    calendar
      .list({ ...weekWindow(monday), limit: WEEK_LIMIT }, controller.signal)
      .then((page) => {
        setEvents(page.events)
        setLoadedWeek(mondayKey)
      })
      .catch((err) => !isAbort(err) && setError(errorMessage(err)))
      .finally(() => !controller.signal.aborted && setLoading(false))
    return () => controller.abort()
  }, [monday, mondayKey, reloadKey])

  // An event the assistant created through an approval should appear without a
  // manual refresh.
  useEffect(() => onActionsChanged(() => setReloadKey((k) => k + 1)), [])

  const byDay = useMemo(() => {
    const map = new Map<string, CalendarEvent[]>()
    for (const event of events) {
      for (const key of eventDayKeys(event)) {
        const list = map.get(key)
        if (list) list.push(event)
        else map.set(key, [event])
      }
    }
    for (const list of map.values()) {
      list.sort((a, b) => Number(b.all_day) - Number(a.all_day) || a.start_time.localeCompare(b.start_time))
    }
    return map
  }, [events])

  const visibleCount = useMemo(() => {
    const ids = new Set<string>()
    for (const d of days) for (const e of byDay.get(localDayKey(d)) ?? []) ids.add(e.id)
    return ids.size
  }, [byDay, days])

  const goToWeek = (d: Date | null) => {
    const key = d ? localDayKey(startOfWeek(d)) : null
    navigate(key && key !== localDayKey(startOfWeek(new Date())) ? `/calendar?week=${key}` : '/calendar', { replace: true })
  }

  const upsert = (saved: CalendarEvent) =>
    setEvents((list) => (list.some((e) => e.id === saved.id) ? list.map((e) => (e.id === saved.id ? saved : e)) : [...list, saved]))

  const isCurrentWeek = mondayKey === localDayKey(startOfWeek(new Date()))
  const stale = loadedWeek !== mondayKey
  const firstLoad = loading && loadedWeek === null

  return (
    <PageContainer wide>
      <PageHeader
        title="Calendar"
        description={
          <>
            {formatWeekRange(monday)}
            {!stale && !loading && <span className="text-ink-faint"> · {visibleCount === 1 ? '1 event' : `${visibleCount} events`}</span>}
          </>
        }
        actions={
          <>
            <div className="flex items-center gap-1">
              <IconButton label="Previous week" onClick={() => goToWeek(addDays(monday, -7))}>
                <IconChevron size={15} className="rotate-180" />
              </IconButton>
              <Button size="sm" variant="ghost" onClick={() => goToWeek(null)} disabled={isCurrentWeek}>
                This week
              </Button>
              <IconButton label="Next week" onClick={() => goToWeek(addDays(monday, 7))}>
                <IconChevron size={15} />
              </IconButton>
            </div>
            <Button
              variant="primary"
              icon={<IconPlus size={15} />}
              onClick={() => setEditing({ event: null, day: isCurrentWeek ? todayKey : mondayKey })}
            >
              New event
            </Button>
          </>
        }
      />

      {error && <ErrorBanner message={error} onRetry={() => setReloadKey((k) => k + 1)} className="mb-4" />}
      {!stale && events.length >= WEEK_LIMIT && (
        <p className="mb-4 text-xs text-warn">Showing the first {WEEK_LIMIT} events in this range.</p>
      )}

      {firstLoad ? (
        <LoadingBlock label="Loading your week…" />
      ) : (
        <div
          className={cx(
            'grid overflow-hidden rounded-xl border border-line bg-line md:grid-cols-7',
            'gap-px transition-opacity',
            (loading || stale) && 'opacity-60',
          )}
        >
          {days.map((day) => {
            const key = localDayKey(day)
            const dayEvents = stale ? [] : (byDay.get(key) ?? [])
            const isToday = key === todayKey
            return (
              <section key={key} aria-label={day.toDateString()} className="group flex min-h-40 flex-col bg-surface md:min-h-80">
                <header className="flex items-center justify-between gap-1 px-2.5 pt-2.5 pb-1.5">
                  <div className="flex items-baseline gap-1.5">
                    <span className={cx('text-xs font-medium uppercase', isToday ? 'text-accent' : 'text-ink-faint')}>
                      {weekdayFmt.format(day)}
                    </span>
                    <span
                      className={cx(
                        'inline-flex size-6 items-center justify-center rounded-full text-sm',
                        isToday ? 'bg-accent font-semibold text-white dark:text-canvas' : 'text-ink',
                      )}
                    >
                      {day.getDate()}
                    </span>
                  </div>
                  <IconButton
                    label={`New event on ${day.toDateString()}`}
                    className="size-6 opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100"
                    onClick={() => setEditing({ event: null, day: key })}
                  >
                    <IconPlus size={13} />
                  </IconButton>
                </header>
                <ul className="flex flex-1 flex-col gap-1 px-1.5 pb-2">
                  {dayEvents.map((event) => (
                    <li key={event.id}>
                      <EventChip event={event} dayKey={key} onOpen={() => setEditing({ event, day: key })} />
                    </li>
                  ))}
                </ul>
              </section>
            )
          })}
        </div>
      )}

      {!loading && !stale && !error && visibleCount === 0 && (
        <div className="mt-6">
          <EmptyState title="Nothing scheduled this week" icon={<IconCalendar size={22} />}>
            Add an event here, or ask the assistant to schedule one — approved events show up on this page.
          </EmptyState>
        </div>
      )}

      <EventDialog
        open={editing !== null}
        event={editing?.event ?? null}
        defaultDay={editing?.day ?? todayKey}
        onClose={() => setEditing(null)}
        onSaved={upsert}
        onDeleted={(id) => setEvents((list) => list.filter((e) => e.id !== id))}
      />
    </PageContainer>
  )
}

function EventChip({ event, dayKey, onOpen }: { event: CalendarEvent; dayKey: string; onOpen: () => void }) {
  return (
    <button
      type="button"
      onClick={onOpen}
      className={cx(
        'w-full rounded-md px-2 py-1.5 text-left text-xs transition-colors',
        event.all_day
          ? 'bg-accent-soft text-accent hover:bg-accent/15'
          : 'border border-line bg-canvas text-ink hover:border-line-strong hover:bg-sunken',
      )}
    >
      {!event.all_day && <span className="block text-[11px] text-ink-faint tabular-nums">{timeLabelForDay(event, dayKey)}</span>}
      <span className="line-clamp-2 font-medium break-words">{event.title}</span>
      {event.location && <span className="block truncate text-[11px] text-ink-muted">{event.location}</span>}
      {(event.related_task_id || event.related_goal_id) && (
        <span className="mt-1 flex gap-1">
          {event.related_task_id && <Badge>Task</Badge>}
          {event.related_goal_id && <Badge>Goal</Badge>}
        </span>
      )}
    </button>
  )
}
