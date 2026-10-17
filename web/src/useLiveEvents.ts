import { useEffect, useRef, useState } from "react";
import { api, isApiError } from "./api";
import { currentSession } from "./auth";
import { CLOSE_POLICY_VIOLATION, LIVE_APPT_EVENTS, ReconnectBackoff, type ReconnectCause, wsUrl } from "./live";
import { EventType } from "./types";
import type { AppointmentNotice, AvailabilityNotice, ConfigNotice, EventTypeCode, LiveMessage } from "./types";

export type LiveHandlers = {
  onAppointment: (type: EventTypeCode, notice: AppointmentNotice) => void;
  onAvailability: (notice: AvailabilityNotice) => void;
  onConfig: (notice: ConfigNotice) => void;
  /** Called after every reconnect (welcome frame): events sent while offline are lost, so refetch state. */
  onResync: () => void;
  /**
   * The ticket request was refused with 401 (after api.ts's one refresh) for session `key`. Only
   * sent while `key` is still this tab's session (not after the tab switched sessions meanwhile)
   * and the socket was not disposed.
   */
  onUnauthorized: (key: string) => void;
  /**
   * The server closed the socket over the session itself (close 1008: role, dealership, or customer
   * link changed, or signed out elsewhere): re-read the user. A revoked token then fails with 401.
   */
  onSessionChanged?: () => void;
};

export type LiveState = "connecting" | "open" | "closed";

/**
 * Keeps one WebSocket open per session (`sessionKey`, auth.ts: a refreshed access token is the
 * same session and keeps the socket; each ticket is requested with the token current then).
 * Opens with a short-lived ticket (never the access token in the URL), follows the selected
 * dealership via a subscribe message, reconnects with backoff (reset only by the server's
 * "connected" frame, long after "try again later"), and asks the app to resync after each
 * reconnect.
 */
export function useLiveEvents(sessionKey: string | undefined, dealershipId: string, handlers: LiveHandlers): LiveState {
  const [state, setState] = useState<LiveState>("connecting");
  const handlersRef = useRef(handlers);
  handlersRef.current = handlers;
  const dealershipRef = useRef(dealershipId);
  dealershipRef.current = dealershipId;
  const socketRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    if (!sessionKey) {
      setState("closed");
      return;
    }
    // The session this socket belongs to.
    const key = sessionKey;
    let disposed = false;
    let reconnectTimer: number | undefined;
    const backoff = new ReconnectBackoff();
    let everConnected = false;

    const scheduleReconnect = (cause?: ReconnectCause) => {
      if (disposed) return;
      reconnectTimer = window.setTimeout(connect, backoff.next(cause));
    };

    const onWelcome = () => {
      backoff.connected();
      setState("open");
      if (everConnected) handlersRef.current.onResync();
      everConnected = true;
    };

    const dispatch = (raw: string) => {
      let msg: LiveMessage;
      try {
        msg = JSON.parse(raw) as LiveMessage;
      } catch {
        return;
      }
      const h = handlersRef.current;
      if (msg.type === EventType.Connected) onWelcome();
      else if (msg.type === EventType.AvailabilityChanged) h.onAvailability(msg.payload as AvailabilityNotice);
      else if (msg.type === EventType.ConfigUpdated) h.onConfig(msg.payload as ConfigNotice);
      else if (LIVE_APPT_EVENTS.has(msg.type)) h.onAppointment(msg.type, msg.payload as AppointmentNotice);
    };

    async function connect() {
      if (disposed) return;
      setState("connecting");
      let ticket: string;
      try {
        ticket = (await api.wsTicket()).ticket;
      } catch (err) {
        // Torn down meanwhile (signed out, or another session adopted): nothing of it may act.
        if (disposed) return;
        if (isApiError(err) && err.status === 401) {
          setState("closed");
          // A 401 for a session this tab no longer uses must not end the session it uses now.
          if (currentSession()?.key === key) handlersRef.current.onUnauthorized(key);
          return;
        }
        setState("closed");
        scheduleReconnect(isApiError(err) ? { status: err.status, retryAfter: err.retryAfter } : undefined);
        return;
      }
      if (disposed) return;
      const socket = new WebSocket(wsUrl(window.location, ticket, dealershipRef.current || undefined));
      socketRef.current = socket;
      // No reset on onopen: only the hub's "connected" frame proves the session is live. Over the
      // per-user cap the upgrade is refused with HTTP 429, which the browser reports as a close
      // 1006 without onopen (or, racing, an upgraded socket closed with 1013); either way the
      // failure count keeps growing the backoff (1013 waits at least LONG_BACKOFF_MS).
      socket.onclose = (ev) => {
        if (socketRef.current === socket) socketRef.current = null;
        setState("closed");
        if (ev.code === CLOSE_POLICY_VIOLATION && !disposed) handlersRef.current.onSessionChanged?.();
        scheduleReconnect({ closeCode: ev.code });
      };
      socket.onmessage = (ev) => dispatch(ev.data as string);
    }

    void connect();
    return () => {
      disposed = true;
      if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer);
      socketRef.current?.close();
      socketRef.current = null;
    };
  }, [sessionKey]);

  // Follow the selected dealership without reconnecting.
  useEffect(() => {
    const socket = socketRef.current;
    if (socket?.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify({ type: "subscribe", dealershipId }));
    }
  }, [dealershipId, state]);

  return state;
}
