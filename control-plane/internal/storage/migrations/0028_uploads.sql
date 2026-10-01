-- S-194: image attachments (chat composer + task threads).
-- Bytes live in Postgres for now; the long-term home is object storage
-- (MinIO/S3) with this table kept as metadata-only. The squad_id scopes
-- every read/write in the HTTP layer.
CREATE TABLE IF NOT EXISTS uploads (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    squad_id     uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    uploader_id  uuid,
    filename     text NOT NULL,
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL,
    data         bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_uploads_squad_created ON uploads (squad_id, created_at);
