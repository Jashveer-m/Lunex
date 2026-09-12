// Fetch wrapper for the Lunex API, with the session behind it.
//
// Token storage (see docs/decisions.md, "Refresh tokens are returned in the
// JSON body"):
//   - the access token lives only in this module's memory, so a page reload
//     drops it and nothing persistent ever holds a bearer credential;
//   - the refresh token lives in localStorage so a reload can mint a new access
//     token. That makes it readable by any script running on this origin -- an
//     XSS bug would leak a 30-day credential. An HttpOnly cookie is the fix and
//     needs a backend change, which this phase does not make.
//
// Refresh tokens rotate on every use and the previous value dies at once, so
// two refreshes racing with the same token log the user out. Refreshing is
// therefore single-flight within a tab, and serialised across tabs with the Web
// Locks API where the browser has it: each refresh reads the latest token from
// storage only after taking the lock.
import type { TokenObject } from './types'

const BASE_URL = import.meta.env.VITE_API_BASE_URL ?? ''
const REFRESH_KEY = 'lunex.refresh_token'
// Refresh a little before the server would reject the token, so an ordinary
// request does not have to fail once first.
const EXPIRY_SKEW_MS = 15_000

export type FieldError = { field: string; message: string }

export class ApiError extends Error {
  status: number
  code: string
  fields: FieldError[]
  retryAfter: number | null

  constructor(status: number, code: string, message: string, fields: FieldError[] = [], retryAfter: number | null = null) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.fields = fields
    this.retryAfter = retryAfter
  }

  fieldError(field: string): string | undefined {
    return this.fields.find((f) => f.field === field)?.message
  }
}

// --- token store -----------------------------------------------------------

let access: { token: string; expiresAt: number } | null = null
let refreshing: Promise<void> | null = null
const expiredListeners = new Set<() => void>()

export function readRefreshToken(): string | null {
  try {
    return localStorage.getItem(REFRESH_KEY)
  } catch {
    return null
  }
}

export function hasStoredSession(): boolean {
  return readRefreshToken() !== null
}

export function storeTokens(tokens: TokenObject) {
  access = { token: tokens.access_token, expiresAt: Date.parse(tokens.access_token_expires_at) }
  try {
    localStorage.setItem(REFRESH_KEY, tokens.refresh_token)
  } catch {
    // Storage blocked: the session still works until the tab is reloaded.
  }
}

export function clearTokens() {
  access = null
  try {
    localStorage.removeItem(REFRESH_KEY)
  } catch {
    // nothing to clear
  }
}

/** Called when the session is gone for good (the refresh token was refused). */
export function onSessionExpired(listener: () => void): () => void {
  expiredListeners.add(listener)
  return () => expiredListeners.delete(listener)
}

/** Called when another tab logs out, which removes the shared refresh token. */
export function onStorageLogout(listener: () => void): () => void {
  const handler = (event: StorageEvent) => {
    if (event.key === REFRESH_KEY && event.newValue === null) {
      access = null
      listener()
    }
  }
  window.addEventListener('storage', handler)
  return () => window.removeEventListener('storage', handler)
}

async function doRefresh(): Promise<void> {
  const refreshToken = readRefreshToken()
  if (!refreshToken) {
    access = null
    throw new ApiError(401, 'session_expired', 'Your session has ended. Please sign in again.')
  }
  const response = await fetch(`${BASE_URL}/api/v1/auth/refresh`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: refreshToken }),
  })
  if (response.status === 401 || response.status === 400) {
    // The token is dead (expired, rotated elsewhere, logged out). Only this
    // answer ends the session -- a network failure or a 5xx leaves it alone
    // so a backend restart does not sign everybody out.
    clearTokens()
    expiredListeners.forEach((l) => l())
    throw new ApiError(401, 'session_expired', 'Your session has expired. Please sign in again.')
  }
  if (!response.ok) throw await toApiError(response)
  storeTokens((await response.json()) as TokenObject)
}

/** Mint a new access token from the stored refresh token. Safe to call concurrently. */
export function refreshSession(): Promise<void> {
  if (!refreshing) {
    const run = () => doRefresh()
    const locked =
      typeof navigator !== 'undefined' && navigator.locks
        ? navigator.locks.request('lunex.refresh', run)
        : run()
    refreshing = locked.finally(() => {
      refreshing = null
    })
  }
  return refreshing
}

// --- requests --------------------------------------------------------------

type RequestOptions = {
  method?: string
  body?: unknown
  signal?: AbortSignal
  /** Send no Authorization header and never refresh (login, register). */
  anonymous?: boolean
}

async function toApiError(response: Response): Promise<ApiError> {
  const body = await response.json().catch(() => null)
  const retryAfter = Number(response.headers.get('Retry-After')) || null
  if (!body && response.status >= 500) {
    // Not the API's JSON error shape: a proxy answering for a backend that is
    // down (the Vite dev proxy says 500 with an empty body).
    return new ApiError(
      response.status,
      'network_error',
      `The Lunex API is not responding (HTTP ${response.status}). Is the backend running?`,
    )
  }
  return new ApiError(
    response.status,
    body?.error ?? 'unknown_error',
    body?.message ?? (response.statusText || `Request failed (${response.status})`),
    Array.isArray(body?.fields) ? body.fields : [],
    retryAfter,
  )
}

function buildInit(opts: RequestOptions): RequestInit {
  const headers: Record<string, string> = {}
  let body: BodyInit | undefined
  if (opts.body instanceof FormData) {
    body = opts.body // the browser sets the multipart boundary itself
  } else if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(opts.body)
  }
  if (!opts.anonymous && access) headers.Authorization = `Bearer ${access.token}`
  return { method: opts.method ?? 'GET', headers, body, signal: opts.signal }
}

/**
 * Perform a request and return the raw Response. Authenticated requests refresh
 * an expiring access token first, and on a 401 refresh once and retry -- a 401
 * means the server did not act on the request, so repeating it is safe even for
 * a POST.
 */
export async function authedFetch(path: string, opts: RequestOptions = {}): Promise<Response> {
  const url = `${BASE_URL}${path}`
  if (opts.anonymous) return fetch(url, buildInit(opts))

  if (!access || access.expiresAt - Date.now() < EXPIRY_SKEW_MS) {
    await refreshSession()
  }
  let response = await fetch(url, buildInit(opts))
  if (response.status === 401) {
    await refreshSession()
    response = await fetch(url, buildInit(opts))
  }
  return response
}

export async function apiFetch<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  let response: Response
  try {
    response = await authedFetch(path, opts)
  } catch (error) {
    if (error instanceof ApiError || (error instanceof DOMException && error.name === 'AbortError')) throw error
    throw new ApiError(0, 'network_error', 'Could not reach the Lunex API. Is the backend running?')
  }
  if (!response.ok) throw await toApiError(response)
  return response.status === 204 ? (undefined as T) : ((await response.json()) as T)
}

export { toApiError }

/** A human sentence for any thrown value. */
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.code === 'rate_limited' && error.retryAfter) {
      return `${error.message} Try again in ${error.retryAfter}s.`
    }
    if (error.fields.length > 0 && error.code === 'validation_failed') {
      return error.fields.map((f) => `${f.field}: ${f.message}`).join('; ')
    }
    return error.message
  }
  if (error instanceof TypeError) return 'Could not reach the Lunex API. Is the backend running?'
  if (error instanceof Error) return error.message
  return 'Something went wrong.'
}

export function isAbort(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

/** Build a query string, dropping empty values. */
export function qs(params: Record<string, string | number | boolean | null | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue
    search.set(key, String(value))
  }
  const s = search.toString()
  return s ? `?${s}` : ''
}
