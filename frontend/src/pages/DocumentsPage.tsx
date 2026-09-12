import { useEffect, useRef, useState, type DragEvent } from 'react'

import { IconFile, IconTrash, IconUpload } from '../components/icons'
import { PageContainer } from '../components/Shell'
import {
  Badge,
  Button,
  ConfirmDialog,
  Dialog,
  EmptyState,
  ErrorBanner,
  IconButton,
  LoadMore,
  LoadingBlock,
  PageHeader,
  Spinner,
  cx,
} from '../components/ui'
import { errorMessage } from '../lib/api'
import { documents as docsApi } from '../lib/endpoints'
import { plural, relativeTime } from '../lib/format'
import type { LunexDocument } from '../lib/types'
import { usePaged } from '../lib/usePaged'

const ACCEPT = '.pdf,.txt,.text,.md,.markdown,application/pdf,text/plain,text/markdown'

type Upload = { key: string; file: File; error?: string }

export function DocumentsPage() {
  const list = usePaged(
    (limit, offset) => docsApi.list({ limit, offset }).then((p) => ({ items: p.documents, count: p.count })),
    'documents',
  )
  const [uploads, setUploads] = useState<Upload[]>([])
  const [dragging, setDragging] = useState(false)
  const [deleting, setDeleting] = useState<LunexDocument | null>(null)
  const [viewing, setViewing] = useState<LunexDocument | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const queue = useRef(Promise.resolve())

  // Processing is synchronous, so "processing" is only ever seen for an upload
  // running elsewhere (another tab). Poll while one is on screen.
  const anyProcessing = list.items.some((d) => d.status === 'processing')
  useEffect(() => {
    if (!anyProcessing) return
    const t = setInterval(list.reload, 3000)
    return () => clearInterval(t)
  }, [anyProcessing, list.reload])

  const enqueue = (files: FileList | File[]) => {
    for (const file of Array.from(files)) {
      const key = `${file.name}-${file.size}-${Math.random()}`
      setUploads((u) => [...u, { key, file }])
      // One at a time: each upload is a full extract-chunk-embed pass on the
      // server, and running them in parallel would only queue them in Ollama.
      queue.current = queue.current.then(async () => {
        try {
          const doc = await docsApi.upload(file)
          list.setItems((items) => [doc, ...items.filter((d) => d.id !== doc.id)])
          setUploads((u) => u.filter((x) => x.key !== key))
        } catch (e) {
          setUploads((u) => u.map((x) => (x.key === key ? { ...x, error: errorMessage(e) } : x)))
        }
      })
    }
  }

  const onDrop = (e: DragEvent) => {
    e.preventDefault()
    setDragging(false)
    if (e.dataTransfer.files.length) enqueue(e.dataTransfer.files)
  }

  return (
    <PageContainer>
      <PageHeader
        title="Documents"
        description="Upload PDFs, Markdown or plain text. Each file is split into chunks and embedded, so the assistant can find and cite the passages that answer a question."
      />

      <div
        onDragOver={(e) => {
          e.preventDefault()
          setDragging(true)
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={onDrop}
        className={cx(
          'flex flex-col items-center justify-center rounded-2xl border-2 border-dashed px-6 py-10 text-center transition-colors',
          dragging ? 'border-accent bg-accent-soft' : 'border-line-strong bg-surface',
        )}
      >
        <div className="mb-3 flex size-11 items-center justify-center rounded-full bg-sunken text-ink-muted">
          <IconUpload size={20} />
        </div>
        <p className="text-sm text-ink">
          Drop files here, or{' '}
          <button type="button" onClick={() => inputRef.current?.click()} className="font-medium text-accent hover:underline">
            choose from your computer
          </button>
        </p>
        <p className="mt-1 text-xs text-ink-faint">PDF (text layer only), .txt, .md · up to 10 MB each</p>
        <input
          ref={inputRef}
          type="file"
          accept={ACCEPT}
          multiple
          hidden
          onChange={(e) => {
            if (e.target.files) enqueue(e.target.files)
            e.target.value = ''
          }}
        />
      </div>

      <div className="mt-8 space-y-3">
        {uploads.map((u, i) => (
          <div
            key={u.key}
            className={cx(
              'flex items-center gap-3 rounded-xl border px-4 py-3',
              u.error ? 'border-danger/25 bg-danger-soft' : 'border-line bg-surface',
            )}
          >
            {u.error ? <IconFile size={18} className="text-danger" /> : <Spinner className="text-accent" />}
            <div className="min-w-0 flex-1">
              <p className="truncate text-sm font-medium">{u.file.name}</p>
              <p className={cx('text-xs', u.error ? 'text-danger' : 'text-ink-faint')}>
                {u.error ?? (i === 0 || uploads.slice(0, i).every((x) => x.error) ? 'Extracting, chunking and embedding…' : 'Waiting…')}
              </p>
            </div>
            {u.error && (
              <Button size="sm" variant="ghost" onClick={() => setUploads((all) => all.filter((x) => x.key !== u.key))}>
                Dismiss
              </Button>
            )}
          </div>
        ))}

        {list.error && <ErrorBanner message={list.error} onRetry={list.reload} />}

        {list.loading && list.items.length === 0 ? (
          <LoadingBlock label="Loading documents…" />
        ) : !list.error && list.items.length === 0 && uploads.length === 0 ? (
          <EmptyState title="No documents yet" icon={<IconFile size={22} />}>
            Upload something you want to ask questions about — lecture notes, a paper, a contract.
          </EmptyState>
        ) : (
          <ul className="divide-y divide-line rounded-xl border border-line bg-surface">
            {list.items.map((doc) => (
              <DocumentRow key={doc.id} doc={doc} onView={() => setViewing(doc)} onDelete={() => setDeleting(doc)} />
            ))}
          </ul>
        )}
        <LoadMore hasMore={list.hasMore} loading={list.loadingMore} onClick={list.loadMore} />
      </div>

      <ConfirmDialog
        open={deleting !== null}
        onClose={() => setDeleting(null)}
        title="Delete document?"
        confirmLabel="Delete document"
        onConfirm={async () => {
          if (!deleting) return
          await docsApi.remove(deleting.id)
          list.setItems((items) => items.filter((d) => d.id !== deleting.id))
        }}
      >
        <p>
          “{deleting?.filename}” and its {deleting?.chunk_count ?? 0} embedded chunks will be deleted. The assistant will no
          longer be able to cite it. The original file is not stored, so this cannot be undone.
        </p>
      </ConfirmDialog>

      <DocumentViewer doc={viewing} onClose={() => setViewing(null)} />
    </PageContainer>
  )
}

function DocumentRow({ doc, onView, onDelete }: { doc: LunexDocument; onView: () => void; onDelete: () => void }) {
  return (
    <li className="group flex items-center gap-3 px-4 py-3">
      <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-sunken text-[10px] font-semibold tracking-wide text-ink-muted uppercase">
        {doc.file_type}
      </div>
      <div className="min-w-0 flex-1">
        <button
          type="button"
          onClick={onView}
          disabled={doc.status !== 'ready'}
          className="max-w-full truncate text-left text-sm font-medium hover:underline disabled:no-underline"
        >
          {doc.filename}
        </button>
        <p className="text-xs text-ink-faint">
          Uploaded {relativeTime(doc.created_at)}
          {doc.status === 'ready' && <> · {plural(doc.chunk_count, 'chunk')}</>}
        </p>
        {doc.status === 'failed' && doc.error_message && <p className="mt-1 text-xs text-danger">{doc.error_message}</p>}
      </div>
      {doc.status === 'ready' && <Badge tone="ok">Ready</Badge>}
      {doc.status === 'failed' && <Badge tone="danger">Failed</Badge>}
      {doc.status === 'processing' && (
        <Badge tone="accent">
          <Spinner size={10} /> Processing
        </Badge>
      )}
      <IconButton label="Delete document" tone="danger" onClick={onDelete} className="opacity-0 group-focus-within:opacity-100 group-hover:opacity-100">
        <IconTrash size={14} />
      </IconButton>
    </li>
  )
}

function DocumentViewer({ doc, onClose }: { doc: LunexDocument | null; onClose: () => void }) {
  const [full, setFull] = useState<LunexDocument | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!doc) return
    let cancelled = false
    setFull(null)
    setError(null)
    docsApi
      .get(doc.id)
      .then((d) => !cancelled && setFull(d))
      .catch((e) => !cancelled && setError(errorMessage(e)))
    return () => {
      cancelled = true
    }
  }, [doc])

  return (
    <Dialog open={doc !== null} onClose={onClose} title={doc?.filename ?? ''} wide>
      {error ? (
        <ErrorBanner message={error} />
      ) : !full ? (
        <LoadingBlock label="Loading text…" />
      ) : (
        <>
          <p className="mb-3 text-xs text-ink-faint">
            Extracted text · {full.text_length?.toLocaleString()} characters · {plural(full.chunk_count, 'chunk')}. The original
            file is not stored.
          </p>
          <pre className="font-sans text-sm leading-relaxed whitespace-pre-wrap text-ink">{full.extracted_text}</pre>
        </>
      )}
    </Dialog>
  )
}
