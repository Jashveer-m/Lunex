// A deliberately tiny router: the History API, one hook, one Link. The app
// has a handful of flat routes and no nested layouts, which does not justify a
// routing dependency.
import { useSyncExternalStore, type AnchorHTMLAttributes, type MouseEvent } from 'react'

const listeners = new Set<() => void>()

function subscribe(listener: () => void) {
  listeners.add(listener)
  window.addEventListener('popstate', listener)
  return () => {
    listeners.delete(listener)
    window.removeEventListener('popstate', listener)
  }
}

const snapshot = () => window.location.pathname + window.location.search

export function navigate(to: string, opts: { replace?: boolean } = {}) {
  if (to === snapshot()) return
  if (opts.replace) window.history.replaceState(null, '', to)
  else window.history.pushState(null, '', to)
  listeners.forEach((l) => l())
}

export type Location = { path: string; query: URLSearchParams }

export function useLocation(): Location {
  const href = useSyncExternalStore(subscribe, snapshot)
  const url = new URL(href, window.location.origin)
  return { path: url.pathname, query: url.searchParams }
}

/** Match "/chat/:id" style patterns. Returns the params, or null. */
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split('/').filter(Boolean)
  const s = path.split('/').filter(Boolean)
  if (p.length !== s.length) return null
  const params: Record<string, string> = {}
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(':')) params[p[i].slice(1)] = decodeURIComponent(s[i])
    else if (p[i] !== s[i]) return null
  }
  return params
}

export function Link({ to, onClick, ...rest }: AnchorHTMLAttributes<HTMLAnchorElement> & { to: string }) {
  const handle = (event: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(event)
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return
    }
    event.preventDefault()
    navigate(to)
  }
  return <a href={to} onClick={handle} {...rest} />
}
