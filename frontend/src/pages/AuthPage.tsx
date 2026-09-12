import { useState, type FormEvent } from 'react'

import { Mark } from '../components/Shell'
import { Button, ErrorBanner, Field, Input } from '../components/ui'
import { ApiError, errorMessage } from '../lib/api'
import { useAuth } from '../lib/auth'
import { Link } from '../lib/router'

const MIN_PASSWORD = 12

export function AuthPage({ mode, notice }: { mode: 'login' | 'register'; notice?: string }) {
  const { login, register } = useAuth()
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | string | null>(null)
  const isRegister = mode === 'register'

  const fieldError = (field: string) => (error instanceof ApiError ? error.fieldError(field) : undefined)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setError(null)
    if (isRegister && password.length < MIN_PASSWORD) {
      setError(new ApiError(400, 'validation_failed', '', [{ field: 'password', message: `must be at least ${MIN_PASSWORD} characters` }]))
      return
    }
    setBusy(true)
    try {
      if (isRegister) {
        const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
        await register({ email, password, name: name.trim() || undefined, timezone })
      } else {
        await login(email, password)
      }
    } catch (e) {
      setError(e instanceof ApiError ? e : errorMessage(e))
      setBusy(false)
    }
  }

  // Field errors render under their field; anything else goes in the banner.
  const banner =
    typeof error === 'string'
      ? error
      : error && error.fields.length === 0
        ? errorMessage(error)
        : null

  return (
    <div className="grid min-h-full lg:grid-cols-[1fr_1.1fr]">
      <section className="flex flex-col justify-center px-8 py-12 sm:px-16">
        <div className="mx-auto w-full max-w-sm">
          <div className="mb-10 flex items-center gap-2.5">
            <Mark />
            <span className="font-display text-xl tracking-tight">Lunex</span>
          </div>

          <h1 className="font-display text-3xl tracking-tight">{isRegister ? 'Create your account' : 'Welcome back'}</h1>
          <p className="mt-2 text-sm text-ink-muted">
            {isRegister ? 'Already have one? ' : 'New here? '}
            <Link to={isRegister ? '/login' : '/register'} className="font-medium text-accent hover:underline">
              {isRegister ? 'Sign in' : 'Create an account'}
            </Link>
          </p>

          {notice && !error && <p className="mt-6 rounded-lg bg-warn-soft px-3.5 py-2.5 text-sm text-warn">{notice}</p>}
          {banner && <ErrorBanner className="mt-6" message={banner} />}

          <form onSubmit={submit} className="mt-8 flex flex-col gap-4" noValidate>
            {isRegister && (
              <Field label="Name" hint="Optional — what the assistant calls you." error={fieldError('name')}>
                {(id) => <Input id={id} value={name} onChange={(e) => setName(e.target.value)} autoComplete="name" />}
              </Field>
            )}
            <Field label="Email" error={fieldError('email')}>
              {(id) => (
                <Input
                  id={id}
                  type="email"
                  required
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  autoComplete="email"
                  aria-invalid={!!fieldError('email')}
                  autoFocus
                />
              )}
            </Field>
            <Field
              label="Password"
              hint={isRegister ? `At least ${MIN_PASSWORD} characters.` : undefined}
              error={fieldError('password')}
            >
              {(id) => (
                <Input
                  id={id}
                  type="password"
                  required
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete={isRegister ? 'new-password' : 'current-password'}
                  aria-invalid={!!fieldError('password')}
                />
              )}
            </Field>
            <Button type="submit" variant="primary" loading={busy} className="mt-2 h-10">
              {isRegister ? 'Create account' : 'Sign in'}
            </Button>
          </form>
        </div>
      </section>

      <aside className="hidden flex-col justify-end overflow-hidden border-l border-line bg-sunken p-14 lg:flex">
        <div className="relative mb-auto ml-auto size-56 rounded-full bg-ink/90 shadow-2xl">
          <div className="absolute -top-3 left-14 size-52 rounded-full bg-sunken" />
        </div>
        <blockquote className="max-w-md">
          <p className="font-display text-2xl leading-snug text-ink">
            Your tasks, goals, notes and documents — and an assistant that answers from them, cites what it used, and
            never changes anything without asking.
          </p>
        </blockquote>
      </aside>
    </div>
  )
}
