// Citations: the [S1] markers inside an answer, and the list of what was
// retrieved for it underneath.
import { Fragment, useState, type ReactNode } from 'react'

import { cx } from '../../components/ui'
import { pct } from '../../lib/format'
import { Link } from '../../lib/router'
import type { Source } from '../../lib/types'

const CITATION = /\[(S\d+(?:\s*[,;]\s*S\d+)*)\]/g

/** Labels an answer actually references, read from its text. */
export function citedLabels(text: string): Set<string> {
  const labels = new Set<string>()
  for (const m of text.matchAll(CITATION)) for (const l of m[1].split(/[,;]/)) labels.add(l.trim())
  return labels
}

function sourceDomId(messageKey: string, label: string) {
  return `src-${messageKey}-${label}`
}

function focusSource(messageKey: string, label: string) {
  const el = document.getElementById(sourceDomId(messageKey, label))
  if (!el) return
  el.scrollIntoView({ behavior: 'smooth', block: 'nearest' })
  el.classList.remove('flash')
  void el.offsetWidth // restart the animation
  el.classList.add('flash')
}

/** **bold** is the only markup rendered; everything else stays literal text. */
function renderBold(text: string, keyPrefix: string): ReactNode[] {
  return text.split(/(\*\*[^*\n]+\*\*)/g).map((part, i) =>
    part.startsWith('**') && part.endsWith('**') && part.length > 4 ? (
      <strong key={`${keyPrefix}-${i}`} className="font-semibold text-ink">
        {part.slice(2, -2)}
      </strong>
    ) : (
      <Fragment key={`${keyPrefix}-${i}`}>{part}</Fragment>
    ),
  )
}

/** Answer text with each [S1] turned into a chip that jumps to its source. */
export function AnswerText({ text, sources, messageKey }: { text: string; sources: Source[]; messageKey: string }) {
  const byLabel = new Map(sources.map((s) => [s.label, s]))
  const nodes: ReactNode[] = []
  let last = 0
  let n = 0
  for (const m of text.matchAll(CITATION)) {
    const start = m.index ?? 0
    if (start > last) nodes.push(...renderBold(text.slice(last, start), `t${n}`))
    const labels = m[1].split(/[,;]/).map((l) => l.trim())
    nodes.push(
      <span key={`c${n}`} className="whitespace-nowrap">
        {labels.map((label) => {
          const source = byLabel.get(label)
          return (
            <button
              key={label}
              type="button"
              onClick={() => focusSource(messageKey, label)}
              title={source ? `${source.title}${source.similarity != null ? ` · ${pct(source.similarity)} match` : ''}` : 'Unknown source'}
              className={cx(
                'mx-0.5 inline-flex h-[18px] items-center rounded px-1 align-[1px] font-mono text-[10.5px] font-semibold transition-colors',
                source ? 'bg-accent-soft text-accent hover:bg-accent hover:text-white' : 'bg-danger-soft text-danger',
              )}
            >
              {label}
            </button>
          )
        })}
      </span>,
    )
    last = start + m[0].length
    n++
  }
  if (last < text.length) nodes.push(...renderBold(text.slice(last), `t${n}`))
  return <>{nodes}</>
}

const TYPE_LABEL: Record<Source['type'], string> = {
  document: 'Document',
  memory: 'Memory',
  graph: 'Connection',
  task: 'Task',
  goal: 'Goal',
  note: 'Note',
  event: 'Event',
}

/** Partial: a source type with no page of its own shows no link rather than a
 *  link somewhere else. Calendar events have none yet. */
const TYPE_LINK: Partial<Record<Source['type'], string>> = {
  document: '/documents',
  memory: '/memories',
  graph: '/graph',
  task: '/',
  goal: '/?tab=goals',
  note: '/?tab=notes',
}

function SourceItem({ source, messageKey }: { source: Source; messageKey: string }) {
  const [open, setOpen] = useState(false)
  return (
    <li id={sourceDomId(messageKey, source.label)} className="rounded-lg px-2 py-1.5">
      <button type="button" onClick={() => setOpen((o) => !o)} className="flex w-full items-start gap-2 text-left" aria-expanded={open}>
        <span
          className={cx(
            'mt-px inline-flex h-[18px] shrink-0 items-center rounded px-1 font-mono text-[10.5px] font-semibold',
            source.cited ? 'bg-accent text-white dark:text-canvas' : 'bg-sunken text-ink-faint',
          )}
        >
          {source.label}
        </span>
        <span className="min-w-0 flex-1">
          <span className="text-xs text-ink">
            <span className="text-ink-faint">{TYPE_LABEL[source.type]} · </span>
            <span className="font-medium">{source.title}</span>
            {source.chunk_index != null && <span className="text-ink-faint"> · chunk {source.chunk_index + 1}</span>}
          </span>
          {source.tool && <span className="ml-1.5 text-[11px] text-ink-faint">(found by {source.tool.replace(/_/g, ' ')})</span>}
        </span>
        {source.similarity != null && (
          <span className="shrink-0 font-mono text-[11px] text-ink-faint" title="Cosine similarity used for retrieval">
            {pct(source.similarity)}
          </span>
        )}
      </button>
      {open && (
        <div className="mt-1.5 ml-8">
          <p className="rounded-md border-l-2 border-line-strong bg-sunken/60 px-2.5 py-1.5 text-xs leading-relaxed whitespace-pre-line text-ink-muted">
            {source.excerpt}
          </p>
          {TYPE_LINK[source.type] && (
            <Link to={TYPE_LINK[source.type]!} className="mt-1 inline-block text-[11px] text-accent hover:underline">
              Open {TYPE_LABEL[source.type].toLowerCase()}s →
            </Link>
          )}
        </div>
      )}
    </li>
  )
}

/**
 * Cited sources first; the rest — retrieved and shown to the model, but not
 * referenced by the answer — behind a toggle.
 */
export function SourceList({ sources, messageKey, streaming }: { sources: Source[]; messageKey: string; streaming?: boolean }) {
  const [showAll, setShowAll] = useState(false)
  if (sources.length === 0) return null
  const cited = sources.filter((s) => s.cited)
  const rest = sources.filter((s) => !s.cited)
  const visible = streaming || showAll ? [...cited, ...rest] : cited

  return (
    <div className="mt-3 border-t border-line pt-2.5">
      <p className="mb-1 px-2 text-[11px] font-medium tracking-wide text-ink-faint uppercase">
        {streaming
          ? `Grounded in ${sources.length} retrieved source${sources.length === 1 ? '' : 's'}`
          : cited.length
            ? `Cited ${cited.length} of ${sources.length} retrieved`
            : `${sources.length} retrieved, none cited`}
      </p>
      {visible.length > 0 && (
        <ul className="-mx-0.5">
          {visible.map((s) => (
            <SourceItem key={s.label} source={s} messageKey={messageKey} />
          ))}
        </ul>
      )}
      {!streaming && rest.length > 0 && (
        <button type="button" onClick={() => setShowAll((v) => !v)} className="mt-0.5 px-2 text-xs text-ink-faint hover:text-ink">
          {showAll ? 'Hide uncited sources' : `Show ${rest.length} uncited source${rest.length === 1 ? '' : 's'}`}
        </button>
      )}
    </div>
  )
}
