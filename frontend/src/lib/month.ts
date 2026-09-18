// Month arithmetic for the spending screen.
//
// An expense_date is a bare calendar date -- "2026-09-17", with no time of day
// and no timezone -- so a month here is string arithmetic on "yyyy-mm" rather
// than a Date in somebody's zone. Turning the month into a Date and back is how
// the September a user in Auckland is looking at starts on the 31st of August.
//
// The API's `start` and `end` are inclusive on both ends, which is what makes a
// month exactly the 1st to the last: there is no 1st-of-the-next-month bound to
// get wrong.

const pad = (n: number) => String(n).padStart(2, '0')

/** "yyyy-mm" of the month the browser is in today. */
export function currentMonthKey(): string {
  const now = new Date()
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}`
}

/** "yyyy-mm-dd" of today, local: the day a person means by "today". */
export function todayKey(): string {
  const now = new Date()
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`
}

/** The month key a "?month=" carries, or null when it is not one. */
export function normalizeMonthKey(value: string | null | undefined): string | null {
  const m = /^\d{4}-(\d{2})$/.exec(value ?? '')
  if (!m) return null
  const month = Number(m[1])
  return month >= 1 && month <= 12 ? (value as string) : null
}

/** The month `n` months after `key`; negative counts back. */
export function shiftMonth(key: string, n: number): string {
  const [year, month] = key.split('-').map(Number)
  const d = new Date(Date.UTC(year, month - 1 + n, 1))
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}`
}

/** The inclusive day range the API takes for a whole month. */
export function monthRange(key: string): { start: string; end: string } {
  const [year, month] = key.split('-').map(Number)
  // Day 0 of the next month is the last day of this one.
  const last = new Date(Date.UTC(year, month, 0)).getUTCDate()
  return { start: `${key}-01`, end: `${key}-${pad(last)}` }
}

/** The month an expense_date falls in. */
export function monthOf(day: string): string {
  return day.slice(0, 7)
}

/** Where a new expense in this month starts: today, or the 1st of another month. */
export function defaultDayIn(key: string): string {
  const today = todayKey()
  return monthOf(today) === key ? today : `${key}-01`
}

const monthFmt = new Intl.DateTimeFormat(undefined, { month: 'long', year: 'numeric', timeZone: 'UTC' })
const dayFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short', day: 'numeric', month: 'short', timeZone: 'UTC' })

/** "September 2026". */
export function formatMonth(key: string): string {
  return monthFmt.format(new Date(`${key}-01T00:00:00Z`))
}

/**
 * "Thu 17 Sep". Formatted in UTC because the value is a date and not a moment:
 * read in local time it would be the day before, west of Greenwich.
 */
export function formatDay(day: string): string {
  return dayFmt.format(new Date(`${day}T00:00:00Z`))
}
