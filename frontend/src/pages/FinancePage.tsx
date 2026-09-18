import { useCallback, useEffect, useMemo, useState } from 'react'

import { PageContainer } from '../components/Shell'
import { IconChevron, IconPlus, IconWallet } from '../components/icons'
import {
  Badge,
  Button,
  EmptyState,
  ErrorBanner,
  IconButton,
  LoadMore,
  LoadingBlock,
  PageHeader,
  cx,
} from '../components/ui'
import { errorMessage, isAbort } from '../lib/api'
import { expenseCategories, expenses as expensesApi } from '../lib/endpoints'
import { onActionsChanged } from '../lib/events'
import { plural } from '../lib/format'
import {
  currentMonthKey,
  defaultDayIn,
  formatDay,
  formatMonth,
  monthOf,
  monthRange,
  normalizeMonthKey,
  shiftMonth,
} from '../lib/month'
import { formatMoney } from '../lib/money'
import { navigate, useLocation } from '../lib/router'
import type { Expense, ExpenseCategory, ExpenseSummary } from '../lib/types'
import { usePaged } from '../lib/usePaged'
import { ExpenseDialog } from './finance/ExpenseDialog'
import { SpendingSummary } from './finance/SpendingSummary'

// What the API defaults an unspecified currency to (migration 000009's column
// default). Only reached by an account that has never recorded anything.
const FALLBACK_CURRENCY = 'INR'

export function FinancePage() {
  const { query } = useLocation()
  const month = normalizeMonthKey(query.get('month')) ?? currentMonthKey()
  const range = useMemo(() => monthRange(month), [month])
  const thisMonth = currentMonthKey()

  // Bumped when the assistant may have written an expense; folded into the
  // list's key and the summary's dependencies so both re-read.
  const [reloadKey, setReloadKey] = useState(0)
  const [editing, setEditing] = useState<{ expense: Expense | null } | null>(null)

  const list = usePaged(
    (limit, offset) =>
      expensesApi.list({ ...range, limit, offset }).then((page) => ({ items: page.expenses, count: page.count })),
    `${month}:${reloadKey}`,
  )

  // --- the month's totals ---------------------------------------------------
  // Its own request rather than a sum of the rows above: the summary covers the
  // whole month, and the list is one page of it.
  const [summary, setSummary] = useState<{ month: string; data: ExpenseSummary } | null>(null)
  const [summaryLoading, setSummaryLoading] = useState(true)
  const [summaryError, setSummaryError] = useState<string | null>(null)
  const [summaryKey, setSummaryKey] = useState(0)
  const refreshSummary = useCallback(() => setSummaryKey((k) => k + 1), [])

  useEffect(() => {
    const controller = new AbortController()
    setSummaryLoading(true)
    setSummaryError(null)
    expensesApi
      .summary(range, controller.signal)
      .then((data) => setSummary({ month, data }))
      .catch((err) => !isAbort(err) && setSummaryError(errorMessage(err)))
      .finally(() => !controller.signal.aborted && setSummaryLoading(false))
    return () => controller.abort()
  }, [month, range, summaryKey, reloadKey])

  // --- categories -----------------------------------------------------------
  const [categories, setCategories] = useState<ExpenseCategory[]>([])
  const [categoriesError, setCategoriesError] = useState<string | null>(null)
  const [categoriesKey, setCategoriesKey] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    setCategoriesError(null)
    expenseCategories
      .list(controller.signal)
      .then((page) => setCategories(page.categories))
      .catch((err) => !isAbort(err) && setCategoriesError(errorMessage(err)))
    return () => controller.abort()
  }, [categoriesKey])

  // --- the currency a new expense starts in ---------------------------------
  // "From existing entries": what was last spent in this month, else what was
  // last spent at all, else the column default. Nothing converts, so this is
  // only a guess at which pile the next expense joins.
  const [lastCurrency, setLastCurrency] = useState<string | null>(null)
  useEffect(() => {
    const controller = new AbortController()
    expensesApi
      .list({ limit: 1 }, controller.signal)
      .then((page) => page.expenses[0] && setLastCurrency(page.expenses[0].currency))
      .catch(() => undefined) // a default is not worth an error banner
    return () => controller.abort()
  }, [reloadKey])
  const defaultCurrency = list.items[0]?.currency ?? lastCurrency ?? FALLBACK_CURRENCY

  // An expense the assistant recorded through an approval should appear without
  // a manual refresh.
  useEffect(() => onActionsChanged(() => setReloadKey((k) => k + 1)), [])

  const goToMonth = useCallback((key: string) => {
    navigate(key === currentMonthKey() ? '/finance' : `/finance?month=${key}`, { replace: true })
  }, [])

  const upsert = (saved: Expense) => {
    if (monthOf(saved.expense_date) !== month) {
      // It was dated into another month. Go there rather than leave the screen
      // looking as though nothing happened.
      goToMonth(monthOf(saved.expense_date))
      return
    }
    list.setItems((items) => sortExpenses([saved, ...items.filter((e) => e.id !== saved.id)]))
    refreshSummary()
  }

  const remove = (id: string) => {
    list.setItems((items) => items.filter((e) => e.id !== id))
    refreshSummary()
  }

  const stale = summary?.month !== month
  const isThisMonth = month === thisMonth
  const monthCount = stale ? null : (summary?.data.count ?? null)

  return (
    <PageContainer>
      <PageHeader
        title="Spending"
        description={
          <>
            {formatMonth(month)}
            {monthCount !== null && <span className="text-ink-faint"> · {plural(monthCount, 'expense')}</span>}
          </>
        }
        actions={
          <>
            <div className="flex items-center gap-1">
              <IconButton label="Previous month" onClick={() => goToMonth(shiftMonth(month, -1))}>
                <IconChevron size={15} className="rotate-180" />
              </IconButton>
              <Button size="sm" variant="ghost" onClick={() => goToMonth(thisMonth)} disabled={isThisMonth}>
                This month
              </Button>
              <IconButton label="Next month" onClick={() => goToMonth(shiftMonth(month, 1))}>
                <IconChevron size={15} />
              </IconButton>
            </div>
            <Button variant="primary" icon={<IconPlus size={15} />} onClick={() => setEditing({ expense: null })}>
              New expense
            </Button>
          </>
        }
      />

      {categoriesError && (
        <ErrorBanner
          message={`Couldn’t load your categories. ${categoriesError}`}
          onRetry={() => setCategoriesKey((k) => k + 1)}
          className="mb-4"
        />
      )}

      {summaryError ? (
        <ErrorBanner message={summaryError} onRetry={refreshSummary} className="mb-6" />
      ) : summaryLoading && stale ? (
        <LoadingBlock label="Adding up the month…" />
      ) : summary && !stale && summary.data.currencies.length > 0 ? (
        <div className={cx('mb-8 transition-opacity', summaryLoading && 'opacity-60')}>
          <SpendingSummary summary={summary.data} />
        </div>
      ) : null}

      {list.error && <ErrorBanner message={list.error} onRetry={list.reload} className="mb-4" />}

      {list.loading && list.items.length === 0 ? (
        <LoadingBlock label="Loading expenses…" />
      ) : list.items.length === 0 ? (
        !list.error && (
          <EmptyState title={`Nothing recorded in ${formatMonth(month)}`} icon={<IconWallet size={22} />}>
            Add an expense here, or tell the assistant what you spent — an approved expense shows up on this page.
          </EmptyState>
        )
      ) : (
        <>
          <ul
            className={cx(
              'divide-y divide-line overflow-hidden rounded-xl border border-line bg-surface transition-opacity',
              list.loading && 'opacity-60',
            )}
          >
            {list.items.map((expense) => (
              <ExpenseRow key={expense.id} expense={expense} onOpen={() => setEditing({ expense })} />
            ))}
          </ul>
          <LoadMore hasMore={list.hasMore} loading={list.loadingMore} onClick={list.loadMore} />
        </>
      )}

      <ExpenseDialog
        open={editing !== null}
        expense={editing?.expense ?? null}
        defaultDay={defaultDayIn(month)}
        defaultCurrency={defaultCurrency}
        categories={categories}
        onCategoryCreated={(created) =>
          setCategories((all) =>
            [...all.filter((c) => c.id !== created.id), created].sort((a, b) =>
              a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }),
            ),
          )
        }
        onClose={() => setEditing(null)}
        onSaved={upsert}
        onDeleted={remove}
      />
    </PageContainer>
  )
}

/** The API's default order: newest day first, and stable within a day. */
function sortExpenses(items: Expense[]): Expense[] {
  return [...items].sort((a, b) => b.expense_date.localeCompare(a.expense_date) || a.id.localeCompare(b.id))
}

function ExpenseRow({ expense, onOpen }: { expense: Expense; onOpen: () => void }) {
  return (
    <li>
      <button
        type="button"
        onClick={onOpen}
        className="flex w-full items-center gap-3 px-4 py-3 text-left transition-colors hover:bg-sunken"
      >
        <span className="w-24 shrink-0 text-xs text-ink-faint tabular-nums">{formatDay(expense.expense_date)}</span>
        <span className={cx('min-w-0 flex-1 truncate text-sm', expense.description ? 'text-ink' : 'text-ink-faint')}>
          {expense.description || 'No description'}
        </span>
        {expense.related_document_id && <Badge tone="accent">Receipt</Badge>}
        <Badge>
          <span className={cx('block max-w-40 truncate', expense.category ? undefined : 'italic')}>
            {expense.category ?? 'No category'}
          </span>
        </Badge>
        <span className="w-32 shrink-0 text-right text-sm font-medium text-ink tabular-nums">
          {formatMoney(expense.amount, expense.currency)}
        </span>
      </button>
    </li>
  )
}
