# Sherlock — skquad Agent Persona Bundle (S-308 / MIG-2)

Version-controlled definition of the `sherlock` agent so the persona lives in the repo, not in pasted chat text.

## Files
- `system-prompt.md` — the assembled skquad SystemPrompt (plain text from "You are Sherlock…" down; the H1 title is not part of the prompt string). ~2.8k chars.
- `agent-spec.json` — API-ready agent definition. `system_prompt` is embedded; `system_prompt_file` notes the canonical source. Regenerate the embedded copy if you edit the .md.
- `README.md` — this file.

## Apply via API (requires owner/admin session)
1. Register the models first (one-time, if not already registered) and note their IDs:
   `POST /api/models` — halogen endpoint `http://192.168.68.137:8123/v1` (OpenAI-compatible; port 8124 = fast llama-server), model `Qwen3.8-Flash-Next`.
2. Replace the placeholders in `agent-spec.json`:
   - `<PLACEHOLDER_AI_MODEL_ID_halogen-qwen3.8-flash-next>` → real AIModel id for halogen Qwen3.8-Flash-Next
   - `<PLACEHOLDER_FALLBACK_AI_MODEL_ID>` → fallback AIModel id (per ADR-0010 grant model)
3. Create the agent (in the target squad — open decision: new `architecture` squad vs existing):
   `POST /api/agents` with the spec body (fields match `domain.Agent`: name, role, system_prompt, ai_model_id, fallback_ai_model_id, thinking_level, storage_enabled, storage_size, idle_timeout_sec).
4. Verify: `GET /api/agents/<id>` — confirm storage PVC requested (S-138) and DeploymentName `skquad-<owner>-agent-sherlock`.
5. Resource grants (git/kanbunny/sonar/browser) are **S-311 / MIG-3 scope** — not part of this bundle.

## Apply via UI
Control UI → Agents → New Agent → paste `system-prompt.md` body into System Prompt, set model/fallback from registered models, thinking level `medium`, storage enabled 2Gi, idle timeout 900s.

## Regenerate from OpenClaw sources
This bundle was assembled from the OpenClaw sherlock agent files:
- `~/.openclaw/workspace-agents/sherlock/SOUL.md` (thinking style, principles)
- `~/.openclaw/workspace-agents/sherlock/IDENTITY.md` (role scope)
- `~/.openclaw/workspace-agents/sherlock/USER.md` (Ross's communication preferences)
- `~/.openclaw/workspace-agents/sherlock/AGENTS.md` (operating rules, red lines — adapted to skquad's gateway-mediated tools and `persist_memory` hygiene)

If the OpenClaw persona changes, re-assemble `system-prompt.md` by hand (target 2500–3500 chars, single string), then re-embed into `agent-spec.json` (see the python snippet in PR #<this PR> history or re-run: read .md from "You are Sherlock", `json.dump` with the fixed fields).

## Provenance
- Assembled: 2026-10-30 (ACDT) by Sherlock (SA-C-persona), card S-308 (Kanbunny skquad `5be694e5-fdad-497c-8183-da1f0186e093`).
- Base commit: `4afd195`.
- Live agent creation NOT performed — requires admin creds (flagged on card). Apply steps above are the contract.
