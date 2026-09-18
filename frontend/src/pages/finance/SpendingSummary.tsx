import { cx } from '../../components/ui'
import { plural } from '../../lib/format'
import { formatMoney } from '../../lib/money'
import type { CurrencyTotal, ExpenseSummary } from '../../lib/types'

/**
 * What the month came to, from GET /expenses/summary.
 *
 * One block per currency, because there is no total across them: adding ₹500 to
 * $20 needs a rate, a rate has a date, and a wrong one produces a plausible
 * number nobody can see is wrong. The API returns them largest first and this
 * renders them in that order.
 *
 * The bars are one length encoding of one measure, so they are one colour; the
 * number beside each is the answer and the bar is only the shape of it.
 */
export function SpendingSummary({ summary }: { summary: ExpenseSummary }) {
  return (
    <section aria-label="Spending this month" className="space-y-3">
      {summary.currencies.map((currency) => (
        <CurrencyBlock key={currency.currency} currency={currency} />
      ))}
      {summary.currencies.length > 1 && (
        <p className="text-xs text-ink-faint">
          Each currency is totalled on its own — nothing here converts between them.
        </p>
      )}
    </section>
  )
}

/**
 * A share that rounds to nothing is written "<1%" rather than "0%": a line that
 * is in the list has money on it, and printing zero beside a real amount reads
 * like a bug.
 */
function formatShare(share: number): string {
  if (share > 0 && share < 0.5) return '<1%'
  return `${Math.round(share)}%`
}

function CurrencyBlock({ currency }: { currency: CurrencyTotal }) {
  return (
    <div className="rounded-xl border border-line bg-surface px-5 py-4">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <div>
          <p className="text-xs font-medium tracking-wide text-ink-faint uppercase">Spent</p>
          <p className="font-display text-3xl tracking-tight text-ink tabular-nums">
            {formatMoney(currency.total, currency.currency)}
          </p>
        </div>
        <p className="text-xs text-ink-faint">
          {plural(currency.count, 'expense')} · {currency.currency}
        </p>
      </div>

      <ul className="mt-4 space-y-2.5">
        {currency.categories.map((line) => {
          // A total of zero is not reachable (every amount is > 0), but a share
          // is a division and this is the one place it could be by zero.
          const share = currency.total > 0 ? (line.total / currency.total) * 100 : 0
          return (
            <li key={line.category_id ?? 'none'}>
              <div className="flex items-baseline justify-between gap-3 text-sm">
                <span className={cx('min-w-0 truncate', line.category ? 'text-ink' : 'text-ink-muted italic')}>
                  {line.category ?? 'No category'}
                </span>
                <span className="shrink-0 text-ink tabular-nums">{formatMoney(line.total, currency.currency)}</span>
              </div>
              <div className="mt-1 flex items-center gap-2.5">
                <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-sunken" aria-hidden="true">
                  <div
                    className="h-full rounded-full bg-accent"
                    style={{ width: `${share}%`, minWidth: share > 0 ? '3px' : undefined }}
                  />
                </div>
                <span className="w-8 shrink-0 text-right text-[11px] text-ink-faint tabular-nums">
                  {formatShare(share)}
                </span>
              </div>
            </li>
          )
        })}
      </ul>
    </div>
  )
}
