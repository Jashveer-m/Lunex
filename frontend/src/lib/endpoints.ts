// One typed function per endpoint in docs/api.md. No logic lives here beyond
// shaping the request; screens decide what to do with errors.
import { apiFetch, qs } from './api'
import type {
  Action,
  AuthResponse,
  CalendarEvent,
  CalendarEventInput,
  CalendarPage,
  Conversation,
  ConversationDetail,
  Expense,
  ExpenseCategory,
  ExpenseInput,
  ExpensePage,
  ExpenseSummary,
  Goal,
  GoalInput,
  Graph,
  LunexDocument,
  Memory,
  Message,
  Milestone,
  Note,
  NoteInput,
  Page,
  Task,
  TaskInput,
  User,
} from './types'

type ListParams = { limit?: number; offset?: number; sort?: string }

// --- auth ------------------------------------------------------------------

export const auth = {
  register: (body: { email: string; password: string; name?: string; timezone?: string }) =>
    apiFetch<AuthResponse>('/api/v1/auth/register', { method: 'POST', body, anonymous: true }),
  login: (body: { email: string; password: string }) =>
    apiFetch<AuthResponse>('/api/v1/auth/login', { method: 'POST', body, anonymous: true }),
  logout: (refreshToken: string) =>
    apiFetch<void>('/api/v1/auth/logout', { method: 'POST', body: { refresh_token: refreshToken }, anonymous: true }),
  me: () => apiFetch<User>('/api/v1/me'),
}

export const getHealth = () =>
  apiFetch<{ status: 'ok' | 'degraded'; database: 'ok' | 'unreachable' }>('/healthz', { anonymous: true })

// --- tasks -----------------------------------------------------------------

export const tasks = {
  list: (p: ListParams & { status?: string; q?: string; tag?: string; category?: string }) =>
    apiFetch<Page<'tasks', Task>>(`/api/v1/tasks${qs(p)}`),
  get: (id: string) => apiFetch<Task>(`/api/v1/tasks/${id}`),
  create: (body: Partial<TaskInput> & { title: string }) =>
    apiFetch<Task>('/api/v1/tasks', { method: 'POST', body }),
  update: (id: string, body: Partial<TaskInput>) =>
    apiFetch<Task>(`/api/v1/tasks/${id}`, { method: 'PATCH', body }),
  remove: (id: string) => apiFetch<void>(`/api/v1/tasks/${id}`, { method: 'DELETE' }),
  addDependency: (id: string, dependsOn: string) =>
    apiFetch<Task>(`/api/v1/tasks/${id}/dependencies`, {
      method: 'POST',
      body: { depends_on_task_id: dependsOn },
    }),
}

// --- goals -----------------------------------------------------------------

export const goals = {
  list: (p: ListParams & { status?: string; type?: string; q?: string }) =>
    apiFetch<Page<'goals', Goal>>(`/api/v1/goals${qs(p)}`),
  get: (id: string) => apiFetch<Goal>(`/api/v1/goals/${id}`),
  create: (body: Partial<GoalInput> & { title: string; type: string }) =>
    apiFetch<Goal>('/api/v1/goals', { method: 'POST', body }),
  update: (id: string, body: Partial<GoalInput>) =>
    apiFetch<Goal>(`/api/v1/goals/${id}`, { method: 'PATCH', body }),
  remove: (id: string) => apiFetch<void>(`/api/v1/goals/${id}`, { method: 'DELETE' }),
  addMilestone: (id: string, body: { title: string; target_date: string | null }) =>
    apiFetch<Milestone>(`/api/v1/goals/${id}/milestones`, { method: 'POST', body }),
  updateMilestone: (id: string, milestoneId: string, body: Partial<Pick<Milestone, 'title' | 'target_date' | 'completed'>>) =>
    apiFetch<Milestone>(`/api/v1/goals/${id}/milestones/${milestoneId}`, { method: 'PATCH', body }),
}

// --- notes -----------------------------------------------------------------

export const notes = {
  list: (p: ListParams & { q?: string; tag?: string }) =>
    apiFetch<Page<'notes', Note>>(`/api/v1/notes${qs(p)}`),
  create: (body: Partial<NoteInput> & { title: string }) =>
    apiFetch<Note>('/api/v1/notes', { method: 'POST', body }),
  update: (id: string, body: Partial<NoteInput>) =>
    apiFetch<Note>(`/api/v1/notes/${id}`, { method: 'PATCH', body }),
  remove: (id: string) => apiFetch<void>(`/api/v1/notes/${id}`, { method: 'DELETE' }),
}

// --- calendar --------------------------------------------------------------

export const calendar = {
  // start and end are required: the API has no unbounded read of events.
  list: (p: { start: string; end: string; limit?: number; offset?: number; sort?: string }, signal?: AbortSignal) =>
    apiFetch<CalendarPage>(`/api/v1/calendar${qs(p)}`, { signal }),
  get: (id: string) => apiFetch<CalendarEvent>(`/api/v1/calendar/${id}`),
  create: (body: Partial<CalendarEventInput> & { title: string; start_time: string }) =>
    apiFetch<CalendarEvent>('/api/v1/calendar', { method: 'POST', body }),
  update: (id: string, body: Partial<CalendarEventInput>) =>
    apiFetch<CalendarEvent>(`/api/v1/calendar/${id}`, { method: 'PATCH', body }),
  remove: (id: string) => apiFetch<void>(`/api/v1/calendar/${id}`, { method: 'DELETE' }),
}

// --- expenses --------------------------------------------------------------

export const expenseCategories = {
  list: (signal?: AbortSignal) =>
    apiFetch<{ categories: ExpenseCategory[]; count: number }>('/api/v1/expense-categories', { signal }),
  // There is no rename and no delete: the API does not offer either, because
  // both rewrite spending history through a door marked "categories".
  create: (name: string) =>
    apiFetch<ExpenseCategory>('/api/v1/expense-categories', { method: 'POST', body: { name } }),
}

type ExpenseFilter = {
  start?: string
  end?: string
  category_id?: string
  q?: string
  sort?: string
  limit?: number
  offset?: number
}

export const expenses = {
  // start and end are optional here, unlike the calendar: "what have I spent on
  // this" over the whole history is a question with an answer, and the paging
  // is what bounds the read. Both bounds are inclusive days.
  list: (p: ExpenseFilter, signal?: AbortSignal) => apiFetch<ExpensePage>(`/api/v1/expenses${qs(p)}`, { signal }),
  // The same filter as the list, with the paging ignored: a summary of the
  // first page would not be a summary.
  summary: (p: Omit<ExpenseFilter, 'limit' | 'offset' | 'sort'>, signal?: AbortSignal) =>
    apiFetch<ExpenseSummary>(`/api/v1/expenses/summary${qs(p)}`, { signal }),
  get: (id: string) => apiFetch<Expense>(`/api/v1/expenses/${id}`),
  create: (body: Partial<ExpenseInput> & { amount: string; expense_date: string }) =>
    apiFetch<Expense>('/api/v1/expenses', { method: 'POST', body }),
  update: (id: string, body: Partial<ExpenseInput>) =>
    apiFetch<Expense>(`/api/v1/expenses/${id}`, { method: 'PATCH', body }),
  remove: (id: string) => apiFetch<void>(`/api/v1/expenses/${id}`, { method: 'DELETE' }),
}

// --- documents -------------------------------------------------------------

export const documents = {
  list: (p: ListParams & { status?: string }) =>
    apiFetch<Page<'documents', LunexDocument>>(`/api/v1/documents${qs(p)}`),
  get: (id: string) => apiFetch<LunexDocument>(`/api/v1/documents/${id}`),
  upload: (file: File, signal?: AbortSignal) => {
    const form = new FormData()
    form.append('file', file)
    return apiFetch<LunexDocument>('/api/v1/documents', { method: 'POST', body: form, signal })
  },
  remove: (id: string) => apiFetch<void>(`/api/v1/documents/${id}`, { method: 'DELETE' }),
}

// --- conversations ---------------------------------------------------------

/**
 * The API omits `messages` for a conversation that has none (the handler tags
 * it `omitempty`, although docs/api.md shows it always present), and a Go nil
 * slice can arrive as `null`. Normalise at the edge so screens can rely on arrays.
 */
export function normalizeMessage(m: Message): Message {
  return { ...m, sources: m.sources ?? [] }
}

export const conversations = {
  list: (p: ListParams) => apiFetch<Page<'conversations', Conversation>>(`/api/v1/conversations${qs(p)}`),
  get: (id: string, signal?: AbortSignal) =>
    apiFetch<ConversationDetail>(`/api/v1/conversations/${id}`, { signal }).then((c) => ({
      ...c,
      messages: (c.messages ?? []).map(normalizeMessage),
    })),
  create: (title?: string) =>
    apiFetch<Conversation>('/api/v1/conversations', { method: 'POST', body: title ? { title } : {} }),
  remove: (id: string) => apiFetch<void>(`/api/v1/conversations/${id}`, { method: 'DELETE' }),
}

// --- actions ---------------------------------------------------------------

export const actions = {
  list: (p: ListParams & { status?: string; permission_level?: string; conversation_id?: string }, signal?: AbortSignal) =>
    apiFetch<Page<'actions', Action>>(`/api/v1/actions${qs(p)}`, { signal }),
  get: (id: string) => apiFetch<Action>(`/api/v1/actions/${id}`),
  // No body at all: the endpoint rejects any field, and what runs is exactly
  // what was proposed.
  approve: (id: string) => apiFetch<Action>(`/api/v1/actions/${id}/approve`, { method: 'POST' }),
  reject: (id: string) => apiFetch<Action>(`/api/v1/actions/${id}/reject`, { method: 'POST' }),
}

// --- memories --------------------------------------------------------------

export const memories = {
  list: (p: ListParams & { type?: string; enabled?: string }) =>
    apiFetch<Page<'memories', Memory>>(`/api/v1/memories${qs(p)}`),
  update: (id: string, body: { content?: string; enabled?: boolean }) =>
    apiFetch<Memory>(`/api/v1/memories/${id}`, { method: 'PATCH', body }),
  remove: (id: string) => apiFetch<void>(`/api/v1/memories/${id}`, { method: 'DELETE' }),
  clearAll: () => apiFetch<{ deleted: number }>('/api/v1/memories', { method: 'DELETE', body: { confirm: true } }),
}

// --- knowledge graph -------------------------------------------------------

export const graph = {
  get: (p: { type?: string; limit?: number; offset?: number }) =>
    apiFetch<Graph>(`/api/v1/knowledge-graph${qs(p)}`),
  removeNode: (id: string) => apiFetch<void>(`/api/v1/knowledge-graph/nodes/${id}`, { method: 'DELETE' }),
  removeEdge: (id: string) => apiFetch<void>(`/api/v1/knowledge-graph/edges/${id}`, { method: 'DELETE' }),
}
