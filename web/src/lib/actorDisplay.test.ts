// S-235: actor display label helpers for the task page thread and the
// status timeline. The control plane resolves names at read time; these
// tests pin the UI conventions: agents keep the "agent" prefix, thread
// humans fall back to "you", timeline users fall back to the raw id.
import { describe, expect, it } from "vitest";
import { chatSenderAttribution, threadActorLabel, timelineActorLabel } from "./actorDisplay";

describe("threadActorLabel", () => {
  it("names the agent from the resolved display", () => {
    expect(
      threadActorLabel({ from_type: "agent", from_id: "22222222-2222-4222-8222-222222222222", from_display: "Bob" }),
    ).toBe("agent Bob");
  });

  it("falls back to the short id when the agent display is missing", () => {
    expect(threadActorLabel({ from_type: "agent", from_id: "22222222-2222-4222-8222-222222222222" })).toBe(
      "agent 22222222",
    );
  });

  it("shows the resolved first name for humans", () => {
    expect(
      threadActorLabel({ from_type: "user", from_id: "11111111-1111-4111-8111-111111111111", from_display: "Ross" }),
    ).toBe("Ross");
  });

  it("falls back to 'you' when the human display is missing", () => {
    expect(threadActorLabel({ from_type: "user", from_id: "11111111-1111-4111-8111-111111111111" })).toBe("you");
  });

  it("treats an empty display as missing", () => {
    expect(threadActorLabel({ from_type: "user", from_id: "u1", from_display: "" })).toBe("you");
    expect(threadActorLabel({ from_type: "agent", from_id: "a1", from_display: "" })).toBe("agent a1");
  });
});

describe("timelineActorLabel", () => {
  it("names the agent ('agent Bob') from the resolved display", () => {
    expect(
      timelineActorLabel({ actor_type: "agent", actor_id: "22222222-2222-4222-8222-222222222222", actor_display: "Bob" }),
    ).toBe("agent Bob");
  });

  it("shows the user's first name instead of the GUID", () => {
    expect(
      timelineActorLabel({ actor_type: "user", actor_id: "11111111-1111-4111-8111-111111111111", actor_display: "Ross" }),
    ).toBe("Ross");
  });

  it("falls back to the raw id for unresolvable users", () => {
    expect(timelineActorLabel({ actor_type: "user", actor_id: "ghost-id" })).toBe("ghost-id");
  });

  it("falls back to the bare type for unresolvable agents and system actors", () => {
    expect(timelineActorLabel({ actor_type: "agent", actor_id: "ghost-agent" })).toBe("agent");
    expect(timelineActorLabel({ actor_type: "system", actor_id: "00000000-0000-4000-8000-000000000000" })).toBe(
      "system",
    );
  });
});

// S-272: the chat viewer and the message sender are not necessarily the
// same person — a platform admin viewing (or testing) someone else's
// agent must not see their own avatar/name on the owner's messages.
describe("chatSenderAttribution", () => {
  const owner = { id: "user-owner-1", name: "Weng Kee Teh" };
  const admin = { id: "user-admin-2", name: "Ross Brigoli" };

  it("marks the viewer's own messages as 'me' with the resolved name", () => {
    expect(
      chatSenderAttribution({ from_type: "user", from_id: "user-owner-1", from_display: "Weng" }, owner),
    ).toEqual({ isMe: true, name: "Weng" });
  });

  it("shows the actual sender for another human's message (admin viewer)", () => {
    expect(
      chatSenderAttribution({ from_type: "user", from_id: "user-owner-1", from_display: "Weng" }, admin),
    ).toEqual({ isMe: false, name: "Weng" });
  });

  it("falls back to the viewer's own name only for the viewer's own messages", () => {
    expect(chatSenderAttribution({ from_type: "user", from_id: "user-admin-2" }, admin)).toEqual({
      isMe: true,
      name: "Ross Brigoli",
    });
  });

  it("uses a neutral label for unresolvable third-party senders", () => {
    expect(chatSenderAttribution({ from_type: "user", from_id: "user-owner-1" }, admin)).toEqual({
      isMe: false,
      name: "a teammate",
    });
  });

  it("never claims 'me' without a viewer or with a mismatched id", () => {
    expect(chatSenderAttribution({ from_type: "user", from_id: "u1" }, null)).toEqual({
      isMe: false,
      name: "a teammate",
    });
    expect(chatSenderAttribution({ from_type: "user", from_id: "u1" }, { name: "No Id" })).toEqual({
      isMe: false,
      name: "a teammate",
    });
  });

  it("falls back to 'You' for the viewer's own message when profile name is empty", () => {
    expect(chatSenderAttribution({ from_type: "user", from_id: "u1" }, { id: "u1", name: "" })).toEqual({
      isMe: true,
      name: "You",
    });
  });
});
