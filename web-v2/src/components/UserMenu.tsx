"use client";

// S-117: bottom-left user menu. The user's name is a button that pops up
// their profile (avatar placeholder, name, role, sign out) — replacing the
// old Settings → Session tab. Closes on Escape (focus returns to the
// trigger) and on outside click.

import { useEffect, useRef, useState } from "react";
import type { ApiUser } from "../lib/api";
import { displayRole, initialsFor } from "../lib/usermenu";

export function UserMenu({
  user,
  onSignOut,
}: {
  user: ApiUser | null;
  onSignOut: () => void;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const popoverRef = useRef<HTMLDivElement>(null);

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

  // Move focus into the popover so keyboard users aren't left behind the
  // (now hidden) panel when it opens.
  useEffect(() => {
    if (open) popoverRef.current?.focus();
  }, [open]);

  if (!user) return <span>not signed in</span>;

  return (
    <div className="user-menu" ref={rootRef}>
      {open ? (
        <div className="user-popover" ref={popoverRef} role="dialog" aria-label="User profile" tabIndex={-1}>
          <div className="user-popover-header">
            {/* Avatar placeholder — initials until real avatars exist. */}
            <span className="avatar-placeholder" aria-hidden="true">
              {initialsFor(user.name, user.email)}
            </span>
            <div className="user-popover-id">
              <div className="user-popover-name">{user.name || user.email}</div>
              <div className="user-popover-role">{displayRole(user.role)}</div>
            </div>
          </div>
          {user.name && user.email ? (
            <div className="user-popover-email">{user.email}</div>
          ) : null}
          <button type="button" className="btn user-signout" onClick={onSignOut}>
            Sign out
          </button>
        </div>
      ) : null}
      <button
        ref={buttonRef}
        type="button"
        className="user-menu-button"
        aria-haspopup="dialog"
        aria-expanded={open}
        title="Profile and sign out"
        onClick={() => setOpen((o) => !o)}
      >
        <span className="user-menu-name">{user.name || user.email}</span>
        <span className="user-menu-chevron" aria-hidden="true">
          {open ? "▾" : "▴"}
        </span>
      </button>
    </div>
  );
}
