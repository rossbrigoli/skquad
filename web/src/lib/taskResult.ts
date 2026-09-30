// S-181: the task screen's "Result" section. Prefers the dedicated
// persisted result field; falls back to the latest agent reply in the
// task thread for tasks that predate the field. Pure — unit-testable.

import type { Message, Task } from "./api";
import { messageText } from "./format";

export type TaskResultTone = "done" | "blocked";

export type TaskResultInfo = {
  label: string;
  tone: TaskResultTone;
  text: string;
  at?: string;
};

function infoFor(tone: TaskResultTone, text: string, at?: string): TaskResultInfo {
  return {
    tone,
    text,
    at,
    label: tone === "blocked" ? "Blocked" : "Result",
  };
}

export function taskResultInfo(task: Task, messages: Message[]): TaskResultInfo | null {
  const dedicated = (task.result ?? "").trim();
  if (dedicated) {
    return infoFor(task.result_status === "blocked" ? "blocked" : "done", dedicated, task.result_at);
  }
  // Pre-S-181 tasks: surface the agent's last thread turn for terminal statuses.
  if (task.status !== "done" && task.status !== "blocked") {
    return null;
  }
  const tone: TaskResultTone = task.status === "blocked" ? "blocked" : "done";
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const message = messages[i];
    if (message.from_type !== "agent" || message.type !== "reply") {
      continue;
    }
    const text = messageText(message).trim();
    if (text) {
      return infoFor(tone, text, message.created_at);
    }
  }
  return null;
}
