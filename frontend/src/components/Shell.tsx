// The signed-in frame: a quiet sidebar and the page beside it.
import { useEffect, useState, type ReactNode } from 'react'

import { useAuth, useUser } from '../lib/auth'
import { actions } from '../lib/endpoints'
import { onActionsChanged } from '../lib/events'
import { Link, useLocation } from '../lib/router'
import { IconBrain, IconCalendar, IconChat, IconFile, IconGraph, IconHome, IconInbox, IconLogout } from './icons'
import { cx } from './ui'

const NAV = [
  { to: '/', label: 'Today', icon: IconHome, match: (p: string) => p === '/' },
  { to: '/calendar', label: 'Calendar', icon: IconCalendar, match: (p: string) => p.startsWith('/calendar') },
  { to: '/chat', label: 'Assistant', icon: IconChat, match: (p: string) => p.startsWith('/chat') },
  { to: '/documents', label: 'Documents', icon: IconFile, match: (p: string) => p.startsWith('/documents') },
  { to: '/memories', label: 'Memories', icon: IconBrain, match: (p: string) => p.startsWith('/memories') },
  { to: '/approvals', label: 'Approvals', icon: IconInbox, match: (p: string) => p.startsWith('/approvals') },
  { to: '/graph', label: 'Connections', icon: IconGraph, match: (p: string) => p.startsWith('/graph') },
]

function usePendingCount(): number {
  const [count, setCount] = useState(0)
  useEffect(() => {
    let cancelled = false
    const load = () =>
      actions
        .list({ status: 'proposed', limit: 100 })
        .then((page) => !cancelled && setCount(page.count))
        .catch(() => undefined) // a badge is not worth an error banner
    load()
    const off = onActionsChanged(load)
    return () => {
      cancelled = true
      off()
    }
  }, [])
  return count
}

export function Mark({ className }: { className?: string }) {
  // A crescent: one disc with a second, canvas-coloured disc laid over it.
  return (
    <span className={cx('relative inline-block size-5 rounded-full bg-ink', className)} aria-hidden="true">
      <span className="absolute -top-0.5 left-1.5 size-4.5 rounded-full bg-canvas" />
    </span>
  )
}

export function Shell({ children }: { children: ReactNode }) {
  const { path } = useLocation()
  const user = useUser()
  const { logout } = useAuth()
  const pending = usePendingCount()

  return (
    <div className="flex h-full">
      <aside className="flex w-60 shrink-0 flex-col border-r border-line bg-canvas px-3 py-5">
        <Link to="/" className="mb-8 flex items-center gap-2.5 px-2.5">
          <Mark />
          <span className="font-display text-xl tracking-tight">Lunex</span>
        </Link>

        <nav className="flex flex-col gap-0.5" aria-label="Main">
          {NAV.map(({ to, label, icon: Icon, match }) => {
            const active = match(path)
            return (
              <Link
                key={to}
                to={to}
                aria-current={active ? 'page' : undefined}
                className={cx(
                  'flex items-center gap-3 rounded-lg px-2.5 py-2 text-sm transition-colors',
                  active ? 'bg-surface font-medium text-ink shadow-xs ring-1 ring-line' : 'text-ink-muted hover:bg-sunken hover:text-ink',
                )}
              >
                <Icon size={17} className={active ? 'text-accent' : ''} />
                <span className="flex-1">{label}</span>
                {to === '/approvals' && pending > 0 && (
                  <span className="rounded-full bg-warn px-1.5 text-[11px] leading-[18px] font-semibold text-white dark:text-canvas">
                    {pending}
                  </span>
                )}
              </Link>
            )
          })}
        </nav>

        <div className="mt-auto flex items-center gap-2.5 rounded-lg px-2.5 py-2">
          <span className="flex size-8 shrink-0 items-center justify-center rounded-full bg-accent-soft text-xs font-semibold text-accent">
            {(user.name || user.email).slice(0, 1).toUpperCase()}
          </span>
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm font-medium">{user.name || 'You'}</p>
            <p className="truncate text-xs text-ink-faint">{user.email}</p>
          </div>
          <button
            type="button"
            onClick={() => void logout()}
            className="rounded-md p-1.5 text-ink-faint hover:bg-sunken hover:text-ink"
            aria-label="Sign out"
            title="Sign out"
          >
            <IconLogout size={16} />
          </button>
        </div>
      </aside>

      <main className="min-w-0 flex-1 overflow-y-auto">{children}</main>
    </div>
  )
}

/** The standard page column. The chat screen opts out and fills the space; the calendar asks for a wider one. */
export function PageContainer({ children, wide }: { children: ReactNode; wide?: boolean }) {
  return <div className={cx('mx-auto w-full px-10 py-10', wide ? 'max-w-6xl' : 'max-w-4xl')}>{children}</div>
}
