// The session as React state. The tokens themselves live in lib/api.ts; this
// context only knows who is signed in and exposes the four things a screen can
// do about it.
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'

import {
  ApiError,
  clearTokens,
  hasStoredSession,
  onSessionExpired,
  onStorageLogout,
  readRefreshToken,
  refreshSession,
  storeTokens,
} from './api'
import { auth } from './endpoints'
import type { User } from './types'

type AuthState =
  | { status: 'booting' }
  /** A stored session exists but the API could not be reached to resume it. */
  | { status: 'offline'; message: string }
  | { status: 'anonymous'; notice?: string }
  | { status: 'authenticated'; user: User }

type AuthContextValue = {
  state: AuthState
  login: (email: string, password: string) => Promise<void>
  register: (input: { email: string; password: string; name?: string; timezone?: string }) => Promise<void>
  logout: () => Promise<void>
  retryBoot: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>(() =>
    hasStoredSession() ? { status: 'booting' } : { status: 'anonymous' },
  )
  const [bootAttempt, setBootAttempt] = useState(0)

  // Resume a stored session: trade the refresh token for an access token, then
  // ask who we are.
  useEffect(() => {
    if (!hasStoredSession()) return
    let cancelled = false
    refreshSession()
      .then(() => auth.me())
      .then((user) => !cancelled && setState({ status: 'authenticated', user }))
      .catch((error: unknown) => {
        if (cancelled) return
        if (error instanceof ApiError && error.status === 401) {
          setState({ status: 'anonymous', notice: 'Your session has expired. Please sign in again.' })
        } else {
          setState({
            status: 'offline',
            message: error instanceof ApiError ? error.message : 'Could not reach the Lunex API.',
          })
        }
      })
    return () => {
      cancelled = true
    }
  }, [bootAttempt])

  useEffect(() => {
    const expired = () =>
      setState({ status: 'anonymous', notice: 'Your session has expired. Please sign in again.' })
    const offExpired = onSessionExpired(expired)
    const offStorage = onStorageLogout(() => setState({ status: 'anonymous', notice: 'You signed out in another tab.' }))
    return () => {
      offExpired()
      offStorage()
    }
  }, [])

  const login = useCallback(async (email: string, password: string) => {
    const res = await auth.login({ email, password })
    storeTokens(res.tokens)
    setState({ status: 'authenticated', user: res.user })
  }, [])

  const register = useCallback(async (input: { email: string; password: string; name?: string; timezone?: string }) => {
    const res = await auth.register(input)
    storeTokens(res.tokens)
    setState({ status: 'authenticated', user: res.user })
  }, [])

  const logout = useCallback(async () => {
    const token = readRefreshToken()
    clearTokens()
    setState({ status: 'anonymous' })
    // Best effort: the local session is already gone either way.
    if (token) await auth.logout(token).catch(() => undefined)
  }, [])

  const retryBoot = useCallback(() => {
    setState({ status: 'booting' })
    setBootAttempt((n) => n + 1)
  }, [])

  const value = useMemo(() => ({ state, login, register, logout, retryBoot }), [state, login, register, logout, retryBoot])
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth outside AuthProvider')
  return ctx
}

export function useUser(): User {
  const { state } = useAuth()
  if (state.status !== 'authenticated') throw new Error('useUser outside an authenticated screen')
  return state.user
}
