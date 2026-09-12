// Display helpers. Deadlines and target dates are treated as calendar dates in
// UTC, matching the backend, which resolves "friday" in UTC and stores a
// date-only deadline as midnight UTC. Formatting them in UTC keeps a date the
// user picked from rendering as the day before west of Greenwich.

const dateFmt = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' })
const dateFmtNoYear = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', timeZone: 'UTC' })
const stampFmt = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })

/** A calendar date ("15 Oct 2026") for a deadline or target date. */
export function formatDate(iso: string | null | undefined): string {
  if (!iso) return ''
  const d = new Date(iso)
  return d.getUTCFullYear() === new Date().getUTCFullYear() ? dateFmtNoYear.format(d) : dateFmt.format(d)
}

/** A local timestamp for created_at / updated_at. */
export function formatStamp(iso: string): string {
  return stampFmt.format(new Date(iso))
}

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })

export function relativeTime(iso: string): string {
  const seconds = Math.round((Date.parse(iso) - Date.now()) / 1000)
  const abs = Math.abs(seconds)
  if (abs < 45) return 'just now'
  if (abs < 3600) return rtf.format(Math.round(seconds / 60), 'minute')
  if (abs < 86400) return rtf.format(Math.round(seconds / 3600), 'hour')
  if (abs < 86400 * 30) return rtf.format(Math.round(seconds / 86400), 'day')
  return formatStamp(iso)
}

/** "yyyy-mm-dd" for an <input type="date">, from an RFC 3339 timestamp. */
export function toDateInput(iso: string | null | undefined): string {
  return iso ? iso.slice(0, 10) : ''
}

/** RFC 3339 midnight UTC from an <input type="date"> value, or null when empty. */
export function fromDateInput(value: string): string | null {
  return value ? `${value}T00:00:00Z` : null
}

/** Days until a UTC calendar date; negative when past. */
export function daysUntil(iso: string): number {
  const today = new Date()
  const start = Date.UTC(today.getFullYear(), today.getMonth(), today.getDate())
  const d = new Date(iso)
  const target = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate())
  return Math.round((target - start) / 86400000)
}

export function parseTags(value: string): string[] {
  return value
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean)
}

export function humanize(value: string): string {
  const s = value.replace(/_/g, ' ')
  return s.charAt(0).toUpperCase() + s.slice(1)
}

export function pct(value: number): string {
  return `${Math.round(value * 100)}%`
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`
}
