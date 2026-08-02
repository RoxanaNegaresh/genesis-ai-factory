/**
 * Live event stream.
 *
 * The stream prefers a websocket and falls back to polling. That fallback is
 * not defensive padding: a desktop app must keep working when the socket is
 * blocked by a corporate proxy or dies during sleep/resume, and the REST log is
 * authoritative anyway. Both paths use the same monotonic `seq` cursor, so a
 * transport switch mid-run produces no gaps and no duplicates.
 */

import { API_BASE, api, getToken, type GenesisEvent } from './api'

export type StreamState = 'connecting' | 'live' | 'polling' | 'closed'

export interface StreamHandlers {
  onEvent: (event: GenesisEvent) => void
  onState?: (state: StreamState) => void
  onGap?: (from: number, to: number) => void
}

export interface Stream {
  close: () => void
}

export function streamRun(runId: string, afterSeq: number, handlers: StreamHandlers): Stream {
  let cursor = afterSeq
  let closed = false
  let socket: WebSocket | null = null
  let pollTimer: number | null = null

  const setState = (state: StreamState) => handlers.onState?.(state)

  const emit = (event: GenesisEvent) => {
    // The cursor guard is what makes the two transports safely interchangeable:
    // an event already delivered by one path is ignored when the other replays it.
    if (event.seq <= cursor) return
    cursor = event.seq
    handlers.onEvent(event)
  }

  const catchUp = async () => {
    try {
      const page = await api.events(runId, cursor)
      page.data.forEach(emit)
      return true
    } catch {
      return false
    }
  }

  const startPolling = () => {
    if (closed || pollTimer !== null) return
    setState('polling')
    const tick = async () => {
      if (closed) return
      await catchUp()
      if (!closed) pollTimer = window.setTimeout(tick, 700)
    }
    void tick()
  }

  const stopPolling = () => {
    if (pollTimer !== null) {
      clearTimeout(pollTimer)
      pollTimer = null
    }
  }

  const connect = () => {
    if (closed) return
    const token = getToken()
    if (!token) {
      startPolling()
      return
    }

    setState('connecting')
    const url = `${API_BASE.replace(/^http/, 'ws')}/ws?token=${encodeURIComponent(token)}`

    try {
      socket = new WebSocket(url)
    } catch {
      startPolling()
      return
    }

    socket.onopen = () => {
      stopPolling()
      setState('live')
      socket?.send(JSON.stringify({ type: 'subscribe', topics: [`run:${runId}`], after_seq: cursor }))
    }

    socket.onmessage = (message) => {
      try {
        const frame = JSON.parse(message.data as string)
        if (frame.type === 'event' && frame.event) {
          emit(frame.event as GenesisEvent)
        } else if (frame.type === 'gap') {
          // The server dropped events under backpressure. Refetch the range
          // from the durable log rather than showing a hole in the timeline.
          handlers.onGap?.(frame.from, frame.to)
          void catchUp()
        }
      } catch {
        // A malformed frame must not tear down a working stream.
      }
    }

    socket.onerror = () => socket?.close()

    socket.onclose = () => {
      socket = null
      if (closed) return
      // Poll immediately so no time is lost, and retry the socket shortly.
      startPolling()
      window.setTimeout(() => {
        if (!closed) connect()
      }, 3000)
    }
  }

  void catchUp().then((ok) => {
    if (!ok) startPolling()
    connect()
  })

  return {
    close: () => {
      closed = true
      setState('closed')
      stopPolling()
      socket?.close()
      socket = null
    },
  }
}
