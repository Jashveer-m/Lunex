import { useState } from 'react'

import { ActionCard } from '../components/ActionCard'
import { IconInbox } from '../components/icons'
import { PageContainer } from '../components/Shell'
import { EmptyState, ErrorBanner, LoadMore, LoadingBlock, PageHeader, Tabs, cx } from '../components/ui'
import { actions as actionsApi } from '../lib/endpoints'
import { usePaged } from '../lib/usePaged'

type Filter = 'proposed' | 'writes' | 'reads'

const FILTERS: { value: Filter; label: string }[] = [
  { value: 'proposed', label: 'Waiting for you' },
  { value: 'writes', label: 'All changes' },
  { value: 'reads', label: 'Searches' },
]

const PARAMS: Record<Filter, { status?: string; permission_level?: string }> = {
  proposed: { status: 'proposed' },
  writes: { permission_level: 'write' },
  reads: { permission_level: 'read' },
}

/**
 * Every proposal the assistant has made, including ones from conversations that
 * were since deleted — a proposal outlives its conversation and can still be
 * decided here.
 */
export function ApprovalsPage() {
  const [filter, setFilter] = useState<Filter>('proposed')
  const list = usePaged(
    (limit, offset) => actionsApi.list({ limit, offset, ...PARAMS[filter] }).then((p) => ({ items: p.actions, count: p.count })),
    filter,
  )

  return (
    <PageContainer>
      <PageHeader
        title="Approvals"
        description="The assistant never changes your data on its own. Every change it wants to make waits here — and in the conversation it came from — until you approve or reject it."
      />
      <Tabs tabs={FILTERS} value={filter} onChange={setFilter} />

      {list.error && <ErrorBanner className="mb-4" message={list.error} onRetry={list.reload} />}
      {list.loading && list.items.length === 0 ? (
        <LoadingBlock label="Loading actions…" />
      ) : !list.error && list.items.length === 0 ? (
        <EmptyState title={filter === 'proposed' ? 'Nothing waiting for you' : 'Nothing here yet'} icon={<IconInbox size={22} />}>
          {filter === 'proposed'
            ? 'Ask the assistant to add a task, goal or note and its proposal will appear here.'
            : 'Actions the assistant takes or proposes are recorded here.'}
        </EmptyState>
      ) : (
        <div className={cx('space-y-3', list.loading && 'opacity-60')}>
          {list.items.map((a) => (
            <ActionCard
              key={a.id}
              action={a}
              showContext
              onChange={(updated) => list.setItems((items) => items.map((x) => (x.id === updated.id ? updated : x)))}
            />
          ))}
        </div>
      )}
      <LoadMore hasMore={list.hasMore} loading={list.loadingMore} onClick={list.loadMore} />
    </PageContainer>
  )
}
