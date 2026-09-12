// Wire types for the Lunex API (docs/api.md). Timestamps are RFC 3339 strings
// in UTC; they are formatted at the edge, never parsed into Dates in state.

export type User = {
  id: string
  email: string
  name: string
  timezone: string
  created_at: string
}

export type TokenObject = {
  access_token: string
  token_type: 'Bearer'
  expires_in: number
  access_token_expires_at: string
  refresh_token: string
  refresh_token_expires_at: string
}

export type AuthResponse = { user: User; tokens: TokenObject }

export type Page<K extends string, T> = { [key in K]: T[] } & {
  count: number
  limit: number
  offset: number
}

// --- tasks, goals, notes ---------------------------------------------------

export const TASK_STATUSES = ['pending', 'in_progress', 'completed'] as const
export const PRIORITIES = ['low', 'medium', 'high'] as const
export type TaskStatus = (typeof TASK_STATUSES)[number]
export type Priority = (typeof PRIORITIES)[number]

export type Task = {
  id: string
  title: string
  description: string | null
  priority: Priority
  status: TaskStatus
  category: string | null
  tags: string[]
  deadline: string | null
  parent_task_id: string | null
  estimated_effort_minutes: number | null
  actual_effort_minutes: number | null
  depends_on: string[]
  created_at: string
  updated_at: string
}

export type TaskInput = {
  title: string
  description: string | null
  priority: Priority
  status: TaskStatus
  category: string | null
  tags: string[]
  deadline: string | null
  parent_task_id: string | null
  estimated_effort_minutes: number | null
  actual_effort_minutes: number | null
}

export const GOAL_STATUSES = ['active', 'completed', 'abandoned'] as const
export const GOAL_TYPES = [
  'short_term',
  'long_term',
  'career',
  'education',
  'financial',
  'personal',
  'project',
] as const
export type GoalStatus = (typeof GOAL_STATUSES)[number]
export type GoalType = (typeof GOAL_TYPES)[number]

export type Milestone = {
  id: string
  goal_id: string
  title: string
  target_date: string | null
  completed: boolean
  created_at: string
}

export type Goal = {
  id: string
  title: string
  description: string | null
  type: GoalType
  status: GoalStatus
  deadline: string | null
  milestones: Milestone[]
  created_at: string
  updated_at: string
}

export type GoalInput = {
  title: string
  description: string | null
  type: GoalType
  status: GoalStatus
  deadline: string | null
}

export type Note = {
  id: string
  title: string
  content: string
  tags: string[]
  created_at: string
  updated_at: string
}

export type NoteInput = { title: string; content: string; tags: string[] }

// --- documents -------------------------------------------------------------

export type DocumentStatus = 'processing' | 'ready' | 'failed'

export type LunexDocument = {
  id: string
  filename: string
  file_type: string
  status: DocumentStatus
  error_message: string | null
  chunk_count: number
  text_length?: number
  extracted_text?: string
  created_at: string
  updated_at: string
}

// --- chat ------------------------------------------------------------------

export type SourceType = 'document' | 'memory' | 'graph' | 'task' | 'goal' | 'note'

export type Source = {
  type: SourceType
  id: string
  label: string
  title: string
  chunk_index?: number
  similarity?: number
  excerpt: string
  cited: boolean
  tool?: string
}

export type Message = {
  id: string
  role: 'user' | 'assistant'
  content: string
  sources: Source[]
  created_at: string
}

export type Conversation = {
  id: string
  title: string
  message_count: number
  created_at: string
  updated_at: string
}

export type ConversationDetail = Conversation & { messages: Message[] }

// --- actions ---------------------------------------------------------------

export type ActionStatus = 'proposed' | 'approved' | 'rejected' | 'executed' | 'failed'

export type Action = {
  id: string
  conversation_id: string | null
  tool_name: string
  permission_level: 'read' | 'write'
  status: ActionStatus
  input: Record<string, unknown>
  summary: string
  result: Record<string, unknown> | null
  error_message: string | null
  created_at: string
  updated_at: string
}

// --- memories --------------------------------------------------------------

export const MEMORY_TYPES = ['episodic', 'semantic', 'preference', 'project', 'goal'] as const
export type MemoryType = (typeof MEMORY_TYPES)[number]

export type Memory = {
  id: string
  type: MemoryType
  content: string
  importance: number
  confidence: number
  source_conversation_id: string | null
  enabled: boolean
  expires_at: string | null
  created_at: string
  updated_at: string
}

// --- knowledge graph -------------------------------------------------------

export type GraphNode = {
  id: string
  type: string
  label: string
  ref_table: string | null
  ref_id: string | null
  extracted: boolean
  created_at: string
  updated_at: string
}

export type GraphEdge = {
  id: string
  from_node_id: string
  to_node_id: string
  relationship: string
  confidence: number
  source_conversation_id: string | null
  created_at: string
}

export type Graph = {
  nodes: GraphNode[]
  edges: GraphEdge[]
  node_count: number
  edge_count: number
  limit: number
  offset: number
}

// --- SSE -------------------------------------------------------------------

export type DoneFrame = {
  conversation_id: string
  user_message: Message
  message: Message
  model: string
  remembered: { id: string; type: MemoryType; content: string }[]
  linked: { from_node_id: string; to_node_id: string; relationship: string; confidence: number }[]
  actions: Action[]
}
