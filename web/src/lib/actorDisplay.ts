// S-235: actor display labels for the task page thread and status
// timeline. The control plane resolves names at READ time into the
// additive `from_display` / `actor_display` wire fields (agents show
// their current name; users show first name with email local-part
// fallback). These helpers layer the UI's rendering conventions on top:
// agents keep the "agent" prefix, thread humans fall back to "you", and
// nothing ever renders a raw GUID where a name is expected.

type ThreadActor = {
  from_type: string;
  from_id: string;
  from_display?: string;
};

type TimelineActor = {
  actor_type: string;
  actor_id: string;
  actor_display?: string;
};

/** Thread row label: "agent Bob" / "Ross"; legacy rows without a
 *  resolved display keep the pre-S-235 fallbacks ("you", short id). */
export function threadActorLabel(message: ThreadActor): string {
  if (message.from_type === "agent") {
    return message.from_display
      ? `agent ${message.from_display}`
      : `agent ${message.from_id.slice(0, 8)}`;
  }
  // S6606: explicit ternary — an empty display is treated as missing
  // (see actorDisplay tests), so `??` would change behavior.
  return message.from_display ? message.from_display : "you";
}

/** Status-timeline label: "agent Bob" / "Ross". System actors and
 *  unresolvable ids degrade to the bare actor type / id. */
export function timelineActorLabel(entry: TimelineActor): string {
  if (entry.actor_type === "agent") {
    return entry.actor_display ? `agent ${entry.actor_display}` : "agent";
  }
  if (entry.actor_type === "user") {
    return entry.actor_display ? entry.actor_display : entry.actor_id;
  }
  return entry.actor_type;
}
