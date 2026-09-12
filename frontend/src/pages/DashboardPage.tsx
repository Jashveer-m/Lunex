import { PageContainer } from '../components/Shell'
import { Tabs } from '../components/ui'
import { useUser } from '../lib/auth'
import { navigate, useLocation } from '../lib/router'
import { GoalsPanel } from './dashboard/GoalsPanel'
import { NotesPanel } from './dashboard/NotesPanel'
import { TasksPanel } from './dashboard/TasksPanel'

type Tab = 'tasks' | 'goals' | 'notes'
const TABS: { value: Tab; label: string }[] = [
  { value: 'tasks', label: 'Tasks' },
  { value: 'goals', label: 'Goals' },
  { value: 'notes', label: 'Notes' },
]

function greeting(): string {
  const h = new Date().getHours()
  if (h < 5) return 'Good evening'
  if (h < 12) return 'Good morning'
  if (h < 18) return 'Good afternoon'
  return 'Good evening'
}

export function DashboardPage() {
  const user = useUser()
  const { query } = useLocation()
  const raw = query.get('tab')
  const tab: Tab = raw === 'goals' || raw === 'notes' ? raw : 'tasks'
  const today = new Intl.DateTimeFormat(undefined, { weekday: 'long', day: 'numeric', month: 'long' }).format(new Date())

  return (
    <PageContainer>
      <header className="mb-8">
        <p className="text-xs font-medium tracking-wide text-ink-faint uppercase">{today}</p>
        <h1 className="mt-1 font-display text-3xl tracking-tight">
          {greeting()}
          {user.name ? `, ${user.name}` : ''}.
        </h1>
      </header>

      <Tabs tabs={TABS} value={tab} onChange={(t) => navigate(t === 'tasks' ? '/' : `/?tab=${t}`, { replace: true })} />

      {tab === 'tasks' && <TasksPanel />}
      {tab === 'goals' && <GoalsPanel />}
      {tab === 'notes' && <NotesPanel />}
    </PageContainer>
  )
}
