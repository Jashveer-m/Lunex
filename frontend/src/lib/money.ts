// Money at the edge of the spending screen.
//
// The API sends an amount as an unquoted JSON number with exactly two decimal
// places -- backend/internal/finance/amount.go holds it as an integer number of
// hundredths and never as a float, for reasons it states at length. JSON.parse
// turns that into a float here, so nothing in this file adds two amounts
// together: every total on the screen is one GET /expenses/summary worked out
// in Postgres, and what arrives is only ever formatted. An amount going the
// other way is sent as the decimal string the user typed, which the API parses
// digit by digit rather than through a float of its own.

const formatters = new Map<string, Intl.NumberFormat>()

function formatterFor(currency: string): Intl.NumberFormat {
  const cached = formatters.get(currency)
  if (cached) return cached
  let format: Intl.NumberFormat
  try {
    format = new Intl.NumberFormat(undefined, {
      style: 'currency',
      currency,
      currencyDisplay: 'narrowSymbol',
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    })
  } catch {
    // The API does not check a currency against a list of real ones -- it only
    // groups by the code -- so Intl can be handed one it refuses.
    format = new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
  }
  formatters.set(currency, format)
  return format
}

/**
 * "₹450.50". Always two decimal places, whatever the currency's own convention
 * is: the column stores hundredths for every code, so a zero-decimal currency
 * reads back as it was written rather than rounded to the nearest unit on the
 * way to the screen.
 */
export function formatMoney(amount: number, currency: string): string {
  const format = formatterFor(currency)
  const text = format.format(amount)
  return format.resolvedOptions().style === 'currency' ? text : `${currency} ${text}`
}

/** The decimal string an <input> starts from, for an amount the API sent. */
export function toAmountInput(amount: number): string {
  return amount.toFixed(2)
}

/**
 * What the amount field accepts, kept in step with the API's parser: digits,
 * optional grouping commas, at most two decimal places.
 *
 * More than two places is refused rather than rounded -- the column cannot hold
 * 12.345, and quietly storing 12.35 is an edit to the ledger nobody asked for.
 * A comma only ever groups thousands, never marks the decimal point, because
 * "12,34" is twelve-and-a-bit to one half of the world and twelve thousand to
 * the other; the ambiguous case is refused rather than guessed.
 *
 * Returns the message to show, or null when the value is one the API will take.
 */
export function amountError(raw: string): string | null {
  const value = raw.trim()
  if (!value) return 'Enter an amount.'
  if (/\.\d{3,}$/.test(value)) return 'An amount has at most two decimal places.'
  const grouped = /^\d{1,3}(,\d{3})+(\.\d{1,2})?$/
  const plain = /^\d+(\.\d{1,2})?$/
  if (!plain.test(value) && !grouped.test(value)) {
    return value.includes(',')
      ? 'A comma only groups thousands — use a full stop for the decimal point.'
      : 'Enter an amount like 450.50.'
  }
  if (Number(value.replace(/,/g, '')) <= 0) return 'An amount must be greater than zero.'
  return null
}

/** Three letters, which is all the API checks: it groups by the code, nothing more. */
export function currencyError(raw: string): string | null {
  const value = raw.trim()
  if (!value) return 'Enter a currency code.'
  return /^\p{L}{3}$/u.test(value) ? null : 'A currency is a three-letter code, such as INR.'
}
