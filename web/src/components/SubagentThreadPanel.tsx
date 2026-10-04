// S-163: subagent transparency — the right-hand panel that shows the
// full captured subagent thread (task, assistant turns with tool calls,
// tool results) opened from the 🤖 chip in the chat.
"use client";

import type { SubagentInfo, SubagentThreadEntry } from "../lib/chat";
import { truncateText, uniqueContentKeys } from "../lib/chat";

export function SubagentThreadPanel({
  info,
  onClose,
}: {
  readonly info: SubagentInfo;
  readonly onClose: () => void;
}) {
  // S-189: content-derived stable keys (thread is static history, never reordered).
  const threadKeys = uniqueContentKeys(info.thread, (e) => `${e.role}|${e.name ?? ""}|${e.content}`);
  return (
    <aside className="subagent-panel" role="complementary" aria-label="Subagent thread">
      <header className="subagent-panel-head">
        <span className="subagent-panel-title">🤖 Subagent thread</span>
        <span className="subagent-panel-meta">
          {info.turns} turn{info.turns === 1 ? "" : "s"} · {info.steps.length} tool call
          {info.steps.length === 1 ? "" : "s"}
        </span>
        <button
          type="button"
          className="btn ghost small"
          onClick={onClose}
          aria-label="Close subagent thread"
        >
          ✕
        </button>
      </header>
      <div className="subagent-panel-body">
        {info.thread.length === 0 ? (
          <p className="subagent-empty">No thread captured for this subagent run.</p>
        ) : (
          info.thread.map((entry, idx) => <ThreadEntry key={threadKeys[idx]} entry={entry} />)
        )}
      </div>
    </aside>
  );
}

function ThreadEntry({ entry }: { readonly entry: SubagentThreadEntry }) {
  if (entry.role === "notice") {
    return <p className="subagent-notice">{entry.content}</p>;
  }
  if (entry.role === "user") {
    return (
      <div className="subagent-entry task">
        <div className="subagent-entry-head">📋 Task</div>
        <div className="subagent-entry-text">{entry.content}</div>
      </div>
    );
  }
  if (entry.role === "assistant") {
    return (
      <div className="subagent-entry assistant">
        <div className="subagent-entry-head">💭 Thinking</div>
        {entry.content ? <div className="subagent-entry-text">{entry.content}</div> : null}
        {entry.tool_calls && entry.tool_calls.length > 0 ? (
          <div className="subagent-entry-calls">
            {entry.tool_calls.map((call, idx) => (
              <details key={`${call.name}-${idx}`} className="chat-tool">
                <summary className="chat-tool-summary">
                  <span className="chat-tool-name">🔧 {call.name}</span>
                </summary>
                <pre className="chat-tool-detail mono">
                  {typeof call.arguments === "string"
                    ? call.arguments
                    : JSON.stringify(call.arguments ?? {}, null, 2)}
                </pre>
              </details>
            ))}
          </div>
        ) : null}
      </div>
    );
  }
  return (
    <div className={`subagent-entry tool${entry.ok === false ? " failed" : ""}`}>
      <details>
        <summary className="chat-tool-summary">
          <span className="chat-tool-name">
            {entry.ok === false ? "✗" : "✓"} {entry.name ?? "tool"}
          </span>
          <span className="chat-tool-state">{entry.ok === false ? "failed" : "ok"}</span>
        </summary>
        <pre className="chat-tool-detail mono">{truncateText(entry.content, 4000)}</pre>
      </details>
    </div>
  );
}
