"use client";

// S-241: shared enable/disable toggle switch. Used by the tool tiles
// (Settings > Tools grid) and the built-in tool config page so the
// enable/disable affordance is the same component everywhere. The look
// follows the S-195 dashboard switch language (pill track, sliding
// knob, --accent when on) as a single on/off control.

export function ToggleSwitch({
  checked,
  onToggle,
  label,
  disabled = false,
  className = "",
}: {
  readonly checked: boolean;
  readonly onToggle: (next: boolean) => void;
  readonly label: string;
  readonly disabled?: boolean;
  readonly className?: string;
}) {
  const extraClass = className ? ` ${className}` : "";
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      className={`toggle-switch${checked ? " on" : ""}${extraClass}`}
      onClick={(e) => {
        // Tiles wrap this control next to (never inside) a <Link>; stop
        // the click from bubbling into navigation handlers anyway.
        e.preventDefault();
        e.stopPropagation();
        onToggle(!checked);
      }}
    >
      <span className="toggle-switch-knob" aria-hidden="true" />
    </button>
  );
}
