// Pure state transitions for a branch board. Kept free of React and I/O so the
// recovery rules are easy to reason about: a snapshot replaces everything, and
// an event is applied only if it is newer than the last one applied.

const FINISHED = new Set(['completed', 'no_show', 'cancelled'])

export const initialBoard = { tickets: {}, lastEventId: null }

export function boardReducer(state, action) {
  switch (action.type) {
    case 'reset':
      return initialBoard

    case 'snapshot': {
      const tickets = {}
      for (const t of action.board.tickets) tickets[t.id] = t
      return { tickets, lastEventId: action.lastEventId }
    }

    case 'event': {
      const ev = action.event
      // Duplicates can arrive around a reconnect; ids are strictly increasing per branch.
      if (state.lastEventId !== null && ev.id <= state.lastEventId) return state
      const next = { ...state, lastEventId: ev.id }
      const ticket = ev.data?.ticket
      if (ticket) {
        next.tickets = { ...state.tickets }
        if (FINISHED.has(ticket.status)) delete next.tickets[ticket.id]
        else next.tickets[ticket.id] = ticket
      }
      return next
    }

    default:
      return state
  }
}

export function selectCalled(state, limit = 6) {
  return Object.values(state.tickets)
    .filter((t) => t.status === 'called')
    .sort((a, b) => new Date(b.called_at) - new Date(a.called_at))
    .slice(0, limit)
}

export function selectWaitingByService(state) {
  const groups = {}
  for (const t of Object.values(state.tickets)) {
    if (t.status !== 'waiting') continue
    ;(groups[t.service_code] ||= []).push(t)
  }
  for (const list of Object.values(groups)) {
    list.sort((a, b) => new Date(a.created_at) - new Date(b.created_at))
  }
  return Object.entries(groups).sort(([a], [b]) => a.localeCompare(b))
}
