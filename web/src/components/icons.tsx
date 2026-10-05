// S-140: inline SVG icons for the primary rail menu. Stroke-based 16px
// glyphs that inherit `currentColor`, so they follow the nav item's theme
// state (muted → bright) without extra CSS. No icon dependency added —
// The web app deliberately stays on a tiny dependency footprint.

type IconProps = { readonly size?: number };

function base(size = 16) {
  return {
    width: size,
    height: size,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 2,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    "aria-hidden": true as const,
    focusable: "false" as const,
  };
}

export function IconDashboard({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <rect x="3" y="3" width="7" height="9" rx="1" />
      <rect x="14" y="3" width="7" height="5" rx="1" />
      <rect x="14" y="12" width="7" height="9" rx="1" />
      <rect x="3" y="16" width="7" height="5" rx="1" />
    </svg>
  );
}

export function IconInbox({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <polyline points="22 12 16 12 14 15 10 15 8 12 2 12" />
      <path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z" />
    </svg>
  );
}

// S-207: Gmail/Outlook-style read-state markers for inbox rows.
// Unread = closed, filled envelope (the flap crease is punched out of
// the fill with an even-odd rule so it reads on any theme). Read =
// the same envelope as a plain outline.
export function IconEnvelopeUnread({ size }: IconProps) {
  return (
    <svg {...base(size)} fill="currentColor" stroke="none">
      <path
        fillRule="evenodd"
        clipRule="evenodd"
        d="M2 6a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V6zm2 -.4 8 6.2 8-6.2v1.8l-8 6.2-8-6.2z"
      />
    </svg>
  );
}

export function IconEnvelopeRead({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <rect x="2" y="4" width="20" height="16" rx="2" />
      <path d="m22 7-8.97 5.7a1.94 1.94 0 0 1-2.06 0L2 7" />
    </svg>
  );
}

export function IconSquads({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
      <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </svg>
  );
}

export function IconAgents({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <rect x="4" y="7" width="16" height="12" rx="2" />
      <path d="M12 7V4" />
      <circle cx="12" cy="2.5" r="1.2" />
      <circle cx="9" cy="12.5" r="1" fill="currentColor" stroke="none" />
      <circle cx="15" cy="12.5" r="1" fill="currentColor" stroke="none" />
      <path d="M9 16h6" />
      <path d="M1.5 11v4" />
      <path d="M22.5 11v4" />
    </svg>
  );
}

// S-172: icons for squad-section tabs and nav sub-items. Same stroke
// style as the rail icons; sized down by callers.
// S-211: robot glyph for the squad Overview agent tiles.
export function IconRobot({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <rect x="5" y="8" width="14" height="11" rx="2" />
      <line x1="12" y1="4" x2="12" y2="8" />
      <circle cx="12" cy="3.5" r="1" />
      <circle cx="9.5" cy="13" r="1.2" fill="currentColor" stroke="none" />
      <circle cx="14.5" cy="13" r="1.2" fill="currentColor" stroke="none" />
      <line x1="9" y1="16.5" x2="15" y2="16.5" />
      <line x1="2.5" y1="11.5" x2="5" y2="11.5" />
      <line x1="19" y1="11.5" x2="21.5" y2="11.5" />
    </svg>
  );
}

export function IconOverview({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <circle cx="12" cy="12" r="9" />
      <circle cx="12" cy="12" r="2.5" fill="currentColor" stroke="none" />
      <path d="M12 3v3.5" />
      <path d="M12 17.5V21" />
      <path d="M3 12h3.5" />
      <path d="M17.5 12H21" />
    </svg>
  );
}

export function IconBoard({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <rect x="3" y="3" width="5.5" height="18" rx="1" />
      <rect x="9.75" y="3" width="5.5" height="13" rx="1" />
      <rect x="16.5" y="3" width="4.5" height="8" rx="1" />
    </svg>
  );
}

export function IconContext({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <polyline points="14 2 14 8 20 8" />
      <path d="M8 13h8" />
      <path d="M8 17h5" />
    </svg>
  );
}

export function IconCosts({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <line x1="12" y1="2" x2="12" y2="22" />
      <path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6" />
    </svg>
  );
}

export function IconSettings({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </svg>
  );
}

export function IconAbout({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <circle cx="12" cy="12" r="10" />
      <line x1="12" y1="16" x2="12" y2="12" />
      <line x1="12" y1="8" x2="12.01" y2="8" />
    </svg>
  );
}

export function IconCloud({ size }: IconProps) {
  return (
    <svg {...base(size)}>
      <path d="M17.5 19a4.5 4.5 0 1 0-.42-8.98 6 6 0 1 0-11.06 3.05A3.5 3.5 0 0 0 7 19.5h10.5z" />
    </svg>
  );
}
