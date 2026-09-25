"use client";

// S-117: tiny theme switcher (S-127: now docked at the right end of the
// top nav bar instead of floating top-right). Three icon buttons act as
// a radiogroup: system / light / dark. Pure theme logic lives in
// lib/theme.ts + ThemeProvider; this is presentation only.

import { useTheme } from "../lib/ThemeProvider";
import type { ThemeMode } from "../lib/theme";

type Option = { value: ThemeMode; label: string; icon: React.ReactNode };

const SUN = (
  <svg viewBox="0 0 24 24" width="14" height="14" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
    <circle cx="12" cy="12" r="4" />
    <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
  </svg>
);

const MOON = (
  <svg viewBox="0 0 24 24" width="14" height="14" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" />
  </svg>
);

const MONITOR = (
  <svg viewBox="0 0 24 24" width="14" height="14" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <rect x="3" y="4" width="18" height="12" rx="2" />
    <path d="M9 20h6M12 16v4" />
  </svg>
);

const OPTIONS: Option[] = [
  { value: "system", label: "System theme", icon: MONITOR },
  { value: "light", label: "Light theme", icon: SUN },
  { value: "dark", label: "Dark theme", icon: MOON },
];

export function ThemeToggle() {
  const { mode, setMode } = useTheme();
  return (
    <div className="theme-toggle" role="radiogroup" aria-label="Theme">
      {OPTIONS.map((opt) => (
        <label
          key={opt.value}
          className={`theme-toggle-btn${mode === opt.value ? " active" : ""}`}
          title={opt.label}
        >
          <input
            type="radio"
            name="theme-toggle"
            value={opt.value}
            checked={mode === opt.value}
            aria-label={opt.label}
            onChange={() => setMode(opt.value)}
          />
          {opt.icon}
        </label>
      ))}
    </div>
  );
}
