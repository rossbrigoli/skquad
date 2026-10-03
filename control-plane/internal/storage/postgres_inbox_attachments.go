package storage

// S-216: inbox attachments storage. Bytes live in Postgres (bytea) with
// server-computed metadata, mirroring the S-194 uploads posture. Rows
// cascade with their inbox message (migration 0038), so the explicit
// user delete in deleteInboxMessage is also the attachment reclaim path.

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const inboxAttachmentMetaColumns = `id::text, message_id::text, squad_id::text, filename,
	content_type, size_bytes, sha256, created_at`

func (p *PostgresStore) CreateInboxAttachment(ctx context.Context, a *domain.InboxAttachment) (*domain.InboxAttachment, error) {
	if a.MessageID == "" || a.SquadID == "" {
		return nil, ErrNotFound
	}
	created := *a
	if created.ID == "" {
		created.ID = uuid.NewString()
	}
	created.CreatedAt = time.Now().UTC()
	_, err := p.pool.Exec(ctx, `
		INSERT INTO inbox_attachments (id, message_id, squad_id, filename, content_type, size_bytes, sha256, data)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8)
	`, created.ID, created.MessageID, created.SquadID, created.Filename, created.ContentType,
		created.SizeBytes, created.SHA256, created.Data)
	if err != nil {
		return nil, mapPgErr(err)
	}
	return &created, nil
}

func (p *PostgresStore) GetInboxAttachment(ctx context.Context, id string) (*domain.InboxAttachment, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT `+inboxAttachmentMetaColumns+`, data
		FROM inbox_attachments WHERE id = $1::uuid
	`, id)
	var a domain.InboxAttachment
	var data []byte
	if err := row.Scan(&a.ID, &a.MessageID, &a.SquadID, &a.Filename, &a.ContentType,
		&a.SizeBytes, &a.SHA256, &a.CreatedAt, &data); err != nil {
		return nil, mapPgErr(err)
	}
	a.Data = data
	return &a, nil
}

func (p *PostgresStore) ListInboxAttachmentMeta(ctx context.Context, messageIDs []string) (map[string][]domain.InboxAttachment, error) {
	out := map[string][]domain.InboxAttachment{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT `+inboxAttachmentMetaColumns+`
		FROM inbox_attachments
		WHERE message_id = ANY($1::uuid[])
		ORDER BY created_at ASC
	`, messageIDs)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a domain.InboxAttachment
		if err := rows.Scan(&a.ID, &a.MessageID, &a.SquadID, &a.Filename, &a.ContentType,
			&a.SizeBytes, &a.SHA256, &a.CreatedAt); err != nil {
			return nil, mapPgErr(err)
		}
		out[a.MessageID] = append(out[a.MessageID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgErr(err)
	}
	return out, nil
}
