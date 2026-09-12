// Reads the chat endpoint's Server-Sent Events.
//
// This is SSE framing over a POST, so EventSource cannot be used (it can send
// neither a body nor an Authorization header). The stream is read with fetch
// and a reader, and frames are split on blank lines as the SSE spec describes.
import { authedFetch, toApiError } from './api'
import { normalizeMessage } from './endpoints'
import type { Action, DoneFrame, Source } from './types'

export type StreamHandlers = {
  onSources?: (sources: Source[]) => void
  onToken?: (text: string) => void
  onAction?: (action: Action) => void
  onDone?: (done: DoneFrame) => void
  /** Generation failed after the stream started; nothing was persisted. */
  onStreamError?: (error: { error: string; message: string }) => void
}

/**
 * POST a message and dispatch each frame as it arrives. Errors that happen
 * before the first frame (404, 400, 503 model_unavailable) are thrown as
 * ApiError, exactly like any other request.
 */
export async function streamMessage(
  conversationId: string,
  content: string,
  handlers: StreamHandlers,
  signal?: AbortSignal,
): Promise<void> {
  const response = await authedFetch(`/api/v1/conversations/${conversationId}/messages`, {
    method: 'POST',
    body: { content },
    signal,
  })
  if (!response.ok) throw await toApiError(response)
  if (!response.body) throw new Error('The browser did not expose the response stream.')

  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader()
  let buffer = ''
  let finished = false

  const dispatch = (raw: string) => {
    let event = 'message'
    const data: string[] = []
    for (const line of raw.split('\n')) {
      if (line.startsWith(':')) continue // comment / keep-alive
      const colon = line.indexOf(':')
      const field = colon === -1 ? line : line.slice(0, colon)
      let value = colon === -1 ? '' : line.slice(colon + 1)
      if (value.startsWith(' ')) value = value.slice(1)
      if (field === 'event') event = value
      else if (field === 'data') data.push(value)
    }
    if (data.length === 0) return
    const payload = JSON.parse(data.join('\n'))
    switch (event) {
      case 'sources':
        handlers.onSources?.(payload.sources ?? [])
        break
      case 'token':
        handlers.onToken?.(payload.text ?? '')
        break
      case 'action':
        handlers.onAction?.(payload as Action)
        break
      case 'done': {
        finished = true
        // Arrays the backend may send as null (a Go nil slice) become [].
        const done = payload as DoneFrame
        handlers.onDone?.({
          ...done,
          user_message: normalizeMessage(done.user_message),
          message: normalizeMessage(done.message),
          remembered: done.remembered ?? [],
          linked: done.linked ?? [],
          actions: done.actions ?? [],
        })
        break
      }
      case 'error':
        finished = true
        handlers.onStreamError?.(payload)
        break
    }
  }

  while (true) {
    const { value, done } = await reader.read()
    if (done) break
    buffer += value.replace(/\r\n?/g, '\n')
    let boundary = buffer.indexOf('\n\n')
    while (boundary !== -1) {
      dispatch(buffer.slice(0, boundary))
      buffer = buffer.slice(boundary + 2)
      boundary = buffer.indexOf('\n\n')
    }
  }
  if (buffer.trim()) dispatch(buffer)
  if (!finished) {
    // The connection closed without `done` or `error` -- a proxy timeout, the
    // server restarting. The backend does not persist an unfinished turn.
    handlers.onStreamError?.({
      error: 'stream_closed',
      message: 'The connection closed before the answer finished. The turn may not have been saved — send it again to retry.',
    })
  }
}
