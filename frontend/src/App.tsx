import { useEffect, type ReactNode } from 'react'

import { Mark, Shell } from './components/Shell'
import { Button, Spinner } from './components/ui'
import { AuthProvider, useAuth } from './lib/auth'
import { match, navigate, useLocation } from './lib/router'
import { ApprovalsPage } from './pages/ApprovalsPage'
import { AuthPage } from './pages/AuthPage'
import { CalendarPage } from './pages/CalendarPage'
import { ChatPage } from './pages/ChatPage'
import { DashboardPage } from './pages/DashboardPage'
import { DocumentsPage } from './pages/DocumentsPage'
import { FinancePage } from './pages/FinancePage'
import { GraphPage } from './pages/GraphPage'
import { MemoriesPage } from './pages/MemoriesPage'
import { NotFoundPage } from './pages/NotFoundPage'

export default function App() {
  return (
    <AuthProvider>
      <Root />
    </AuthProvider>
  )
}

function Root() {
  const { state, retryBoot } = useAuth()
  const { path } = useLocation()

  // Signed in on an auth route (just logged in, or a bookmarked /login): go home.
  useEffect(() => {
    if (state.status === 'authenticated' && (path === '/login' || path === '/register')) {
      navigate('/', { replace: true })
    }
  }, [state.status, path])

  if (state.status === 'booting') {
    return (
      <FullScreen>
        <Spinner size={20} className="text-ink-faint" />
        <p className="text-sm text-ink-muted">Resuming your session…</p>
      </FullScreen>
    )
  }

  if (state.status === 'offline') {
    return (
      <FullScreen>
        <Mark />
        <p className="font-display text-2xl">Can’t reach Lunex</p>
        <p className="max-w-sm text-center text-sm text-ink-muted">
          {state.message} Your session is kept — start the API (<code className="font-mono text-xs">make run</code>) and
          try again.
        </p>
        <Button variant="primary" onClick={retryBoot}>
          Try again
        </Button>
      </FullScreen>
    )
  }

  if (state.status === 'anonymous') {
    return <AuthPage mode={path === '/register' ? 'register' : 'login'} notice={state.notice} />
  }

  return (
    <Shell>
      <Routes path={path} />
    </Shell>
  )
}

function Routes({ path }: { path: string }) {
  if (path === '/' || path === '/login' || path === '/register') return <DashboardPage />
  if (path === '/calendar') return <CalendarPage />
  if (path === '/finance') return <FinancePage />
  if (path === '/documents') return <DocumentsPage />
  if (path === '/memories') return <MemoriesPage />
  if (path === '/approvals') return <ApprovalsPage />
  if (path === '/graph') return <GraphPage />
  if (path === '/chat') return <ChatPage conversationId={null} />
  const chat = match('/chat/:id', path)
  if (chat) return <ChatPage conversationId={chat.id} />
  return <NotFoundPage />
}

function FullScreen({ children }: { children: ReactNode }) {
  return <div className="flex h-full flex-col items-center justify-center gap-4 p-6">{children}</div>
}
