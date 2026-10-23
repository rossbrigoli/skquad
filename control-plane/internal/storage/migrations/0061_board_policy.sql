-- S-289 (ADR-0016 D8): squad board policy + per-agent self-approval override.
--
-- squads.board_policy is the typed, versioned board-policy document
-- (reopen_cap_per_day / self_approve_default / leader_approval_allowed).
-- '{}' is interpreted as the platform defaults (cap=3, self_approve=true,
-- leader_approval=false) — the column deliberately does NOT overload the
-- untyped operating_model blob (ADR-0016 D8).
--
-- agents.self_approve_allowed is the per-agent override for the squad-level
-- self_approve_default (D2). NULL means "no per-agent override" — the squad
-- default (then the platform default) applies.

ALTER TABLE squads ADD COLUMN IF NOT EXISTS board_policy JSONB NOT NULL DEFAULT '{}';

ALTER TABLE agents ADD COLUMN IF NOT EXISTS self_approve_allowed BOOLEAN NULL;
