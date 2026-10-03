-- S-216: inbox attachments. Agents deliver files (reports, documents,
-- images, scripts) alongside send_inbox messages so the human owner can
-- download them from the Inbox.
--
-- Bytes live in Postgres (bytea), the same posture as the S-194 `uploads`
-- table; the long-term home is object storage with this table kept
-- metadata-only. Unlike S-194 uploads, these rows are keyed by the
-- inbox message they belong to: ON DELETE CASCADE ties attachment
-- lifecycle to the message's lifecycle. Inbox messages are never
-- auto-pruned (S-193: explicit user delete is the only removal path),
-- so deleting the message reclaims the bytes and nothing else ever does.
--
-- content_type is the server-sniffed canonical MIME (never the
-- client-supplied one); sha256 is computed server-side at ingest for
-- integrity display/verification. Executables are rejected at the HTTP
-- boundary (see attachment_validator.go) — nothing ELF/PE/Mach-O/wasm
-- should ever exist in this table.
CREATE TABLE IF NOT EXISTS inbox_attachments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id   uuid NOT NULL REFERENCES inbox_messages(id) ON DELETE CASCADE,
    squad_id     uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    filename     text NOT NULL,
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL,
    sha256       char(64) NOT NULL,
    data         bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inbox_attachments_message ON inbox_attachments (message_id);
CREATE INDEX IF NOT EXISTS idx_inbox_attachments_squad ON inbox_attachments (squad_id, created_at);
