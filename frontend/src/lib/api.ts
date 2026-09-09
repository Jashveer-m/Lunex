// Thin fetch wrapper for the LifeOS API.
//
// Phase 1 scaffold: only the health endpoint is wired up. The auth calls
// (register / login / refresh / logout / me) land in Phase 2 together with the
// token store and the routed UI.
const BASE_URL = import.meta.env.VITE_API_BASE_URL ?? ''

export class ApiError extends Error {
  status: number
  code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

export async function apiFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(`${BASE_URL}${path}`, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...init.headers,
    },
  })

  if (!response.ok) {
    const body = await response.json().catch(() => null)
    throw new ApiError(
      response.status,
      body?.error ?? 'unknown_error',
      body?.message ?? response.statusText,
    )
  }
  return response.status === 204 ? (undefined as T) : ((await response.json()) as T)
}

export type Health = {
  status: 'ok' | 'degraded'
  database: 'ok' | 'unreachable'
}

export const getHealth = () => apiFetch<Health>('/healthz')
