// Loading state for a limit/offset list endpoint: the first page on mount (and
// whenever `key` changes), "load more" after it, and local edits in between so
// a create or delete does not need a round trip to show up.
import { useCallback, useEffect, useRef, useState } from 'react'

import { errorMessage } from './api'

export type PagedFetch<T> = (limit: number, offset: number) => Promise<{ items: T[]; count: number }>

export type Paged<T> = {
  items: T[]
  loading: boolean
  loadingMore: boolean
  error: string | null
  hasMore: boolean
  loadMore: () => void
  reload: () => void
  setItems: (update: (items: T[]) => T[]) => void
}

export function usePaged<T>(fetchPage: PagedFetch<T>, key: string, pageSize = 50): Paged<T> {
  const [items, setItemsState] = useState<T[]>([])
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [hasMore, setHasMore] = useState(false)
  const [reloadCount, setReloadCount] = useState(0)
  // The fetcher is usually an inline closure; only `key` decides when to refetch.
  const fetchRef = useRef(fetchPage)
  const itemsRef = useRef(items)
  const generation = useRef(0)
  useEffect(() => {
    fetchRef.current = fetchPage
    itemsRef.current = items
  })

  useEffect(() => {
    const gen = ++generation.current
    setLoading(true)
    setError(null)
    fetchRef
      .current(pageSize, 0)
      .then((page) => {
        if (gen !== generation.current) return
        setItemsState(page.items)
        setHasMore(page.count === pageSize)
      })
      .catch((e: unknown) => gen === generation.current && setError(errorMessage(e)))
      .finally(() => gen === generation.current && setLoading(false))
  }, [key, pageSize, reloadCount])

  const loadMore = useCallback(() => {
    const gen = generation.current
    setLoadingMore(true)
    fetchRef
      .current(pageSize, itemsRef.current.length)
      .then((page) => {
        if (gen !== generation.current) return
        setItemsState((prev) => {
          const seen = new Set(prev.map((i) => (i as { id?: string }).id))
          return [...prev, ...page.items.filter((i) => !seen.has((i as { id?: string }).id))]
        })
        setHasMore(page.count === pageSize)
      })
      .catch((e: unknown) => gen === generation.current && setError(errorMessage(e)))
      .finally(() => gen === generation.current && setLoadingMore(false))
  }, [pageSize])

  const reload = useCallback(() => setReloadCount((n) => n + 1), [])
  const setItems = useCallback((update: (items: T[]) => T[]) => setItemsState(update), [])

  return { items, loading, loadingMore, error, hasMore, loadMore, reload, setItems }
}
