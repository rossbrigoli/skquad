// S-304 follow-up: the slash-command autocomplete popup that floats
// above the chat composer.
//
// Presentation only — all matching lives in lib/slashCommands.ts so the
// rules are unit-testable. The parent owns the keyboard handling and the
// selected index; this renders the list and exposes it to assistive tech
// via the combobox pattern (aria-activedescendant on the textarea).

import type { SlashCommand } from "../lib/slashCommands";

export const SLASH_LISTBOX_ID = "slash-command-listbox";

export function slashOptionId(index: number): string {
  return `slash-command-option-${index}`;
}

export function SlashSuggestions({
  commands,
  selectedIndex,
  onPick,
  onHover,
}: {
  readonly commands: readonly SlashCommand[];
  readonly selectedIndex: number;
  readonly onPick: (cmd: SlashCommand) => void;
  readonly onHover: (index: number) => void;
}) {
  return (
    <div className="slash-suggestions" role="presentation">
      <div
        id={SLASH_LISTBOX_ID}
        className="slash-suggestions-list"
        role="listbox"
        aria-label="Slash commands"
      >
        {commands.map((cmd, i) => {
          const selected = i === selectedIndex;
          return (
            <button
              key={cmd.command}
              id={slashOptionId(i)}
              type="button"
              role="option"
              aria-selected={selected}
              className={selected ? "slash-suggestion selected" : "slash-suggestion"}
              // mousedown so the pick lands before the textarea's blur
              // tears the popup down.
              onMouseDown={(e) => {
                e.preventDefault();
                onPick(cmd);
              }}
              onMouseEnter={() => onHover(i)}
            >
              <span className="slash-suggestion-glyph" aria-hidden="true">
                {cmd.glyph}
              </span>
              <span className="slash-suggestion-body">
                <span className="slash-suggestion-command">{cmd.command}</span>
                <span className="slash-suggestion-description">{cmd.description}</span>
              </span>
              {cmd.destructive ? (
                <span className="slash-suggestion-badge" title="Asks for confirmation">
                  confirm
                </span>
              ) : null}
            </button>
          );
        })}
      </div>
      <div className="slash-suggestions-hint">
        ↑↓ to choose · Tab to complete · Enter to run · Esc to dismiss
      </div>
    </div>
  );
}
