import test from 'node:test'
import assert from 'node:assert/strict'
import { boardReducer, initialBoard, selectCalled, selectWaitingByService } from './boardReducer.js'

const ticket = (id, status, extra = {}) => ({
  id, status, display_code: `A-00${id}`, service_code: 'A', created_at: `2026-10-25T10:0${id}:00Z`, ...extra,
})
const event = (id, t) => ({ type: 'event', event: { id, event_type: 'ticket.x', data: { ticket: t } } })

test('snapshot replaces state', () => {
  const s = boardReducer(initialBoard, { type: 'snapshot', lastEventId: 7, board: { tickets: [ticket(1, 'waiting')] } })
  assert.equal(s.lastEventId, 7)
  assert.deepEqual(Object.keys(s.tickets), ['1'])
})

test('events at or before lastEventId are ignored (replay/live overlap)', () => {
  let s = boardReducer(initialBoard, { type: 'snapshot', lastEventId: 10, board: { tickets: [] } })
  s = boardReducer(s, event(10, ticket(1, 'waiting')))
  s = boardReducer(s, event(9, ticket(2, 'waiting')))
  assert.deepEqual(s.tickets, {})
  s = boardReducer(s, event(11, ticket(3, 'waiting')))
  assert.deepEqual(Object.keys(s.tickets), ['3'])
  assert.equal(s.lastEventId, 11)
})

test('finished tickets leave the board, called ones are ordered newest first', () => {
  let s = boardReducer(initialBoard, { type: 'snapshot', lastEventId: 0, board: { tickets: [] } })
  s = boardReducer(s, event(1, ticket(1, 'called', { called_at: '2026-10-25T10:00:00Z' })))
  s = boardReducer(s, event(2, ticket(2, 'called', { called_at: '2026-10-25T10:05:00Z' })))
  s = boardReducer(s, event(3, ticket(3, 'waiting')))
  assert.deepEqual(selectCalled(s).map((t) => t.id), [2, 1])
  s = boardReducer(s, event(4, ticket(2, 'completed')))
  assert.deepEqual(selectCalled(s).map((t) => t.id), [1])
  assert.deepEqual(selectWaitingByService(s).map(([code, list]) => [code, list.length]), [['A', 1]])
})

test('non-ticket events only advance the cursor', () => {
  let s = boardReducer(initialBoard, { type: 'snapshot', lastEventId: 5, board: { tickets: [ticket(1, 'waiting')] } })
  s = boardReducer(s, { type: 'event', event: { id: 6, event_type: 'appointment.created', data: { appointment: {} } } })
  assert.equal(s.lastEventId, 6)
  assert.equal(Object.keys(s.tickets).length, 1)
})
