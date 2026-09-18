import { useEffect, useState, type FormEvent } from 'react'

import { IconPlus, IconTrash } from '../../components/icons'
import { Button, ConfirmDialog, Dialog, ErrorBanner, Field, Input, Select, Textarea } from '../../components/ui'
import { ApiError, errorMessage } from '../../lib/api'
import { documents, expenseCategories, expenses } from '../../lib/endpoints'
import { formatStamp } from '../../lib/format'
import { amountError, currencyError, formatMoney, toAmountInput } from '../../lib/money'
import { Link } from '../../lib/router'
import type { Expense, ExpenseCategory, ExpenseInput } from '../../lib/types'

/** Record an expense (expense = null) or edit and delete an existing one. */
export function ExpenseDialog({
  open,
  expense,
  defaultDay,
  defaultCurrency,
  categories,
  onCategoryCreated,
  onClose,
  onSaved,
  onDeleted,
}: {
  open: boolean
  expense: Expense | null
  defaultDay: string
  defaultCurrency: string
  categories: ExpenseCategory[]
  onCategoryCreated: (category: ExpenseCategory) => void
  onClose: () => void
  onSaved: (expense: Expense) => void
  onDeleted: (id: string) => void
}) {
  const [confirming, setConfirming] = useState(false)

  return (
    <>
      <Dialog open={open} onClose={onClose} title={expense ? 'Edit expense' : 'New expense'}>
        {/* Keyed so reopening on another expense starts from that one's values. */}
        <ExpenseForm
          key={expense?.id ?? `new-${defaultDay}-${defaultCurrency}`}
          expense={expense}
          defaultDay={defaultDay}
          defaultCurrency={defaultCurrency}
          categories={categories}
          onCategoryCreated={onCategoryCreated}
          onCancel={onClose}
          onDelete={() => setConfirming(true)}
          onSubmit={async (input) => {
            const saved = expense ? await expenses.update(expense.id, input) : await expenses.create(createBody(input))
            onSaved(saved)
            onClose()
          }}
        />
      </Dialog>
      <ConfirmDialog
        open={confirming && expense !== null}
        onClose={() => setConfirming(false)}
        title="Delete expense?"
        confirmLabel="Delete expense"
        onConfirm={async () => {
          if (!expense) return
          await expenses.remove(expense.id)
          onDeleted(expense.id)
          onClose()
        }}
      >
        <p>
          {expense && formatMoney(expense.amount, expense.currency)}
          {expense?.description ? ` for “${expense.description}”` : ''} will be removed from your spending, and it will stop
          counting towards any total.
        </p>
      </ConfirmDialog>
    </>
  )
}

/**
 * A create body leaves empty optional fields out rather than sending null: the
 * endpoint rejects fields it does not know and reads an absent one as "not
 * given", which is what an unfiled expense with no note is.
 */
function createBody(input: ExpenseInput) {
  return {
    amount: input.amount,
    currency: input.currency,
    expense_date: input.expense_date,
    category_id: input.category_id ?? undefined,
    description: input.description ?? undefined,
  }
}

function ExpenseForm({
  expense,
  defaultDay,
  defaultCurrency,
  categories,
  onCategoryCreated,
  onSubmit,
  onCancel,
  onDelete,
}: {
  expense: Expense | null
  defaultDay: string
  defaultCurrency: string
  categories: ExpenseCategory[]
  onCategoryCreated: (category: ExpenseCategory) => void
  onSubmit: (input: ExpenseInput) => Promise<void>
  onCancel: () => void
  onDelete: () => void
}) {
  const [amount, setAmount] = useState(expense ? toAmountInput(expense.amount) : '')
  const [currency, setCurrency] = useState(expense?.currency ?? defaultCurrency)
  const [categoryId, setCategoryId] = useState<string | null>(expense?.category_id ?? null)
  const [day, setDay] = useState(expense?.expense_date ?? defaultDay)
  const [description, setDescription] = useState(expense?.description ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | string | null>(null)
  // Client-side checks first, then whatever the API said about the same field.
  const [fields, setFields] = useState<Record<string, string>>({})
  const fe = (f: string) => fields[f] ?? (error instanceof ApiError ? error.fieldError(f) : undefined)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const next: Record<string, string> = {}
    const badAmount = amountError(amount)
    if (badAmount) next.amount = badAmount
    const badCurrency = currencyError(currency)
    if (badCurrency) next.currency = badCurrency
    if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) next.expense_date = 'Pick the day it was spent.'
    setFields(next)
    if (Object.keys(next).length > 0) return

    setBusy(true)
    setError(null)
    try {
      // The amount goes as the string that was typed: the API parses it digit
      // by digit, so nothing here rounds it on the way.
      await onSubmit({
        amount: amount.trim(),
        currency: currency.trim().toUpperCase(),
        category_id: categoryId,
        description: description.trim() || null,
        expense_date: day,
      })
    } catch (err) {
      setError(err instanceof ApiError ? err : errorMessage(err))
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="space-y-3">
      <div className="grid grid-cols-[1fr_auto] gap-3">
        <Field label="Amount" error={fe('amount')}>
          {(id) => (
            <Input
              id={id}
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              inputMode="decimal"
              placeholder="450.50"
              maxLength={20}
              autoFocus
              required
            />
          )}
        </Field>
        <Field label="Currency" error={fe('currency')}>
          {(id) => (
            <Input
              id={id}
              className="w-24 uppercase"
              value={currency}
              onChange={(e) => setCurrency(e.target.value.toUpperCase())}
              maxLength={3}
              required
            />
          )}
        </Field>
      </div>

      <CategoryPicker
        value={categoryId}
        categories={categories}
        error={fe('category_id')}
        onChange={setCategoryId}
        onCreated={(created) => {
          onCategoryCreated(created)
          setCategoryId(created.id)
        }}
      />

      <Field label="Date" error={fe('expense_date')}>
        {(id) => <Input id={id} type="date" value={day} onChange={(e) => setDay(e.target.value)} required />}
      </Field>

      <Field label="Description" error={fe('description')}>
        {(id) => (
          <Textarea
            id={id}
            rows={2}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            maxLength={1000}
            placeholder="Optional — what the money was for"
          />
        )}
      </Field>

      {expense && <Related expense={expense} />}

      {error && <ErrorBanner message={typeof error === 'string' ? error : errorMessage(error)} />}

      <div className="flex items-center gap-2 pt-1">
        {expense && (
          <Button
            variant="ghost"
            className="text-danger hover:bg-danger-soft hover:text-danger"
            icon={<IconTrash size={14} />}
            onClick={onDelete}
          >
            Delete
          </Button>
        )}
        <div className="ml-auto flex gap-2">
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!amount.trim()}>
            {expense ? 'Save' : 'Add expense'}
          </Button>
        </div>
      </div>
    </form>
  )
}

const NEW_CATEGORY = '__new'

/**
 * The user's categories, plus a way to add one.
 *
 * Adding is offered here because an expense filed under nothing is money the
 * summary cannot explain. Renaming and deleting are not offered, because the
 * API does not have them: either would rewrite every expense already filed
 * under the category.
 */
function CategoryPicker({
  value,
  categories,
  error,
  onChange,
  onCreated,
}: {
  value: string | null
  categories: ExpenseCategory[]
  error?: string
  onChange: (id: string | null) => void
  onCreated: (category: ExpenseCategory) => void
}) {
  const [adding, setAdding] = useState(false)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [addError, setAddError] = useState<string | null>(null)

  const add = async () => {
    const trimmed = name.trim()
    if (!trimmed) return
    setBusy(true)
    setAddError(null)
    try {
      onCreated(await expenseCategories.create(trimmed))
      setAdding(false)
      setName('')
    } catch (e) {
      setAddError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-2">
      <Field label="Category" error={error}>
        {(id) => (
          <Select
            id={id}
            value={adding ? NEW_CATEGORY : (value ?? '')}
            onChange={(e) => {
              if (e.target.value === NEW_CATEGORY) {
                setAdding(true)
                return
              }
              setAdding(false)
              setAddError(null)
              onChange(e.target.value || null)
            }}
          >
            <option value="">No category</option>
            {categories.map((category) => (
              <option key={category.id} value={category.id}>
                {category.name}
              </option>
            ))}
            <option value={NEW_CATEGORY}>New category…</option>
          </Select>
        )}
      </Field>
      {adding && (
        <div className="flex items-center gap-2">
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              // Enter here adds the category; it must not submit the expense.
              if (e.key === 'Enter') {
                e.preventDefault()
                void add()
              }
            }}
            placeholder="Name, e.g. Books"
            maxLength={100}
            aria-label="New category name"
            autoFocus
          />
          <Button variant="secondary" icon={<IconPlus size={14} />} loading={busy} disabled={!name.trim()} onClick={add}>
            Add
          </Button>
          <Button
            variant="ghost"
            onClick={() => {
              setAdding(false)
              setAddError(null)
              setName('')
            }}
          >
            Cancel
          </Button>
        </div>
      )}
      {addError && <ErrorBanner message={addError} onDismiss={() => setAddError(null)} />}
    </div>
  )
}

/** What the expense is linked to, plus the facts the form does not edit. */
function Related({ expense }: { expense: Expense }) {
  const [filename, setFilename] = useState<string | null>(null)

  useEffect(() => {
    if (!expense.related_document_id) return
    let cancelled = false
    // A filename is a nicety: if the lookup fails the link still says what it is.
    documents.get(expense.related_document_id).then(
      (doc) => !cancelled && setFilename(doc.filename),
      () => undefined,
    )
    return () => {
      cancelled = true
    }
  }, [expense.related_document_id])

  return (
    <div className="space-y-1.5 rounded-lg bg-sunken px-3 py-2.5 text-xs text-ink-muted">
      {expense.related_document_id && (
        <p>
          Receipt:{' '}
          <Link to="/documents" className="font-medium text-accent hover:underline">
            {filename ?? 'view documents'}
          </Link>
          . The file is linked and nothing more — no amount is read out of it.
        </p>
      )}
      <p className="text-ink-faint">Last updated {formatStamp(expense.updated_at)}</p>
    </div>
  )
}
