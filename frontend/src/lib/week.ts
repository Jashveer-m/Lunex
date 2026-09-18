// Week arithmetic for the calendar screen.
//
// Two clocks are in play. A timed event is shown in the browser's local time.
// An all-day event is midnight to midnight UTC (what the API and the assistant's
// create_calendar_event tool store) and is shown by its UTC dates, the same rule
// lib/format.ts applies to deadlines -- otherwise a Thursday all-day event would
// straddle Wednesday and Thursday anywhere off Greenwich. Days are therefore
// keyed by "yyyy-mm-dd" strings, which both clocks can produce.
import type { CalendarEvent } from './types'

const DAY_MS = 86_400_000

const pad = (n: number) => String(n).padStart(2, '0')

/** "yyyy-mm-dd" of a Date in local time. */
export function localDayKey(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/** Local midnight of a "yyyy-mm-dd" key. */
export function parseDayKey(key: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(key)
  if (!m) return null
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]))
  return Number.isNaN(d.getTime()) || localDayKey(d) !== key ? null : d
}

export function addDays(d: Date, n: number): Date {
  // Calendar days, not 24h steps, so a DST change does not shift midnight.
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n)
}

/** Local midnight of the Monday on or before `d`. */
export function startOfWeek(d: Date): Date {
  const offset = (d.getDay() + 6) % 7
  return addDays(new Date(d.getFullYear(), d.getMonth(), d.getDate()), -offset)
}

/** Midnight UTC of the calendar date a local day key names. */
function utcMidnight(key: string): number {
  return Date.parse(`${key}T00:00:00Z`)
}

/**
 * The range to ask the API for: the seven local days, widened to also cover
 * the same seven UTC dates so all-day events at either edge are included.
 * Events outside the visible days are dropped when they are bucketed.
 */
export function weekWindow(monday: Date): { start: string; end: string } {
  const localStart = monday.getTime()
  const localEnd = addDays(monday, 7).getTime()
  const utcStart = utcMidnight(localDayKey(monday))
  const utcEnd = utcStart + 7 * DAY_MS
  return {
    start: new Date(Math.min(localStart, utcStart)).toISOString(),
    end: new Date(Math.max(localEnd, utcEnd)).toISOString(),
  }
}

/** The day keys an event occupies, in order. The interval is half-open. */
export function eventDayKeys(event: CalendarEvent): string[] {
  const s = Date.parse(event.start_time)
  const e = Date.parse(event.end_time)
  const keys: string[] = []
  if (event.all_day) {
    let day = utcMidnight(event.start_time.slice(0, 10))
    do {
      keys.push(new Date(day).toISOString().slice(0, 10))
      day += DAY_MS
    } while (day < e && keys.length < 366)
    return keys
  }
  let day = new Date(new Date(s).getFullYear(), new Date(s).getMonth(), new Date(s).getDate())
  do {
    keys.push(localDayKey(day))
    day = addDays(day, 1)
  } while (day.getTime() < e && keys.length < 366)
  return keys
}

const timeFmt = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' })

export function formatTime(iso: string): string {
  return timeFmt.format(new Date(iso))
}

/**
 * How a timed event reads in one day's column: "9:00 – 10:00", or with an
 * arrow when it started before this day or runs past it.
 */
export function timeLabelForDay(event: CalendarEvent, dayKey: string): string {
  const day = parseDayKey(dayKey)
  if (!day) return ''
  const s = Date.parse(event.start_time)
  const e = Date.parse(event.end_time)
  const startsHere = s >= day.getTime()
  const endsHere = e <= addDays(day, 1).getTime()
  if (startsHere && endsHere) {
    return s === e ? formatTime(event.start_time) : `${formatTime(event.start_time)} – ${formatTime(event.end_time)}`
  }
  if (startsHere) return `${formatTime(event.start_time)} →`
  if (endsHere) return `→ ${formatTime(event.end_time)}`
  return 'All day'
}

/** "14 – 20 Sep 2026", "29 Sep – 5 Oct 2026", "29 Dec 2026 – 4 Jan 2027". */
export function formatWeekRange(monday: Date): string {
  const sunday = addDays(monday, 6)
  const full = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', year: 'numeric' })
  if (monday.getFullYear() !== sunday.getFullYear()) return `${full.format(monday)} – ${full.format(sunday)}`
  const dayMonth = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short' })
  if (monday.getMonth() !== sunday.getMonth()) return `${dayMonth.format(monday)} – ${full.format(sunday)}`
  return `${monday.getDate()} – ${full.format(sunday)}`
}

// --- form conversions ------------------------------------------------------

/** "yyyy-mm-ddThh:mm" in local time, for <input type="datetime-local">. */
export function toLocalInput(iso: string): string {
  const d = new Date(iso)
  return `${localDayKey(d)}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** RFC 3339 (UTC) from a datetime-local value, or null when it does not parse. */
export function fromLocalInput(value: string): string | null {
  const t = new Date(value).getTime()
  return value && !Number.isNaN(t) ? new Date(t).toISOString() : null
}

/** Add minutes to a datetime-local value. */
export function shiftLocalInput(value: string, minutes: number): string {
  const t = new Date(value).getTime()
  return Number.isNaN(t) ? value : toLocalInput(new Date(t + minutes * 60_000).toISOString())
}

/** An all-day event's first and last date (inclusive), "yyyy-mm-dd". */
export function allDayDates(event: CalendarEvent): { first: string; last: string } {
  const first = event.start_time.slice(0, 10)
  const e = Date.parse(event.end_time)
  const last = e > Date.parse(event.start_time) ? new Date(e - 1).toISOString().slice(0, 10) : first
  return { first, last }
}

/** The stored interval for an all-day event spanning first..last inclusive. */
export function allDayInterval(first: string, last: string): { start_time: string; end_time: string } {
  return {
    start_time: `${first}T00:00:00Z`,
    end_time: new Date(utcMidnight(last) + DAY_MS).toISOString(),
  }
}
