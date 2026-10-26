import { useEffect, useReducer, useRef, useState } from 'react'
import { boardReducer, initialBoard } from './boardReducer.js'
import { getAccessToken, refreshAuth } from './api.js'

// Must match internal/realtime/ws.go.
const CLOSE_UNAUTHORIZED = 4001
const CLOSE_FORBIDDEN = 4003
const CLOSE_BAD_REQUEST = 4400

const MAX_BACKOFF_MS = 30_000

function backoff(attempt) {
  const base = Math.min(MAX_BACKOFF_MS, 500 * 2 ** attempt)
  // Full jitter so 50 branches' screens do not reconnect in lockstep after an outage.
  return Math.random() * base
}

function socketURL() {
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${window.location.host}/ws`
}

/**
 * Keeps a live, self-healing view of one branch's queue.
 *
 * status: 'connecting' | 'recovering' | 'live' | 'reconnecting' | 'error'
 * recovery: details of the last reconnect ({ replayed } or { snapshot: true })
 */
export function useQueueSocket(branchId, { onLoggedOut } = {}) {
  const [board, dispatch] = useReducer(boardReducer, initialBoard)
  const [status, setStatus] = useState('connecting')
  const [recovery, setRecovery] = useState(null)
  const [error, setError] = useState(null)

  // Socket callbacks need the last applied event id synchronously (to put it in
  // the next subscribe message), so it is tracked in a ref alongside the reducer.
  const lastEventId = useRef(null)

  const onLoggedOutRef = useRef(onLoggedOut)
  onLoggedOutRef.current = onLoggedOut

  useEffect(() => {
    if (!branchId) return
    dispatch({ type: 'reset' })
    lastEventId.current = null

    let ws = null
    let stopped = false
    let attempt = 0
    let timer = null
    let hadConnection = false

    const scheduleReconnect = (delay) => {
      clearTimeout(timer)
      setStatus('reconnecting')
      timer = setTimeout(connect, delay)
    }

    async function connect() {
      if (stopped) return
      let token
      try {
        token = await getAccessToken()
      } catch {
        onLoggedOutRef.current?.()
        return
      }
      if (stopped) return

      const resuming = lastEventId.current !== null
      setStatus(resuming ? 'recovering' : 'connecting')
      ws = new WebSocket(socketURL())

      ws.onopen = () => {
        const msg = { type: 'subscribe', token, branch_id: branchId }
        if (resuming) msg.last_event_id = lastEventId.current
        ws.send(JSON.stringify(msg))
      }

      ws.onmessage = (e) => {
        const msg = JSON.parse(e.data)
        switch (msg.type) {
          case 'snapshot':
            dispatch({ type: 'snapshot', board: msg.board, lastEventId: msg.last_event_id })
            lastEventId.current = msg.last_event_id
            if (hadConnection) setRecovery({ snapshot: true, at: Date.now() })
            hadConnection = true
            attempt = 0
            setStatus('live')
            break
          case 'event':
            dispatch({ type: 'event', event: msg })
            if (lastEventId.current === null || msg.id > lastEventId.current) lastEventId.current = msg.id
            break
          case 'replay_complete':
            setRecovery({ replayed: msg.replayed, at: Date.now() })
            hadConnection = true
            attempt = 0
            setStatus('live')
            break
        }
      }

      ws.onclose = async (e) => {
        ws = null
        if (stopped) return
        if (e.code === CLOSE_FORBIDDEN || e.code === CLOSE_BAD_REQUEST) {
          setError(e.reason || 'Bu ekran bu şubeyi izleyemiyor.')
          setStatus('error')
          return
        }
        if (e.code === CLOSE_UNAUTHORIZED) {
          // Token expired mid-connection: refresh and reconnect right away; the
          // last_event_id we send makes the gap invisible to viewers.
          try {
            await refreshAuth()
            scheduleReconnect(0)
          } catch {
            onLoggedOutRef.current?.()
          }
          return
        }
        scheduleReconnect(backoff(attempt++))
      }
    }

    // Reconnect immediately when the network comes back instead of waiting out the backoff.
    const onOnline = () => {
      if (!ws) scheduleReconnect(0)
    }
    window.addEventListener('online', onOnline)
    connect()

    return () => {
      stopped = true
      clearTimeout(timer)
      window.removeEventListener('online', onOnline)
      ws?.close(1000, 'screen closed')
    }
  }, [branchId])

  return { board, status, recovery, error }
}
