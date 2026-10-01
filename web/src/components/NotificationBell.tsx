"use client";

// S-193: notification bell. Docked in the top bar immediately left of
// the theme switcher. Shows an unread-count badge and a dropdown of
// "something went wrong" alerts (task failed / stuck, agent died, task
// blocked needing input). Inbox messages are a separate surface — this
// bell never shows them.

import { useCallback, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { apiGet, apiPost, type AppNotification } from "../lib/api";
import { useAuth } from "../lib/auth";
import { formatRelativeTime } from "../lib/format";
import {
  isUnread,
  notificationLink,
  notificationMeta,
  notificationSeverityClass,
  unreadCount,
} from "../lib/notifications";

const POLL_MS = 30_000;

export function NotificationBell() {
  const { token, authed } = useAuth();
  const [items, setItems] = useState<AppNotification[]>([]);
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);

  const refresh = useCallback(async () => {
    if (!authed) {
      setItems([]);
      return;
    }
    try {
      const next = await apiGet<AppNotification[]>("/notifications?limit=50", token);
      setItems(Array.isArray(next) ? next : []);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "notification fetch failed");
    }
  }, [authed, token]);

  useEffect(() => {
    void refresh();
    const timer = setInterval(() => void refresh(), POLL_MS);
    return () => clearInterval(timer);
  }, [refresh]);

  useEffect(() => {
    if (!open) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        e.preventDefault();
        setOpen(false);
        buttonRef.current?.focus();
      }
    }
    function onPointerDown(e: MouseEvent | TouchEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("touchstart", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("touchstart", onPointerDown);
    };
  }, [open]);

  const markRead = useCallback(
    async (id: string) => {
      try {
        const updated = await apiPost<AppNotification>(`/notifications/${id}/read`, token, {});
        setItems((prev) => prev.map((n) => (n.id === id ? updated : n)));
      } catch {
        // Transient: the next poll will resync.
      }
    },
    [token],
  );

  const markAllRead = useCallback(async () => {
    try {
      await apiPost("/notifications/read-all", token, {});
      setItems((prev) =>
        prev.map((n) => (isUnread(n.read_at) ? { ...n, read_at: new Date().toISOString() } : n)),
      );
    } catch {
      // Transient: the next poll will resync.
    }
  }, [token]);

  const unread = unreadCount(items);

  return (
    <div className="notif-bell-root" ref={rootRef}>
      <button
        ref={buttonRef}
        type="button"
        className="notif-bell-btn"
        aria-label={unread > 0 ? `Notifications: ${unread} unread` : "Notifications"}
        aria-haspopup="true"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <svg
          viewBox="0 0 24 24"
          width="18"
          height="18"
          aria-hidden="true"
          focusable="false"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9" />
          <path d="M13.7 21a2 2 0 0 1-3.4 0" />
        </svg>
        {unread > 0 ? (
          <span className="notif-badge" aria-hidden="true">
            {unread > 9 ? "9+" : unread}
          </span>
        ) : null}
      </button>
      {open ? (
        <div className="notif-popover" role="dialog" aria-label="Notifications">
          <div className="notif-popover-header">
            <span className="notif-popover-title">Notifications</span>
            {unread > 0 ? (
              <button type="button" className="btn btn-small" onClick={() => void markAllRead()}>
                Mark all read
              </button>
            ) : null}
          </div>
          {error ? <div className="notice error">{error}</div> : null}
          {items.length === 0 ? (
            <div className="notif-empty">Nothing needs you right now.</div>
          ) : (
            <ul className="notif-list">
              {items.map((n) => {
                const meta = notificationMeta(n);
                const href = notificationLink(n);
                return (
                  <li
                    key={n.id}
                    className={`notif-item ${isUnread(n.read_at) ? "notif-unread" : ""} ${notificationSeverityClass(n.severity)}`}
                  >
                    <div className="notif-item-main">
                      <span className={`chip ${meta.className}`}>{meta.label}</span>
                      {href ? (
                        <Link
                          className="notif-message notif-message-link"
                          href={href}
                          onClick={() => {
                            if (isUnread(n.read_at)) void markRead(n.id);
                          }}
                        >
                          {n.message}
                        </Link>
                      ) : (
                        <span className="notif-message">{n.message}</span>
                      )}
                      <span className="notif-time">{formatRelativeTime(n.created_at)}</span>
                    </div>
                    {isUnread(n.read_at) ? (
                      <button type="button" className="btn btn-small" onClick={() => void markRead(n.id)}>
                        Mark read
                      </button>
                    ) : null}
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      ) : null}
    </div>
  );
}
