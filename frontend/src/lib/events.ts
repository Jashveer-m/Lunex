// A one-event bus: "the set of proposed actions may have changed". The chat
// fires it when an action frame arrives, action cards fire it after a decision,
// and the sidebar's pending counter listens.
const EVENT = 'lunex:actions-changed'

export function announceActionsChanged() {
  window.dispatchEvent(new Event(EVENT))
}

export function onActionsChanged(listener: () => void): () => void {
  window.addEventListener(EVENT, listener)
  return () => window.removeEventListener(EVENT, listener)
}
