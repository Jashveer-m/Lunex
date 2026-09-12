// Shared primitives. Kept to what the screens actually use; each one reads its
// colours from the tokens in index.css.
import {
  forwardRef,
  useEffect,
  useId,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from 'react'

import { IconAlert, IconRefresh, IconX } from './icons'

export function cx(...classes: (string | false | null | undefined)[]) {
  return classes.filter(Boolean).join(' ')
}

// --- buttons ---------------------------------------------------------------

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger' | 'accent'
  size?: 'sm' | 'md'
  loading?: boolean
  icon?: ReactNode
}

const variants: Record<NonNullable<ButtonProps['variant']>, string> = {
  primary: 'bg-ink text-canvas hover:bg-ink/85 shadow-sm',
  accent: 'bg-accent text-white hover:bg-accent/90 shadow-sm dark:text-canvas',
  secondary: 'bg-surface text-ink border border-line-strong hover:bg-sunken shadow-xs',
  ghost: 'text-ink-muted hover:text-ink hover:bg-sunken',
  danger: 'bg-danger text-white hover:bg-danger/90 shadow-sm dark:text-canvas',
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'secondary', size = 'md', loading, icon, disabled, className, children, type = 'button', ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || loading}
      className={cx(
        'inline-flex items-center justify-center gap-1.5 rounded-lg font-medium whitespace-nowrap transition-colors',
        'disabled:cursor-not-allowed disabled:opacity-50',
        size === 'sm' ? 'h-7 px-2.5 text-xs' : 'h-9 px-3.5 text-sm',
        variants[variant],
        className,
      )}
      {...rest}
    >
      {loading ? <Spinner size={size === 'sm' ? 12 : 14} /> : icon}
      {children}
    </button>
  )
})

export function IconButton({
  label,
  className,
  children,
  tone = 'default',
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { label: string; tone?: 'default' | 'danger' }) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      className={cx(
        'inline-flex size-7 shrink-0 items-center justify-center rounded-md text-ink-faint transition-colors disabled:opacity-40',
        tone === 'danger' ? 'hover:bg-danger-soft hover:text-danger' : 'hover:bg-sunken hover:text-ink',
        className,
      )}
      {...rest}
    >
      {children}
    </button>
  )
}

// --- form controls ---------------------------------------------------------

// Controls fill their container unless the caller sizes them: a class string
// holding both w-full and w-28 would be decided by stylesheet order, not intent.
const width = (className?: string) => (/(^|\s)(w-|flex-1)/.test(className ?? '') ? '' : 'w-full')

const control =
  'rounded-lg border border-line-strong bg-surface px-3 text-sm text-ink placeholder:text-ink-faint shadow-xs transition-colors focus:border-accent focus:outline-none focus:ring-3 focus:ring-accent/15 disabled:opacity-60 aria-invalid:border-danger'

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(function Input(
  { className, ...rest },
  ref,
) {
  return <input ref={ref} className={cx(control, width(className), 'h-9', className)} {...rest} />
})

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(function Textarea(
  { className, ...rest },
  ref,
) {
  return <textarea ref={ref} className={cx(control, width(className), 'py-2 leading-relaxed', className)} {...rest} />
})

export function Select({ className, children, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select className={cx(control, width(className), 'h-9 cursor-pointer pr-8', className)} {...rest}>
      {children}
    </select>
  )
}

/** A label, a control and its error. The control is passed the generated id. */
export function Field({
  label,
  hint,
  error,
  children,
  className,
}: {
  label: string
  hint?: string
  error?: string
  children: (id: string) => ReactNode
  className?: string
}) {
  const id = useId()
  return (
    <div className={cx('flex flex-col gap-1.5', className)}>
      <label htmlFor={id} className="text-xs font-medium text-ink-muted">
        {label}
      </label>
      {children(id)}
      {error ? (
        <p className="text-xs text-danger">{error}</p>
      ) : hint ? (
        <p className="text-xs text-ink-faint">{hint}</p>
      ) : null}
    </div>
  )
}

export function Toggle({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean
  onChange: (next: boolean) => void
  label: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cx(
        'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors disabled:opacity-50',
        checked ? 'bg-accent' : 'bg-line-strong',
      )}
    >
      <span
        className={cx(
          'inline-block size-4 rounded-full bg-white shadow transition-transform',
          checked ? 'translate-x-4.5' : 'translate-x-0.5',
        )}
      />
    </button>
  )
}

// --- feedback --------------------------------------------------------------

export function Spinner({ size = 16, className }: { size?: number; className?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={cx('animate-spin', className)}
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" strokeWidth="2.5" opacity="0.2" />
      <path d="M21 12a9 9 0 0 0-9-9" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" />
    </svg>
  )
}

export function LoadingBlock({ label = 'Loading…' }: { label?: string }) {
  return (
    <div role="status" className="flex items-center justify-center gap-2 py-16 text-sm text-ink-faint">
      <Spinner />
      {label}
    </div>
  )
}

export function ErrorBanner({
  message,
  onRetry,
  onDismiss,
  className,
}: {
  message: string
  onRetry?: () => void
  onDismiss?: () => void
  className?: string
}) {
  return (
    <div
      role="alert"
      className={cx(
        'flex items-start gap-2.5 rounded-lg border border-danger/25 bg-danger-soft px-3.5 py-2.5 text-sm text-danger',
        className,
      )}
    >
      <IconAlert size={16} className="mt-0.5 shrink-0" />
      <p className="min-w-0 flex-1 break-words">{message}</p>
      {onRetry && (
        <button type="button" onClick={onRetry} className="inline-flex items-center gap-1 font-medium hover:underline">
          <IconRefresh size={13} /> Retry
        </button>
      )}
      {onDismiss && (
        <button type="button" onClick={onDismiss} aria-label="Dismiss" className="opacity-70 hover:opacity-100">
          <IconX size={14} />
        </button>
      )}
    </div>
  )
}

export function EmptyState({ title, children, icon }: { title: string; children?: ReactNode; icon?: ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center rounded-xl border border-dashed border-line-strong px-6 py-14 text-center">
      {icon && <div className="mb-3 text-ink-faint">{icon}</div>}
      <p className="font-display text-lg text-ink">{title}</p>
      {children && <div className="mt-1 max-w-sm text-sm text-ink-muted">{children}</div>}
    </div>
  )
}

type Tone = 'neutral' | 'accent' | 'ok' | 'warn' | 'danger'
const tones: Record<Tone, string> = {
  neutral: 'bg-sunken text-ink-muted border-line',
  accent: 'bg-accent-soft text-accent border-accent/20',
  ok: 'bg-ok-soft text-ok border-ok/20',
  warn: 'bg-warn-soft text-warn border-warn/25',
  danger: 'bg-danger-soft text-danger border-danger/20',
}

export function Badge({ tone = 'neutral', children, className }: { tone?: Tone; children: ReactNode; className?: string }) {
  return (
    <span
      className={cx(
        'inline-flex items-center gap-1 rounded-md border px-1.5 py-px text-[11px] font-medium leading-4 whitespace-nowrap',
        tones[tone],
        className,
      )}
    >
      {children}
    </span>
  )
}

// --- layout ----------------------------------------------------------------

export function PageHeader({ title, description, actions }: { title: string; description?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="mb-8 flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        <h1 className="font-display text-3xl tracking-tight text-ink">{title}</h1>
        {description && <p className="mt-1.5 max-w-xl text-sm text-ink-muted">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </header>
  )
}

export function Tabs<T extends string>({
  tabs,
  value,
  onChange,
}: {
  tabs: { value: T; label: string; count?: number }[]
  value: T
  onChange: (value: T) => void
}) {
  return (
    <div role="tablist" className="mb-6 flex gap-1 border-b border-line">
      {tabs.map((tab) => (
        <button
          key={tab.value}
          role="tab"
          type="button"
          aria-selected={tab.value === value}
          onClick={() => onChange(tab.value)}
          className={cx(
            '-mb-px border-b-2 px-3 pb-2.5 text-sm font-medium transition-colors',
            tab.value === value ? 'border-ink text-ink' : 'border-transparent text-ink-faint hover:text-ink-muted',
          )}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}

export function LoadMore({ hasMore, loading, onClick }: { hasMore: boolean; loading: boolean; onClick: () => void }) {
  if (!hasMore) return null
  return (
    <div className="flex justify-center pt-4">
      <Button variant="ghost" size="sm" loading={loading} onClick={onClick}>
        Load more
      </Button>
    </div>
  )
}

// --- dialog ----------------------------------------------------------------

/**
 * A modal built on <dialog>, which gives focus trapping, Escape and the
 * backdrop for free.
 */
export function Dialog({
  open,
  onClose,
  title,
  children,
  footer,
  wide,
}: {
  open: boolean
  onClose: () => void
  title: string
  children: ReactNode
  footer?: ReactNode
  wide?: boolean
}) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    if (open && !el.open) el.showModal()
    if (!open && el.open) el.close()
  }, [open])
  return (
    <dialog
      ref={ref}
      onClose={onClose}
      onClick={(e) => e.target === ref.current && onClose()}
      className={cx(
        'm-auto w-[calc(100%-2rem)] rounded-2xl border border-line bg-surface p-0 text-ink shadow-2xl',
        wide ? 'max-w-3xl' : 'max-w-md',
      )}
    >
      {open && (
        <div className="flex max-h-[85vh] flex-col">
          <div className="flex items-center justify-between gap-4 border-b border-line px-5 py-4">
            <h2 className="font-display text-xl">{title}</h2>
            <IconButton label="Close" onClick={onClose}>
              <IconX />
            </IconButton>
          </div>
          <div className="min-h-0 overflow-y-auto px-5 py-4 text-sm text-ink-muted">{children}</div>
          {footer && <div className="flex justify-end gap-2 border-t border-line px-5 py-3">{footer}</div>}
        </div>
      )}
    </dialog>
  )
}

/**
 * Ask before a destructive call. `onConfirm` may be async; while it runs the
 * button spins, and if it throws the error is shown in the dialog rather than
 * the dialog closing as if it had worked.
 */
export function ConfirmDialog({
  open,
  onClose,
  title,
  children,
  confirmLabel,
  onConfirm,
}: {
  open: boolean
  onClose: () => void
  title: string
  children: ReactNode
  confirmLabel: string
  onConfirm: () => Promise<void>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (open) setError(null)
  }, [open])
  const confirm = async () => {
    setBusy(true)
    setError(null)
    try {
      await onConfirm()
      onClose()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Something went wrong.')
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open={open}
      onClose={() => !busy && onClose()}
      title={title}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="danger" onClick={confirm} loading={busy}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        {children}
        {error && <ErrorBanner message={error} />}
      </div>
    </Dialog>
  )
}
