import { useEffect, useState } from 'react'
import { getHealth, type Health } from './lib/api'

type Status = { state: 'loading' } | { state: 'ok'; health: Health } | { state: 'error'; message: string }

export default function App() {
  const [status, setStatus] = useState<Status>({ state: 'loading' })

  useEffect(() => {
    let cancelled = false
    getHealth()
      .then((health) => !cancelled && setStatus({ state: 'ok', health }))
      .catch((error: Error) => !cancelled && setStatus({ state: 'error', message: error.message }))
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-950 p-6 text-slate-100">
      <div className="w-full max-w-md rounded-xl border border-slate-800 bg-slate-900 p-8 shadow-lg">
        <h1 className="text-2xl font-semibold tracking-tight">LifeOS</h1>
        <p className="mt-2 text-sm text-slate-400">
          Phase 1 scaffold — backend, database and auth only. The UI lands in a later phase.
        </p>

        <dl className="mt-6 space-y-2 text-sm">
          <div className="flex items-center justify-between border-t border-slate-800 pt-3">
            <dt className="text-slate-400">API</dt>
            <dd className="font-mono">
              {status.state === 'loading' && <span className="text-slate-500">checking…</span>}
              {status.state === 'ok' && <span className="text-emerald-400">{status.health.status}</span>}
              {status.state === 'error' && <span className="text-rose-400">unreachable</span>}
            </dd>
          </div>
          <div className="flex items-center justify-between">
            <dt className="text-slate-400">Database</dt>
            <dd className="font-mono">
              {status.state === 'ok' ? (
                <span className={status.health.database === 'ok' ? 'text-emerald-400' : 'text-rose-400'}>
                  {status.health.database}
                </span>
              ) : (
                <span className="text-slate-500">—</span>
              )}
            </dd>
          </div>
        </dl>

        {status.state === 'error' && (
          <p className="mt-4 text-xs text-slate-500">
            Start the API with <code className="text-slate-300">go run ./cmd/api</code>, then reload.
          </p>
        )}
      </div>
    </main>
  )
}
