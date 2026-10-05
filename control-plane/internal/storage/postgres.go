package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const (
	secondsFormat = "%d seconds"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationLockKey int64 = 0x5A51444144
const agentWorkNotifyChannel = "skquad_agent_work"

type migrationFile struct {
	Version  string
	SQL      string
	Checksum string
}

// PostgresStore persists control-plane data in Postgres.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore connects to Postgres, pings it, and applies embedded
// migrations under a Postgres advisory lock.
func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	store := &PostgresStore{pool: pool}
	if err := store.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database pool.
func (p *PostgresStore) Close() {
	p.pool.Close()
}

func (p *PostgresStore) migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("postgres: acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if err := ensureSchemaMigrations(ctx, conn); err != nil {
		return err
	}
	migrations, err := readMigrationFiles()
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		applied, err := migrationApplied(ctx, conn, migration)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("postgres: begin migration %s: %w", migration.Version, err)
		}
		if err := applyMigrationTx(ctx, tx, migration); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("postgres: commit migration %s: %w", migration.Version, err)
		}
	}
	return nil
}

func readMigrationFiles() ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("postgres: read migrations: %w", err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	migrations := []migrationFile{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("postgres: read migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migrationFile{
			Version:  entry.Name(),
			SQL:      string(body),
			Checksum: migrationChecksum(body),
		})
	}
	return migrations, nil
}

func migrationChecksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func vectorLiteral(values []float64) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.FormatFloat(value, 'g', -1, 64))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func parseVectorText(raw string) []float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = strings.TrimPrefix(strings.TrimSuffix(raw, "]"), "[")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	values := make([]float64, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil
		}
		values = append(values, value)
	}
	return values
}

func ensureSchemaMigrations(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("postgres: ensure schema_migrations: %w", err)
	}
	return nil
}

func migrationApplied(ctx context.Context, conn *pgxpool.Conn, migration migrationFile) (bool, error) {
	var checksum string
	err := conn.QueryRow(ctx, `
		SELECT checksum
		FROM schema_migrations
		WHERE version = $1
	`, migration.Version).Scan(&checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: read migration ledger %s: %w", migration.Version, err)
	}
	if checksum != migration.Checksum {
		return false, fmt.Errorf("postgres: migration %s checksum mismatch", migration.Version)
	}
	return true, nil
}

func applyMigrationTx(ctx context.Context, tx pgx.Tx, migration migrationFile) error {
	if _, err := tx.Exec(ctx, migration.SQL); err != nil {
		return fmt.Errorf("postgres: apply migration %s: %w", migration.Version, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schema_migrations (version, checksum)
		VALUES ($1, $2)
	`, migration.Version, migration.Checksum); err != nil {
		return fmt.Errorf("postgres: record migration %s: %w", migration.Version, err)
	}
	return nil
}

func (p *PostgresStore) enqueueKubernetesOutboxTx(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID, operation string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("postgres: marshal kubernetes outbox payload: %w", err)
	}
	// Coalesce: every CR write payload is full-state and idempotent, so any
	// still-pending/failed event for the same aggregate is superseded by the
	// one being enqueued. Without this, a producer-rate burst (e.g. a runtime
	// retry loop) grows the backlog unboundedly and delays the one wake that
	// matters. Same-tx UPDATE keeps ordering-safe: the INSERT below is the
	// only live event for this aggregate when the worker leases it.
	if _, err := tx.Exec(ctx, `
		UPDATE kubernetes_outbox
		SET status = 'applied',
		    last_error = 'coalesced: superseded by newer event at enqueue',
		    updated_at = now()
		WHERE aggregate_type = $1 AND aggregate_id = $2
		  AND status IN ('pending', 'failed')
	`, aggregateType, aggregateID); err != nil {
		return mapPgErr(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO kubernetes_outbox (aggregate_type, aggregate_id, operation, payload)
		VALUES ($1, $2, $3, $4)
	`, aggregateType, aggregateID, operation, raw); err != nil {
		return mapPgErr(err)
	}
	return nil
}

func (p *PostgresStore) enqueueSquadOutboxTx(ctx context.Context, tx pgx.Tx, operation string, squad *domain.Squad) error {
	return p.enqueueKubernetesOutboxTx(ctx, tx, domain.KubernetesAggregateSquad, squad.ID, operation, domain.KubernetesOutboxPayload{Squad: squad})
}

func (p *PostgresStore) enqueueAgentOutboxTx(ctx context.Context, tx pgx.Tx, operation string, agent *domain.Agent) error {
	identity, err := getAgentIdentityTx(ctx, tx, agent.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if errors.Is(err, ErrNotFound) {
		identity = nil
	}
	return p.enqueueKubernetesOutboxTx(ctx, tx, domain.KubernetesAggregateAgent, agent.ID, operation, domain.KubernetesOutboxPayload{
		Agent:    agent,
		Identity: identity,
	})
}

func getAgentTx(ctx context.Context, tx pgx.Tx, id string) (*domain.Agent, error) {
	row := tx.QueryRow(ctx, `
		SELECT id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		       coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
		FROM agents
		WHERE id = $1
	`, id)
	return scanAgent(row)
}

func getAgentIdentityTx(ctx context.Context, tx pgx.Tx, agentID string) (*domain.AgentIdentity, error) {
	row := tx.QueryRow(ctx, `
		SELECT id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at, gateway_key_token, gateway_key_status
		FROM agent_identities
		WHERE agent_id = $1
	`, agentID)
	return scanAgentIdentity(row)
}

func (p *PostgresStore) GetUser(ctx context.Context, id string) (*domain.User, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, coalesce(oidc_issuer, ''), coalesce(oidc_subject, ''), email, email_verified, name, role, created_at
		FROM users
		WHERE id = $1
	`, id)
	return scanUser(row)
}

func (p *PostgresStore) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, coalesce(oidc_issuer, ''), coalesce(oidc_subject, ''), email, email_verified, name, role, created_at
		FROM users
		WHERE email = lower($1)
		ORDER BY created_at
		LIMIT 1
	`, email)
	return scanUser(row)
}

func (p *PostgresStore) UpsertUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	role := u.Role
	if role == "" {
		role = domain.RoleUser
	}
	issuer := strings.TrimSpace(u.OIDCIssuer)
	subject := strings.TrimSpace(u.OIDCSubject)
	if issuer == "" || subject == "" {
		existing, err := p.GetUserByEmail(ctx, u.Email)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if err == nil {
			row := p.pool.QueryRow(ctx, `
				UPDATE users
				SET name = $2
				WHERE id = $1
				RETURNING id::text, coalesce(oidc_issuer, ''), coalesce(oidc_subject, ''), email, email_verified, name, role, created_at
			`, existing.ID, u.Name)
			return scanUser(row)
		}
		row := p.pool.QueryRow(ctx, `
			INSERT INTO users (email, email_verified, name, role)
			VALUES (lower($1), $2, $3, $4)
			RETURNING id::text, coalesce(oidc_issuer, ''), coalesce(oidc_subject, ''), email, email_verified, name, role, created_at
		`, u.Email, u.EmailVerified, u.Name, role)
		return scanUser(row)
	}
	row := p.pool.QueryRow(ctx, `
		INSERT INTO users (oidc_issuer, oidc_subject, email, email_verified, name, role)
		VALUES (nullif($1, ''), nullif($2, ''), lower($3), $4, $5, $6)
		ON CONFLICT (oidc_issuer, oidc_subject)
		WHERE oidc_issuer IS NOT NULL AND oidc_subject IS NOT NULL
		DO UPDATE
		SET email = EXCLUDED.email,
		    email_verified = EXCLUDED.email_verified,
		    name = EXCLUDED.name
		RETURNING id::text, coalesce(oidc_issuer, ''), coalesce(oidc_subject, ''), email, email_verified, name, role, created_at
	`, issuer, subject, u.Email, u.EmailVerified, u.Name, role)
	return scanUser(row)
}

func (p *PostgresStore) SetUserRole(ctx context.Context, id string, role domain.Role) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE users
		SET role = $2
		WHERE id = $1
	`, id, role)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) ListUsers(ctx context.Context) ([]*domain.User, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, coalesce(oidc_issuer, ''), coalesce(oidc_subject, ''), email, email_verified, name, role, created_at
		FROM users
		ORDER BY email
	`)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	users := make([]*domain.User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, mapPgErr(rows.Err())
}

func (p *PostgresStore) CreateSquad(ctx context.Context, s *domain.Squad) (*domain.Squad, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		INSERT INTO squads (name, mission, prompt, operating_model, owner_id, namespace, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, name, mission, prompt, operating_model, owner_id::text, namespace, status, created_at, updated_at
	`, s.Name, s.Mission, s.Prompt, defaultJSON(s.OperatingModel, "{}"), s.OwnerID, s.Namespace, defaultSquadStatus(s.Status))
	created, err := scanSquad(row)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO kanban_boards (squad_id)
		VALUES ($1)
	`, created.ID); err != nil {
		return nil, mapPgErr(err)
	}
	// S-158: a squad born with a prompt (e.g. from a template) records its
	// first revision in the same transaction as the row insert.
	if intent := DrainPromptRevision(ctx); intent != nil && created.Prompt != "" {
		rev := buildPromptRevision(intent, created.Prompt)
		rev.ScopeID = created.ID
		if err := insertPromptRevisionTx(ctx, tx, rev); err != nil {
			return nil, err
		}
	}
	if err := p.enqueueSquadOutboxTx(ctx, tx, domain.KubernetesOpUpsertSquad, created); err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetSquad(ctx context.Context, id string) (*domain.Squad, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, name, mission, prompt, operating_model, owner_id::text, namespace, status, created_at, updated_at
		FROM squads
		WHERE id = $1
	`, id)
	return scanSquad(row)
}

func (p *PostgresStore) GetSquadByName(ctx context.Context, ownerID, name string) (*domain.Squad, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, name, mission, prompt, operating_model, owner_id::text, namespace, status, created_at, updated_at
		FROM squads
		WHERE owner_id = $1 AND lower(name) = lower($2)
	`, ownerID, name)
	return scanSquad(row)
}

// SquadNamespaceExists reports whether any squad already owns the namespace
// (S-156 namespace de-duplication).
func (p *PostgresStore) SquadNamespaceExists(ctx context.Context, namespace string) (bool, error) {
	var exists bool
	err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM squads WHERE namespace = $1)`, namespace).Scan(&exists)
	if err != nil {
		return false, mapPgErr(err)
	}
	return exists, nil
}

// GetAgentByNameForOwner finds an agent by name across every squad owned by
// ownerID (S-156 per-user agent name uniqueness).
func (p *PostgresStore) GetAgentByNameForOwner(ctx context.Context, ownerID, name string) (*domain.Agent, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT a.id::text, a.squad_id::text, a.name, a.role, a.system_prompt, coalesce(a.identity_id::text, ''),
		       coalesce(a.ai_model_id::text, ''), coalesce(a.fallback_ai_model_id::text, ''), a.permissions, a.idle_timeout_sec, a.thinking_level, a.storage_enabled, a.storage_size, a.deployment_name, a.chat_reset_at, a.status, a.created_at, a.updated_at
		FROM agents a
		JOIN squads s ON s.id = a.squad_id
		WHERE s.owner_id = $1 AND lower(a.name) = lower($2)
		LIMIT 1`, ownerID, name)
	return scanAgent(row)
}

func (p *PostgresStore) UpdateSquad(ctx context.Context, s *domain.Squad) (*domain.Squad, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	// S-PROMPT WP2: a declared prompt revision lands in the same
	// transaction as the squad row update (append-only history).
	if intent := DrainPromptRevision(ctx); intent != nil {
		if err := p.appendPromptRevisionTx(ctx, tx, intent, `SELECT prompt FROM squads WHERE id = $1`, s.Prompt, s.ID); err != nil {
			return nil, err
		}
	}

	row := tx.QueryRow(ctx, `
		UPDATE squads
		SET name = $2,
		    mission = $3,
		    prompt = $4,
		    operating_model = $5,
		    status = $6,
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, name, mission, prompt, operating_model, owner_id::text, namespace, status, created_at, updated_at
	`, s.ID, s.Name, s.Mission, s.Prompt, defaultJSON(s.OperatingModel, "{}"), defaultSquadStatus(s.Status))
	updated, err := scanSquad(row)
	if err != nil {
		return nil, err
	}
	if err := p.enqueueSquadOutboxTx(ctx, tx, domain.KubernetesOpUpsertSquad, updated); err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, updated.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) DeleteSquad(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		SELECT id::text, name, mission, prompt, operating_model, owner_id::text, namespace, status, created_at, updated_at
		FROM squads
		WHERE id = $1
	`, id)
	squad, err := scanSquad(row)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		       coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
		FROM agents
		WHERE squad_id = $1
		ORDER BY name
	`, id)
	if err != nil {
		return mapPgErr(err)
	}
	defer rows.Close()
	agents := make([]*domain.Agent, 0)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return err
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return mapPgErr(err)
	}
	for _, agent := range agents {
		if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpDeleteAgent, agent); err != nil {
			return err
		}
	}
	if err := p.enqueueSquadOutboxTx(ctx, tx, domain.KubernetesOpDeleteSquad, squad); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM squads WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) ListSquads(ctx context.Context, ownerID string) ([]*domain.Squad, error) {
	var rows pgx.Rows
	var err error
	if ownerID == "" {
		rows, err = p.pool.Query(ctx, `
			SELECT id::text, name, mission, prompt, operating_model, owner_id::text, namespace, status, created_at, updated_at
			FROM squads
			ORDER BY name
		`)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT id::text, name, mission, prompt, operating_model, owner_id::text, namespace, status, created_at, updated_at
			FROM squads
			WHERE owner_id = $1
			ORDER BY name
		`, ownerID)
	}
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	squads := make([]*domain.Squad, 0)
	for rows.Next() {
		s, err := scanSquad(rows)
		if err != nil {
			return nil, err
		}
		squads = append(squads, s)
	}
	return squads, mapPgErr(rows.Err())
}

func (p *PostgresStore) CreateAgent(ctx context.Context, a *domain.Agent) (*domain.Agent, error) {
	if err := validateAgentModelBinding(a); err != nil {
		return nil, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	// S-156: agent names are unique per user (across all their squads).
	// The legacy per-squad DB constraint stays as a backstop; the per-owner
	// rule needs the squads join, so it is checked here in-tx.
	var squadOwner string
	if err := tx.QueryRow(ctx, `SELECT owner_id::text FROM squads WHERE id = $1`, a.SquadID).Scan(&squadOwner); err != nil {
		return nil, mapPgErr(err)
	}
	var clashID string
	if err := tx.QueryRow(ctx, `
		SELECT a.id::text FROM agents a
		JOIN squads s ON s.id = a.squad_id
		WHERE s.owner_id = $1 AND lower(a.name) = lower($2)
		LIMIT 1`, squadOwner, a.Name).Scan(&clashID); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, mapPgErr(err)
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO agents (squad_id, name, role, system_prompt, ai_model_id, fallback_ai_model_id, permissions, idle_timeout_sec, status, storage_enabled, storage_size, deployment_name, thinking_level)
		VALUES ($1, $2, $3, $4, nullif($5, '')::uuid, nullif($6, '')::uuid, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		          coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
	`, a.SquadID, a.Name, a.Role, a.SystemPrompt, a.AIModelID, a.FallbackAIModelID, defaultJSON(a.Permissions, "[]"), a.IdleTimeoutSec, defaultAgentStatus(a.Status), a.StorageEnabled, defaultStorageSize(a.StorageSize), a.DeploymentName, a.ThinkingLevel)
	created, err := scanAgent(row)
	if err != nil {
		return nil, err
	}
	// S-PROMPT WP2: an agent born with a prompt records its first revision
	// in the same transaction as the row insert.
	if intent := DrainPromptRevision(ctx); intent != nil && created.SystemPrompt != "" {
		rev := buildPromptRevision(intent, created.SystemPrompt)
		rev.ScopeID = created.ID
		if err := insertPromptRevisionTx(ctx, tx, rev); err != nil {
			return nil, err
		}
	}
	if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpUpsertAgent, created); err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		       coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
		FROM agents
		WHERE id = $1
	`, id)
	return scanAgent(row)
}

func (p *PostgresStore) UpdateAgent(ctx context.Context, a *domain.Agent) (*domain.Agent, error) {
	if err := validateAgentModelBinding(a); err != nil {
		return nil, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	// S-PROMPT WP2: the agent tier's revision lands in the same
	// transaction as the agent row update.
	if intent := DrainPromptRevision(ctx); intent != nil {
		if err := p.appendPromptRevisionTx(ctx, tx, intent, `SELECT system_prompt FROM agents WHERE id = $1`, a.SystemPrompt, a.ID); err != nil {
			return nil, err
		}
	}

	row := tx.QueryRow(ctx, `
		UPDATE agents
		SET name = $2,
		    role = $3,
		    system_prompt = $4,
		    ai_model_id = nullif($8, '')::uuid,
		    fallback_ai_model_id = nullif($9, '')::uuid,
		    permissions = $5,
		    idle_timeout_sec = $6,
		    status = $7,
		    storage_enabled = $10,
		    storage_size = $11,
		    thinking_level = $12,
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		          coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
	`, a.ID, a.Name, a.Role, a.SystemPrompt, defaultJSON(a.Permissions, "[]"), a.IdleTimeoutSec, defaultAgentStatus(a.Status), a.AIModelID, a.FallbackAIModelID, a.StorageEnabled, defaultStorageSize(a.StorageSize), a.ThinkingLevel)
	updated, err := scanAgent(row)
	if err != nil {
		return nil, err
	}
	if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpUpsertAgent, updated); err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, updated.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) DeleteAgent(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		SELECT id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		       coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
		FROM agents
		WHERE id = $1
	`, id)
	agent, err := scanAgent(row)
	if err != nil {
		return err
	}
	if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpDeleteAgent, agent); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM agents WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) ListAgents(ctx context.Context, squadID string) ([]*domain.Agent, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		       coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
		FROM agents
		WHERE squad_id = $1
		ORDER BY name
	`, squadID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	agents := make([]*domain.Agent, 0)
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, mapPgErr(rows.Err())
}

func (p *PostgresStore) SetAgentStatus(ctx context.Context, id string, status domain.AgentStatus) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE agents
		SET status = $2, updated_at = now()
		WHERE id = $1
		RETURNING id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		          coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
	`, id, status)
	agent, err := scanAgent(row)
	if err != nil {
		return err
	}
	if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpUpsertAgent, agent); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) CreateAgentIdentity(ctx context.Context, i *domain.AgentIdentity) (*domain.AgentIdentity, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		INSERT INTO agent_identities (agent_id, credential_ref, credential_hash, virtual_key_ref, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at, gateway_key_token, gateway_key_status
	`, i.AgentID, i.CredentialRef, i.CredentialHash, nullableText(i.VirtualKeyRef), i.CreatedBy)
	created, err := scanAgentIdentity(row)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE agents
		SET identity_id = $2, updated_at = now()
		WHERE id = $1
	`, created.AgentID, created.ID); err != nil {
		return nil, mapPgErr(err)
	}
	agent, err := getAgentTx(ctx, tx, created.AgentID)
	if err != nil {
		return nil, err
	}
	if err := p.enqueueKubernetesOutboxTx(ctx, tx, domain.KubernetesAggregateAgent, agent.ID, domain.KubernetesOpUpsertAgent, domain.KubernetesOutboxPayload{
		Agent:    agent,
		Identity: created,
	}); err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetAgentIdentity(ctx context.Context, agentID string) (*domain.AgentIdentity, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at, gateway_key_token, gateway_key_status
		FROM agent_identities
		WHERE agent_id = $1
	`, agentID)
	return scanAgentIdentity(row)
}

func (p *PostgresStore) RotateAgentIdentity(ctx context.Context, agentID string, credentialRef string, credentialHash string, virtualKeyRef string) (*domain.AgentIdentity, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE agent_identities
		SET credential_ref = $2,
		    credential_hash = $3,
		    virtual_key_ref = $4,
		    gateway_key_token = '',
		    gateway_key_status = 'none',
		    rotated_at = now()
		WHERE agent_id = $1
		RETURNING id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at, gateway_key_token, gateway_key_status
	`, agentID, credentialRef, credentialHash, nullableText(virtualKeyRef))
	identity, err := scanAgentIdentity(row)
	if err != nil {
		return nil, err
	}
	agent, err := getAgentTx(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}
	if err := p.enqueueKubernetesOutboxTx(ctx, tx, domain.KubernetesAggregateAgent, agent.ID, domain.KubernetesOpUpsertAgent, domain.KubernetesOutboxPayload{
		Agent:    agent,
		Identity: identity,
	}); err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, identity.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return identity, nil
}

func (p *PostgresStore) SetAgentIdentityGatewayKey(ctx context.Context, agentID string, token string, status domain.GatewayKeyStatus) (*domain.AgentIdentity, error) {
	row := p.pool.QueryRow(ctx, `
		UPDATE agent_identities
		SET gateway_key_token = $2,
		    gateway_key_status = $3
		WHERE agent_id = $1
		RETURNING id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at, gateway_key_token, gateway_key_status
	`, agentID, token, string(status))
	return scanAgentIdentity(row)
}

func (p *PostgresStore) ListAllAgents(ctx context.Context) ([]*domain.Agent, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, squad_id::text, name, role, system_prompt, coalesce(identity_id::text, ''),
		       coalesce(ai_model_id::text, ''), coalesce(fallback_ai_model_id::text, ''), permissions, idle_timeout_sec, thinking_level, storage_enabled, storage_size, deployment_name, chat_reset_at, status, created_at, updated_at
		FROM agents
		ORDER BY name
	`)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	agents := make([]*domain.Agent, 0)
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, mapPgErr(rows.Err())
}

func (p *PostgresStore) CreateGrant(ctx context.Context, g *domain.AccessGrant) (*domain.AccessGrant, error) {
	row := p.pool.QueryRow(ctx, `
		INSERT INTO access_grants (squad_id, grantee_type, grantee_id, permissions, granted_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, squad_id::text, grantee_type, grantee_id::text, permissions, granted_by::text, created_at
	`, g.SquadID, g.GranteeType, g.GranteeID, g.Permissions, g.GrantedBy)
	return scanGrant(row)
}

func (p *PostgresStore) GetGrant(ctx context.Context, id string) (*domain.AccessGrant, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, squad_id::text, grantee_type, grantee_id::text, permissions, granted_by::text, created_at
		FROM access_grants
		WHERE id = $1
	`, id)
	return scanGrant(row)
}

func (p *PostgresStore) RevokeGrant(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM access_grants WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) ListGrants(ctx context.Context, squadID string) ([]*domain.AccessGrant, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, squad_id::text, grantee_type, grantee_id::text, permissions, granted_by::text, created_at
		FROM access_grants
		WHERE squad_id = $1
		ORDER BY created_at, id
	`, squadID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	grants := make([]*domain.AccessGrant, 0)
	for rows.Next() {
		grant, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, mapPgErr(rows.Err())
}

func (p *PostgresStore) UserMayAccessSquad(ctx context.Context, userID, squadID string, action string) (bool, error) {
	var allowed bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM squads
			WHERE id = $2 AND owner_id = $1
		) OR EXISTS (
			SELECT 1
			FROM access_grants
			WHERE squad_id = $2
			  AND grantee_type = 'user'
			  AND grantee_id = $1
			  AND EXISTS (
			      SELECT 1
			      FROM regexp_split_to_table(permissions, '\s*,\s*') AS perm
			      WHERE lower(perm) IN ('*', 'admin', lower($3))
			         OR (lower($3) = 'ping' AND lower(perm) = 'talk')
			  )
		)
	`, userID, squadID, action).Scan(&allowed)
	return allowed, mapPgErr(err)
}

func (p *PostgresStore) AgentMayMessageSquad(ctx context.Context, agentID, squadID string, action string) (bool, error) {
	var allowed bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM access_grants
			WHERE squad_id = $2
			  AND grantee_type = 'agent'
			  AND grantee_id = $1
			  AND EXISTS (
			      SELECT 1
			      FROM regexp_split_to_table(permissions, '\s*,\s*') AS perm
			      WHERE lower(perm) IN ('*', 'admin', lower($3))
			         OR (lower($3) = 'ping' AND lower(perm) = 'talk')
			  )
		)
	`, agentID, squadID, action).Scan(&allowed)
	return allowed, mapPgErr(err)
}

func (p *PostgresStore) CreateAIProvider(ctx context.Context, provider *domain.AIProvider) (*domain.AIProvider, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	txRow := tx.QueryRow(ctx, `
		INSERT INTO providers (name, kind, base_url, api_key_ref, status, registered_by, api_key_mask)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, name, kind, base_url, api_key_ref, status, registered_by::text, created_at, api_key_mask
	`, provider.Name, provider.Kind, provider.BaseURL, provider.APIKeyRef, defaultResourceStatus(provider.Status), provider.RegisteredBy, provider.APIKeyMask)
	created, err := scanAIProvider(txRow)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetAIProvider(ctx context.Context, id string) (*domain.AIProvider, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, name, kind, base_url, api_key_ref, status, registered_by::text, created_at, api_key_mask
		FROM providers
		WHERE id = $1
	`, id)
	return scanAIProvider(row)
}

func (p *PostgresStore) UpdateAIProvider(ctx context.Context, provider *domain.AIProvider) (*domain.AIProvider, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	txRow := tx.QueryRow(ctx, `
		UPDATE providers
		SET name = $2,
		    kind = $3,
		    base_url = $4,
		    api_key_ref = $5,
		    status = $6,
		    api_key_mask = $7
		WHERE id = $1
		RETURNING id::text, name, kind, base_url, api_key_ref, status, registered_by::text, created_at, api_key_mask
	`, provider.ID, provider.Name, provider.Kind, provider.BaseURL, provider.APIKeyRef, defaultResourceStatus(provider.Status), provider.APIKeyMask)
	created, err := scanAIProvider(txRow)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) DeprecateAIProvider(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE providers
		SET status = $2
		WHERE id = $1
	`, id, domain.ResourceDeprecated)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

// DeleteAIProvider hard-deletes the provider and revokes every grant that
// references it in the same transaction (S-103). It is RESTRICTed while
// any ai_models still reference the provider's credential (ADR-0010 D2).
func (p *PostgresStore) DeleteAIProvider(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var refCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ai_models WHERE provider_id = $1`, id).Scan(&refCount); err != nil {
		return mapPgErr(err)
	}
	if refCount > 0 {
		return fmt.Errorf("%w: provider still has %d registered AI model(s)", ErrConflict, refCount)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM agent_permissions WHERE resource_type = $1 AND resource_id = $2`,
		domain.ResAIProvider, id,
	); err != nil {
		return mapPgErr(err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM providers WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) ListAIProviders(ctx context.Context) ([]*domain.AIProvider, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, name, kind, base_url, api_key_ref, status, registered_by::text, created_at, api_key_mask
		FROM providers
		ORDER BY name
	`)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	providers := make([]*domain.AIProvider, 0)
	for rows.Next() {
		provider, err := scanAIProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, mapPgErr(rows.Err())
}

// aiModelColumns is the shared SELECT list for ai_models rows.
const aiModelColumns = `
	id::text, provider_id::text, display_name, model_name, context_window, supports_tools,
	supports_vision, pricing, long_context_threshold_tokens, status, registered_by, created_at, updated_at`

// CreateAIModel registers a model under an existing provider credential.
// The (provider_id, model_name) pair is unique (UNIQUE violation →
// ErrConflict); an unknown provider → ErrNotFound.
func (p *PostgresStore) CreateAIModel(ctx context.Context, model *domain.AIModel) (*domain.AIModel, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM providers WHERE id = $1`, model.ProviderID).Scan(&one); err != nil {
		return nil, mapPgErr(err)
	}
	txRow := tx.QueryRow(ctx, `
		INSERT INTO ai_models (provider_id, display_name, model_name, context_window, supports_tools,
		                     supports_vision, pricing, long_context_threshold_tokens, status, registered_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+aiModelColumns,
		model.ProviderID, model.DisplayName, model.ModelName, model.ContextWindow, model.SupportsTools,
		model.SupportsVision, defaultJSON(model.Pricing, "{}"), model.LongContextThresholdTokens, defaultResourceStatus(model.Status), model.RegisteredBy)
	created, err := scanAIModel(txRow)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetAIModel(ctx context.Context, id string) (*domain.AIModel, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT `+aiModelColumns+`
		FROM ai_models
		WHERE id = $1
	`, id)
	return scanAIModel(row)
}

// ListAIModels returns models filtered by status; status "" = all.
func (p *PostgresStore) ListAIModels(ctx context.Context, status domain.ResourceStatus) ([]*domain.AIModel, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+aiModelColumns+`
		FROM ai_models
		WHERE $1 = '' OR status = $1
		ORDER BY display_name
	`, string(status))
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	models := make([]*domain.AIModel, 0)
	for rows.Next() {
		model, err := scanAIModel(rows)
		if err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, mapPgErr(rows.Err())
}

func (p *PostgresStore) UpdateAIModel(ctx context.Context, model *domain.AIModel) (*domain.AIModel, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM providers WHERE id = $1`, model.ProviderID).Scan(&one); err != nil {
		return nil, mapPgErr(err)
	}
	txRow := tx.QueryRow(ctx, `
		UPDATE ai_models
		SET provider_id = $2,
		    display_name = $3,
		    model_name = $4,
		    context_window = $5,
		    supports_tools = $6,
		    supports_vision = $7,
		    pricing = $8,
		    long_context_threshold_tokens = $9,
		    status = $10,
		    updated_at = now()
		WHERE id = $1
		RETURNING `+aiModelColumns,
		model.ID, model.ProviderID, model.DisplayName, model.ModelName, model.ContextWindow, model.SupportsTools,
		model.SupportsVision, defaultJSON(model.Pricing, "{}"), model.LongContextThresholdTokens, defaultResourceStatus(model.Status))
	updated, err := scanAIModel(txRow)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, updated.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) DeprecateAIModel(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE ai_models
		SET status = $2, updated_at = now()
		WHERE id = $1
	`, id, domain.ResourceDeprecated)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

// DeleteAIModel hard-deletes the model and cascades its user grants. It is
// RESTRICTed while any agent is bound to the model (primary or fallback),
// mirroring the FK default (NO ACTION) with a detectable ErrConflict.
func (p *PostgresStore) DeleteAIModel(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var bound int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM agents WHERE ai_model_id = $1 OR fallback_ai_model_id = $1
	`, id).Scan(&bound); err != nil {
		return mapPgErr(err)
	}
	if bound > 0 {
		return fmt.Errorf("%w: %d agent(s) still bound to this AI model", ErrConflict, bound)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_model_grants WHERE ai_model_id = $1`, id); err != nil {
		return mapPgErr(err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM ai_models WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

// GrantModelToUser grants a user the right to bind a model. Missing user or
// model → ErrNotFound; duplicate (user, model) pair → ErrConflict.
func (p *PostgresStore) GrantModelToUser(ctx context.Context, g *domain.UserModelGrant) (*domain.UserModelGrant, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM users WHERE id = $1`, g.GranteeUserID).Scan(&one); err != nil {
		return nil, mapPgErr(err)
	}
	if err := tx.QueryRow(ctx, `SELECT 1 FROM ai_models WHERE id = $1`, g.AIModelID).Scan(&one); err != nil {
		return nil, mapPgErr(err)
	}
	txRow := tx.QueryRow(ctx, `
		INSERT INTO user_model_grants (grantee_user_id, ai_model_id, granted_by)
		VALUES ($1, $2, $3)
		RETURNING id::text, grantee_user_id::text, ai_model_id::text, granted_by::text, created_at
	`, g.GranteeUserID, g.AIModelID, g.GrantedBy)
	created, err := scanUserModelGrant(txRow)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) RevokeModelFromUser(ctx context.Context, userID string, aiModelID string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		DELETE FROM user_model_grants WHERE grantee_user_id = $1 AND ai_model_id = $2
	`, userID, aiModelID)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, aiModelID); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) ListUserModelGrants(ctx context.Context, userID string) ([]*domain.UserModelGrant, error) {
	return p.queryUserModelGrants(ctx, `grantee_user_id = $1`, userID)
}

func (p *PostgresStore) ListUsersGrantedModel(ctx context.Context, aiModelID string) ([]*domain.UserModelGrant, error) {
	return p.queryUserModelGrants(ctx, `ai_model_id = $1`, aiModelID)
}

func (p *PostgresStore) queryUserModelGrants(ctx context.Context, where string, arg string) ([]*domain.UserModelGrant, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, grantee_user_id::text, ai_model_id::text, granted_by::text, created_at
		FROM user_model_grants
		WHERE `+where+`
		ORDER BY created_at, id
	`, arg)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	grants := make([]*domain.UserModelGrant, 0)
	for rows.Next() {
		grant, err := scanUserModelGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, mapPgErr(rows.Err())
}

func (p *PostgresStore) CreateResource(ctx context.Context, resource *domain.RegistryResource) (*domain.RegistryResource, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	txRow := tx.QueryRow(ctx, `
		INSERT INTO registry_resources (
			type, name, description, endpoint, auth_ref, manifest, status, registered_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id::text, type, name, description, endpoint, auth_ref, manifest, status, registered_by::text, created_at
	`, resource.Type, resource.Name, resource.Description, resource.Endpoint, resource.AuthRef, defaultJSON(resource.Manifest, "{}"), defaultResourceStatus(resource.Status), resource.RegisteredBy)
	created, err := scanResource(txRow)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetResource(ctx context.Context, typ domain.ResourceType, id string) (*domain.RegistryResource, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, type, name, description, endpoint, auth_ref, manifest, status, registered_by::text, created_at
		FROM registry_resources
		WHERE type = $1 AND id = $2
	`, typ, id)
	return scanResource(row)
}

func (p *PostgresStore) UpdateResource(ctx context.Context, resource *domain.RegistryResource) (*domain.RegistryResource, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	txRow := tx.QueryRow(ctx, `
		UPDATE registry_resources
		SET name = $3,
		    description = $4,
		    endpoint = $5,
		    auth_ref = $6,
		    manifest = $7,
		    status = $8
		WHERE type = $1 AND id = $2
		RETURNING id::text, type, name, description, endpoint, auth_ref, manifest, status, registered_by::text, created_at
	`, resource.Type, resource.ID, resource.Name, resource.Description, resource.Endpoint, resource.AuthRef, defaultJSON(resource.Manifest, "{}"), defaultResourceStatus(resource.Status))
	created, err := scanResource(txRow)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) DeprecateResource(ctx context.Context, typ domain.ResourceType, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE registry_resources
		SET status = $3
		WHERE type = $1 AND id = $2
	`, typ, id, domain.ResourceDeprecated)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

// DeleteResource hard-deletes a registry resource and revokes every agent
// grant that references it in the same transaction (S-103).
func (p *PostgresStore) DeleteResource(ctx context.Context, typ domain.ResourceType, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM agent_permissions WHERE resource_type = $1 AND resource_id = $2`,
		typ, id,
	); err != nil {
		return mapPgErr(err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM registry_resources WHERE type = $1 AND id = $2`, typ, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) ListResources(ctx context.Context, typ domain.ResourceType) ([]*domain.RegistryResource, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, type, name, description, endpoint, auth_ref, manifest, status, registered_by::text, created_at
		FROM registry_resources
		WHERE type = $1
		ORDER BY name
	`, typ)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	resources := make([]*domain.RegistryResource, 0)
	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, mapPgErr(rows.Err())
}

func (p *PostgresStore) GrantAgentPermission(ctx context.Context, perm *domain.AgentPermission) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO agent_permissions (agent_id, resource_type, resource_id, granted_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (agent_id, resource_type, resource_id) DO NOTHING
	`, perm.AgentID, perm.ResourceType, perm.ResourceID, perm.GrantedBy)
	return mapPgErr(err)
}

func (p *PostgresStore) RevokeAgentPermission(ctx context.Context, agentID string, typ domain.ResourceType, resourceID string) error {
	_, err := p.pool.Exec(ctx, `
		DELETE FROM agent_permissions
		WHERE agent_id = $1 AND resource_type = $2 AND resource_id = $3
	`, agentID, typ, resourceID)
	return mapPgErr(err)
}

func (p *PostgresStore) ListAgentPermissions(ctx context.Context, agentID string) ([]*domain.AgentPermission, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, agent_id::text, resource_type, resource_id::text, granted_by::text, created_at
		FROM agent_permissions
		WHERE agent_id = $1
		ORDER BY resource_type, resource_id
	`, agentID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	perms := make([]*domain.AgentPermission, 0)
	for rows.Next() {
		perm, err := scanAgentPermission(rows)
		if err != nil {
			return nil, err
		}
		perms = append(perms, perm)
	}
	return perms, mapPgErr(rows.Err())
}

// ListPermissionsByResource returns every agent grant pointing at one
// resource — the usage check behind delete warnings (S-103).
func (p *PostgresStore) ListPermissionsByResource(ctx context.Context, typ domain.ResourceType, resourceID string) ([]*domain.AgentPermission, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, agent_id::text, resource_type, resource_id::text, granted_by::text, created_at
		FROM agent_permissions
		WHERE resource_type = $1 AND resource_id = $2
		ORDER BY agent_id
	`, typ, resourceID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	perms := make([]*domain.AgentPermission, 0)
	for rows.Next() {
		perm, err := scanAgentPermission(rows)
		if err != nil {
			return nil, err
		}
		perms = append(perms, perm)
	}
	return perms, mapPgErr(rows.Err())
}

func (p *PostgresStore) SetAgentPermissions(ctx context.Context, agentID string, perms []domain.AgentPermission) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	agent, err := getAgentTx(ctx, tx, agentID)
	if err != nil {
		return mapPgErr(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_permissions WHERE agent_id = $1`, agentID); err != nil {
		return mapPgErr(err)
	}
	for _, perm := range perms {
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_permissions (agent_id, resource_type, resource_id, granted_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (agent_id, resource_type, resource_id) DO NOTHING
		`, agentID, perm.ResourceType, perm.ResourceID, perm.GrantedBy); err != nil {
			return mapPgErr(err)
		}
	}
	// Grant changes (especially project_workspace) must re-sync the Agent CR so
	// workspaceSecrets reflect the live grant set (ADR-0009).
	if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpUpsertAgent, agent); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) AgentHasPermission(ctx context.Context, agentID string, typ domain.ResourceType, resourceID string) (bool, error) {
	var allowed bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM agent_permissions
			WHERE agent_id = $1 AND resource_type = $2 AND resource_id = $3
		)
	`, agentID, typ, resourceID).Scan(&allowed)
	return allowed, mapPgErr(err)
}

func (p *PostgresStore) RecordMetering(ctx context.Context, event *domain.MeteringEvent) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO metering (
			agent_id, squad_id, task_id, provider_id, model, input_tokens, output_tokens,
			cost, currency, timestamp, model_used,
			rate_input_per_1m, rate_cached_input_per_1m, rate_cache_write_per_1m,
			rate_output_per_1m, rate_snapshot
		)
		VALUES ($1, $2, nullif($3, '')::uuid, nullif($4, '')::uuid, $5, $6, $7, $8, $9, coalesce(nullif($10, ''), now()::text)::timestamptz, $11, $12, $13, $14, $15, $16)
	`, event.AgentID, event.SquadID, event.TaskID, event.ProviderID, event.Model, event.InputTokens, event.OutputTokens, event.Cost, defaultCurrency(event.Currency), nullableTimeText(event.Timestamp), event.ModelUsed,
		rateValue(event.RateInputPer1M), rateValue(event.RateCachedInputPer1M), rateValue(event.RateCacheWritePer1M), rateValue(event.RateOutputPer1M), event.RateSnapshot)
	return mapPgErr(err)
}

// rateValue maps a nil rate pointer to a SQL NULL (no snapshot) without
// importing database/sql into the call sites.
func rateValue(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// SumMetering aggregates metering events. A non-zero `since` restricts
// the sum to events at/after that instant (S-169: month-to-date spend);
// the zero value aggregates over all time.
func (p *PostgresStore) SumMetering(ctx context.Context, squadID, agentID string, since time.Time) (*domain.MeteringEvent, error) {
	var sinceArg any
	if !since.IsZero() {
		sinceArg = since.UTC()
	}
	row := p.pool.QueryRow(ctx, `
		SELECT coalesce(sum(input_tokens), 0)::integer,
		       coalesce(sum(output_tokens), 0)::integer,
		       coalesce(sum(cost), 0)::double precision,
		       coalesce(max(currency), 'USD'),
		       max(timestamp)
		FROM metering
		WHERE ($1 = '' OR squad_id = nullif($1, '')::uuid)
		  AND ($2 = '' OR agent_id = nullif($2, '')::uuid)
		  AND ($3::timestamptz IS NULL OR timestamp >= $3::timestamptz)
	`, squadID, agentID, sinceArg)

	var out domain.MeteringEvent
	var timestamp sql.NullTime
	if err := row.Scan(&out.InputTokens, &out.OutputTokens, &out.Cost, &out.Currency, &timestamp); err != nil {
		return nil, mapPgErr(err)
	}
	out.SquadID = squadID
	out.AgentID = agentID
	if timestamp.Valid {
		out.Timestamp = timestamp.Time
	}
	return &out, nil
}

// SumMeteringDaily buckets metering per UTC day per
// (squad, agent, provider, model) for the S-190 dashboard histograms.
// squadIDs is an explicit allowlist; empty means all squads (see
// MeteringStore). Names come from LEFT JOINs so deleted rows degrade to
// empty labels rather than dropping history.
func (p *PostgresStore) SumMeteringDaily(ctx context.Context, since time.Time, squadIDs []string) ([]domain.MeteringDailyRow, error) {
	var sinceArg any
	if !since.IsZero() {
		sinceArg = since.UTC()
	}
	// Text array literal cast to uuid[]; empty list → '{}' → no filter.
	squadArg := "{" + strings.Join(squadIDs, ",") + "}"

	rows, err := p.pool.Query(ctx, `
		SELECT to_char(m.timestamp AT TIME ZONE 'UTC', 'YYYY-MM-DD'),
		       m.squad_id::text, coalesce(s.name, ''),
		       m.agent_id::text, coalesce(a.name, ''),
		       coalesce(m.provider_id::text, ''), coalesce(p.name, ''),
		       m.model,
		       sum(m.input_tokens)::integer,
		       sum(m.output_tokens)::integer,
		       coalesce(sum(m.cost), 0)::double precision,
		       coalesce(max(m.currency), 'USD')
		FROM metering m
		LEFT JOIN squads s ON s.id = m.squad_id
		LEFT JOIN agents a ON a.id = m.agent_id
		LEFT JOIN providers p ON p.id = m.provider_id
		WHERE ($1::timestamptz IS NULL OR m.timestamp >= $1::timestamptz)
		  AND (cardinality($2::uuid[]) = 0 OR m.squad_id = ANY($2::uuid[]))
		GROUP BY 1, 2, 3, 4, 5, 6, 7, 8
		ORDER BY 1, 3, 5, 7, 8
	`, sinceArg, squadArg)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	out := []domain.MeteringDailyRow{}
	for rows.Next() {
		var row domain.MeteringDailyRow
		if err := rows.Scan(&row.Day, &row.SquadID, &row.SquadName, &row.AgentID, &row.AgentName,
			&row.ProviderID, &row.ProviderName, &row.Model,
			&row.InputTokens, &row.OutputTokens, &row.Cost, &row.Currency); err != nil {
			return nil, mapPgErr(err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (p *PostgresStore) RecordAudit(ctx context.Context, entry *domain.AuditEntry) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO audit_log (
			actor_type, actor_id, action, resource_type, resource_id, squad_id,
			metadata, timestamp
		)
		VALUES (
			$1, $2, $3, $4, nullif($5, '')::uuid, nullif($6, '')::uuid,
			$7, coalesce(nullif($8, ''), now()::text)::timestamptz
		)
	`, entry.ActorType, entry.ActorID, entry.Action, entry.ResourceType, entry.ResourceID, entry.SquadID, defaultJSON(entry.Metadata, "{}"), nullableTimeText(entry.Timestamp))
	return mapPgErr(err)
}

// writePendingAuditsTx drains the context's pending audit entries and
// writes them inside the caller's transaction (S-86). A failure here rolls
// the whole mutation back, so a committed state change always has its
// audit record.
func (p *PostgresStore) writePendingAuditsTx(ctx context.Context, tx pgx.Tx, resourceID string) error {
	for _, entry := range DrainPendingAudits(ctx) {
		if entry.ResourceID == "" && resourceID != "" {
			entry.ResourceID = resourceID
		}
		// A squad mutation audits the squad itself; the store knows the new
		// squad id even when the handler could not.
		if entry.ResourceType == "squad" && entry.SquadID == "" && resourceID != "" {
			entry.SquadID = resourceID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_log (
				actor_type, actor_id, action, resource_type, resource_id, squad_id,
				metadata, timestamp
			)
			VALUES (
				$1, $2, $3, $4, nullif($5, '')::uuid, nullif($6, '')::uuid,
				$7, coalesce(nullif($8, ''), now()::text)::timestamptz
			)
		`, entry.ActorType, entry.ActorID, entry.Action, entry.ResourceType, entry.ResourceID, entry.SquadID, defaultJSON(entry.Metadata, "{}"), nullableTimeText(entry.Timestamp)); err != nil {
			return mapPgErr(err)
		}
	}
	return nil
}

func (p *PostgresStore) ListAudit(ctx context.Context, squadID string, limit int) ([]*domain.AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, actor_type, actor_id::text, action, resource_type,
		       coalesce(resource_id::text, ''), coalesce(squad_id::text, ''),
		       metadata, timestamp
		FROM audit_log
		WHERE ($1 = '' OR squad_id = nullif($1, '')::uuid)
		ORDER BY timestamp DESC, id
		LIMIT $2
	`, squadID, limit)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	entries := make([]*domain.AuditEntry, 0)
	for rows.Next() {
		entry, err := scanAuditEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, mapPgErr(rows.Err())
}

// DeleteAuditByActionBefore (S-198) purges audit rows with the given
// action recorded before cutoff. Action-scoped so the retention sweep
// never touches unrelated audit history (the log stays append-only for
// everything else).
func (p *PostgresStore) DeleteAuditByActionBefore(ctx context.Context, action string, cutoff time.Time) (int, error) {
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM audit_log WHERE action = $1 AND timestamp < $2
	`, action, cutoff.UTC())
	if err != nil {
		return 0, mapPgErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (p *PostgresStore) GetBoard(ctx context.Context, squadID string) (*domain.Board, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, squad_id::text, created_at
		FROM kanban_boards
		WHERE squad_id = $1
	`, squadID)
	return scanBoard(row)
}

func (p *PostgresStore) CreateTask(ctx context.Context, t *domain.Task) (*domain.Task, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		INSERT INTO tasks (
			board_id, squad_id, title, description, status, assignee_agent_id,
			created_by_type, created_by_id, position, origin_message_id, task_number
		)
		VALUES (
			$1, $2, $3, $4, $5, nullif($6, '')::uuid,
			$7, $8,
			coalesce((SELECT max(position) + 1 FROM tasks WHERE board_id = $1 AND status = $5), 1),
			$9,
			(SELECT coalesce(max(task_number), 0) + 1 FROM tasks WHERE squad_id = $2)
		)
		RETURNING id::text, board_id::text, squad_id::text, title, description, status,
		          coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		          position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
	`, t.BoardID, t.SquadID, t.Title, t.Description, defaultTaskStatus(t.Status), t.AssigneeAgentID, t.CreatedByType, t.CreatedByID, t.OriginMessageID)
	created, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, board_id::text, squad_id::text, title, description, status,
		       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		       position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
		FROM tasks
		WHERE id = $1
	`, id)
	return scanTask(row)
}

func (p *PostgresStore) UpdateTask(ctx context.Context, t *domain.Task) (*domain.Task, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		WITH existing AS (
			SELECT id, board_id, status
			FROM tasks
			WHERE id = $1
		),
		next_position AS (
			SELECT coalesce(max(position) + 1, 1) AS position
			FROM tasks
			WHERE board_id = (SELECT board_id FROM existing)
			  AND status = $5
			  AND id <> $1
		)
		UPDATE tasks
		SET title = $2,
		    description = $3,
		    assignee_agent_id = nullif($4, '')::uuid,
		    status = $5,
		    position = CASE
		    	WHEN tasks.status = $5 THEN tasks.position
		    	ELSE (SELECT position FROM next_position)
		    END,
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, board_id::text, squad_id::text, title, description, status,
		          coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		          position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
	`, t.ID, t.Title, t.Description, t.AssigneeAgentID, defaultTaskStatus(t.Status))
	updated, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, updated.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) SetTaskWorkspace(ctx context.Context, taskID string, resourceID string, branch string, commitSHA string) (*domain.Task, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE tasks
		SET workspace_resource_id = $2,
		    workspace_branch = $3,
		    workspace_commit_sha = $4,
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, board_id::text, squad_id::text, title, description, status,
		          coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		          position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
	`, taskID, resourceID, branch, commitSHA)
	updated, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, taskID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) DeleteTask(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `DELETE FROM tasks WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return err
	}
	return mapPgErr(tx.Commit(ctx))
}
func (p *PostgresStore) ListTasks(ctx context.Context, boardID string, status domain.TaskStatus) ([]*domain.Task, error) {
	var rows pgx.Rows
	var err error
	if status == "" {
		rows, err = p.pool.Query(ctx, `
			SELECT id::text, board_id::text, squad_id::text, title, description, status,
			       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
			       position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
			FROM tasks
			WHERE board_id = $1
			ORDER BY status, position
		`, boardID)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT id::text, board_id::text, squad_id::text, title, description, status,
			       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
			       position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
			FROM tasks
			WHERE board_id = $1 AND status = $2
			ORDER BY position
		`, boardID, status)
	}
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	tasks := []*domain.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, mapPgErr(rows.Err())
}

func (p *PostgresStore) ListAgentTasks(ctx context.Context, agentID string) ([]*domain.Task, error) {
	// Visibility rule (S-213): only backlog tasks are hidden from the
	// agent-facing listing; an agent may still see its own done/review
	// work. This is NOT the pickup rule — claim/start enforce the
	// AgentPickupStatuses allowlist server-side (S-228).
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, board_id::text, squad_id::text, title, description, status,
		       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		       position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
		FROM tasks
		WHERE assignee_agent_id = $1
		  AND status <> $2 -- S-213: backlog tasks are not agent-facing until a human moves them out
		ORDER BY status, position
	`, agentID, domain.TaskBacklog)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	tasks := make([]*domain.Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, mapPgErr(rows.Err())
}

func (p *PostgresStore) ClaimNextTask(ctx context.Context, agentID string, workerID string, leaseFor time.Duration) (*domain.Task, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)
	if leaseFor <= 0 {
		leaseFor = 5 * time.Minute
	}
	if workerID == "" {
		workerID = agentID
	}
	active, err := p.agentHasActiveTaskExecution(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, ErrNotFound
	}

	task, err := p.claimReclaimableInProgress(ctx, tx, agentID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if errors.Is(err, ErrNotFound) {
		task, err = p.claimTodoTask(ctx, tx, agentID)
		if err != nil {
			return nil, err
		}
	}
	exec, err := p.createTaskExecutionTx(ctx, tx, task.ID, agentID, workerID, leaseFor)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, task.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return attachTaskExecution(task, exec), nil
}

func (p *PostgresStore) agentHasActiveTaskExecution(ctx context.Context, tx pgx.Tx, agentID string) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM task_executions
			WHERE agent_id = $1
			  AND status = $2
			  AND lease_expires_at > now()
		)
	`, agentID, domain.TaskExecutionActive).Scan(&active)
	return active, mapPgErr(err)
}

func (p *PostgresStore) claimReclaimableInProgress(ctx context.Context, tx pgx.Tx, agentID string) (*domain.Task, error) {
	row := tx.QueryRow(ctx, `
		SELECT id::text, board_id::text, squad_id::text, title, description, status,
		       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		       position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
		FROM tasks
		WHERE assignee_agent_id = $1
		  AND status = $2
		  AND NOT EXISTS (
		    SELECT 1
		    FROM task_executions
		    WHERE task_id = tasks.id
		      AND status = $3
		      AND lease_expires_at > now()
		  )
		ORDER BY updated_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`, agentID, domain.TaskInProgress, domain.TaskExecutionActive)
	return scanTask(row)
}

func (p *PostgresStore) claimTodoTask(ctx context.Context, tx pgx.Tx, agentID string) (*domain.Task, error) {
	row := tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id, board_id
			FROM tasks
			WHERE assignee_agent_id = $1 AND status = $2
			ORDER BY position
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		),
		next_position AS (
			SELECT coalesce(max(t.position) + 1, 1) AS position
			FROM tasks t
			JOIN candidate c ON c.board_id = t.board_id
			WHERE t.status = $3
		)
		UPDATE tasks
		SET status = $3,
		    position = (SELECT position FROM next_position),
		    updated_at = now()
		WHERE id = (SELECT id FROM candidate)
		RETURNING id::text, board_id::text, squad_id::text, title, description, status,
		          coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		          position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
	`, agentID, domain.TaskTodo, domain.TaskInProgress)
	return scanTask(row)
}

func (p *PostgresStore) createTaskExecutionTx(ctx context.Context, tx pgx.Tx, taskID string, agentID string, workerID string, leaseFor time.Duration) (*domain.TaskExecution, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO task_executions (
			task_id, agent_id, worker_id, lease_expires_at
		)
		VALUES ($1, $2, $3, now() + ($4::text)::interval)
		RETURNING id::text, task_id::text, agent_id::text, worker_id, fencing_token,
		          status, lease_expires_at, coalesce(result_status, ''), result_summary,
		          prompt_sha, started_at, completed_at, updated_at
	`, taskID, agentID, workerID, fmt.Sprintf(secondsFormat, int(leaseFor/time.Second)))
	return scanTaskExecution(row)
}

func (p *PostgresStore) ListBoardTaskExecutions(ctx context.Context, boardID string) ([]*domain.TaskExecution, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT e.id::text, e.task_id::text, e.agent_id::text, e.worker_id, e.fencing_token,
		       e.status, e.lease_expires_at, coalesce(e.result_status, ''), e.result_summary,
		       e.prompt_sha, e.started_at, e.completed_at, e.updated_at
		FROM task_executions e
		JOIN tasks t ON t.id = e.task_id
		WHERE t.board_id = $1 AND e.status = $2
		ORDER BY e.started_at
	`, boardID, domain.TaskExecutionActive)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	executions := []*domain.TaskExecution{}
	for rows.Next() {
		exec, err := scanTaskExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, exec)
	}
	return executions, mapPgErr(rows.Err())
}

func (p *PostgresStore) HeartbeatTaskExecution(ctx context.Context, agentID string, executionID string, fencingToken string, leaseFor time.Duration) (*domain.TaskExecution, error) {
	if leaseFor <= 0 {
		leaseFor = 5 * time.Minute
	}
	row := p.pool.QueryRow(ctx, `
		UPDATE task_executions
		SET lease_expires_at = now() + ($4::text)::interval,
		    updated_at = now()
		WHERE id = $1
		  AND agent_id = $2
		  AND fencing_token = $3
		  AND status = 'active'
		RETURNING id::text, task_id::text, agent_id::text, worker_id, fencing_token,
		          status, lease_expires_at, coalesce(result_status, ''), result_summary,
		          prompt_sha, started_at, completed_at, updated_at
	`, executionID, agentID, fencingToken, fmt.Sprintf(secondsFormat, int(leaseFor/time.Second)))
	exec, err := scanTaskExecution(row)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrConflict
	}
	return exec, err
}

// ReapExpiredTaskExecutions expires dead attempts and re-queues their tasks.
// Two conditional statements in one transaction: a heartbeat or complete that
// lands after the cutoff wins (its own WHERE clause matches first), so the
// reaper is idempotent and safe under concurrent replicas.
func (p *PostgresStore) ReapExpiredTaskExecutions(ctx context.Context, cutoff time.Time) ([]domain.ReapedExecution, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		UPDATE task_executions
		SET status         = $2,
		    completed_at   = now(),
		    result_summary = 'lease expired without completion',
		    updated_at     = now()
		WHERE status = $1
		  AND lease_expires_at < $3
		RETURNING id::text, task_id::text, agent_id::text
	`, domain.TaskExecutionActive, domain.TaskExecutionExpired, cutoff)
	if err != nil {
		return nil, mapPgErr(err)
	}
	reaped := []domain.ReapedExecution{}
	taskIDs := []string{}
	for rows.Next() {
		var r domain.ReapedExecution
		if err := rows.Scan(&r.ExecutionID, &r.TaskID, &r.AgentID); err != nil {
			rows.Close()
			return nil, mapPgErr(err)
		}
		reaped = append(reaped, r)
		taskIDs = append(taskIDs, r.TaskID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapPgErr(err)
	}
	if len(taskIDs) == 0 {
		return reaped, mapPgErr(tx.Commit(ctx))
	}

	// Re-queue only tasks with no remaining live attempt: a task can
	// transiently hold two active executions (a lapsed one plus a fresh one
	// from a lazy reclaim), and the fresh lease must keep it in-progress.
	_, err = tx.Exec(ctx, `
		UPDATE tasks
		SET status     = $2,
		    updated_at = now()
		WHERE id = ANY($1)
		  AND status = $3
		  AND NOT EXISTS (
		    SELECT 1
		    FROM task_executions e
		    WHERE e.task_id = tasks.id
		      AND e.status = $4
		      AND e.lease_expires_at > now()
		  )
	`, taskIDs, domain.TaskTodo, domain.TaskInProgress, domain.TaskExecutionActive)
	if err != nil {
		return nil, mapPgErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return reaped, nil
}

func (p *PostgresStore) CompleteTaskExecution(ctx context.Context, agentID string, taskID string, executionID string, fencingToken string, status domain.TaskStatus, summary string) (*domain.Task, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var execStatus domain.TaskExecutionStatus
	if status == domain.TaskBlocked {
		execStatus = domain.TaskExecutionBlocked
	} else {
		execStatus = domain.TaskExecutionCompleted
	}
	tag, err := tx.Exec(ctx, `
		UPDATE task_executions
		SET status = $6,
		    result_status = $7,
		    result_summary = $8,
		    completed_at = now(),
		    updated_at = now()
		WHERE id = $1
		  AND task_id = $2
		  AND agent_id = $3
		  AND fencing_token = $4
		  AND status = $5
	`, executionID, taskID, agentID, fencingToken, domain.TaskExecutionActive, execStatus, status, summary)
	if err != nil {
		return nil, mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrConflict
	}

	row := tx.QueryRow(ctx, `
		WITH existing AS (
			SELECT id, board_id, status
			FROM tasks
			WHERE id = $1 AND assignee_agent_id = $2
			FOR UPDATE
		),
		next_position AS (
			SELECT coalesce(max(position) + 1, 1) AS position
			FROM tasks
			WHERE board_id = (SELECT board_id FROM existing)
			  AND status = $3
			  AND id <> $1
		)
		UPDATE tasks
		SET status = $3,
		    position = CASE
		        WHEN tasks.status = $3 THEN tasks.position
		        ELSE (SELECT position FROM next_position)
		    END,
		    result = CASE
		        WHEN $3 IN ('done', 'blocked') THEN $4 ELSE result
		    END,
		    result_status = CASE
		        WHEN $3 IN ('done', 'blocked') THEN $3 ELSE result_status
		    END,
		    result_at = CASE
		        WHEN $3 IN ('done', 'blocked') THEN now() ELSE result_at
		    END,
		    updated_at = now()
		WHERE id = (SELECT id FROM existing)
		RETURNING id::text, board_id::text, squad_id::text, title, description, status,
		          coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		          position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
	`, taskID, agentID, status, summary)
	task, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, task.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return task, nil
}

func (p *PostgresStore) CreateAgentMemory(ctx context.Context, memory *domain.AgentMemory) (*domain.AgentMemory, error) {
	applyMemoryDefaults(memory)
	row := p.pool.QueryRow(ctx, `
		INSERT INTO agent_memory (
			agent_id, squad_id, content, raw_content, trust_level, provenance,
			review_status, embedding, embedding_model, source_task_id, metadata
		)
		VALUES (
			$1, nullif($2, '')::uuid, $3, $4, $5, $6, $7,
			nullif($8, '')::vector, $9, nullif($10, '')::uuid, $11
		)
		RETURNING id::text, agent_id::text, coalesce(squad_id::text, ''), content,
		          raw_content, trust_level, provenance, review_status,
		          coalesce(embedding::text, ''), embedding_model,
		          coalesce(source_task_id::text, ''), metadata, created_at
	`, memory.AgentID, memory.SquadID, memory.Content, memory.RawContent, memory.TrustLevel, memory.Provenance, memory.ReviewStatus, vectorLiteral(memory.Embedding), memory.EmbeddingModel, memory.SourceTaskID, defaultJSON(memory.Metadata, "{}"))
	return scanAgentMemory(row)
}

func (p *PostgresStore) ListAgentMemory(ctx context.Context, agentID string, squadID string, queryEmbedding []float64, limit int) ([]*domain.AgentMemory, error) {
	if limit <= 0 {
		limit = 10
	}
	queryVector := vectorLiteral(queryEmbedding)
	orderBy := "created_at DESC, id"
	if queryVector != "" {
		orderBy = "CASE WHEN embedding IS NULL THEN 1 ELSE 0 END, embedding <=> $4::vector, created_at DESC, id"
	}
	args := []any{agentID, squadID, limit}
	if queryVector != "" {
		args = append(args, queryVector)
	}
	rows, err := p.pool.Query(ctx, fmt.Sprintf(`
		SELECT id::text, agent_id::text, coalesce(squad_id::text, ''), content,
		       raw_content, trust_level, provenance, review_status,
		       coalesce(embedding::text, ''), embedding_model,
		       coalesce(source_task_id::text, ''), metadata, created_at
		FROM agent_memory
		WHERE agent_id = $1
		  AND (squad_id IS NULL OR squad_id = nullif($2, '')::uuid)
		ORDER BY %s
		LIMIT $3
	`, orderBy), args...)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	memories := make([]*domain.AgentMemory, 0)
	for rows.Next() {
		memory, err := scanAgentMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, mapPgErr(rows.Err())
}

// SearchAgentMemory (S-212) ranks the agent's own memories by cosine
// similarity. Trust-gating policy: review_status='rejected' rows are
// never recalled; pending_review and approved rows are recallable across
// every trust level (raw_model_output, distilled, verified) — pending
// review means "not yet curated", not "untrustworthy", and withholding
// it would starve recall. Rows without an embedding cannot participate
// in cosine ranking and are excluded. Score = 1 - cosine distance.
func (p *PostgresStore) SearchAgentMemory(ctx context.Context, agentID string, squadID string, queryEmbedding []float64, limit int) ([]MemorySearchHit, error) {
	if limit <= 0 {
		limit = 5
	}
	queryVector := vectorLiteral(queryEmbedding)
	if queryVector == "" {
		return nil, ErrInvalidInput
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, agent_id::text, coalesce(squad_id::text, ''), content,
		       raw_content, trust_level, provenance, review_status,
		       coalesce(embedding::text, ''), embedding_model,
		       coalesce(source_task_id::text, ''), metadata, created_at,
		       1 - (embedding <=> $4::vector) AS score
		FROM agent_memory
		WHERE agent_id = $1
		  AND (squad_id IS NULL OR squad_id = nullif($2, '')::uuid)
		  AND review_status <> 'rejected'
		  AND embedding IS NOT NULL
		ORDER BY embedding <=> $4::vector
		LIMIT $3
	`, agentID, squadID, limit, queryVector)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	hits := make([]MemorySearchHit, 0)
	for rows.Next() {
		var memory domain.AgentMemory
		var embeddingText string
		var score float64
		if err := rows.Scan(
			&memory.ID,
			&memory.AgentID,
			&memory.SquadID,
			&memory.Content,
			&memory.RawContent,
			&memory.TrustLevel,
			&memory.Provenance,
			&memory.ReviewStatus,
			&embeddingText,
			&memory.EmbeddingModel,
			&memory.SourceTaskID,
			&memory.Metadata,
			&memory.CreatedAt,
			&score,
		); err != nil {
			return nil, mapPgErr(err)
		}
		memory.Embedding = parseVectorText(embeddingText)
		hits = append(hits, MemorySearchHit{Memory: &memory, Score: score})
	}
	return hits, mapPgErr(rows.Err())
}

// ListMemoriesNeedingEmbedding (S-212 backfill) returns rows whose
// embedding_model differs from the current model — including rows
// without any embedding (embedding_model = ”). Ordered by created_at
// so the oldest memories are embedded first.
func (p *PostgresStore) ListMemoriesNeedingEmbedding(ctx context.Context, currentModel string, limit int) ([]*domain.AgentMemory, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, agent_id::text, coalesce(squad_id::text, ''), content,
		       raw_content, trust_level, provenance, review_status,
		       coalesce(embedding::text, ''), embedding_model,
		       coalesce(source_task_id::text, ''), metadata, created_at
		FROM agent_memory
		WHERE embedding_model IS DISTINCT FROM $1
		ORDER BY created_at, id
		LIMIT $2
	`, currentModel, limit)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	memories := make([]*domain.AgentMemory, 0)
	for rows.Next() {
		memory, err := scanAgentMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, mapPgErr(rows.Err())
}

// SetAgentMemoryEmbedding (S-212 backfill) writes a freshly generated
// vector plus the model that produced it.
func (p *PostgresStore) SetAgentMemoryEmbedding(ctx context.Context, id string, embedding []float64, model string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE agent_memory
		SET embedding = nullif($2, '')::vector,
		    embedding_model = $3
		WHERE id = $1::uuid
	`, id, vectorLiteral(embedding), model)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) LeaseKubernetesOutbox(ctx context.Context, limit int, leaseFor time.Duration) ([]*domain.KubernetesOutboxEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := p.pool.Query(ctx, `
		WITH leased AS (
			SELECT id
			FROM kubernetes_outbox
			WHERE status IN ('pending','failed')
			  AND next_attempt_at <= now()
			  AND (locked_until IS NULL OR locked_until <= now())
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE kubernetes_outbox
		SET locked_until = now() + $2::interval,
		    updated_at = now()
		WHERE id IN (SELECT id FROM leased)
		RETURNING id::text, aggregate_type, aggregate_id::text, operation, payload, status,
		          attempts, last_error, next_attempt_at, locked_until, created_at, updated_at
	`, limit, fmt.Sprintf(secondsFormat, int(leaseFor/time.Second)))
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	return scanKubernetesOutboxRows(rows)
}

func (p *PostgresStore) MarkKubernetesOutboxApplied(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE kubernetes_outbox
		SET status = 'applied',
		    locked_until = NULL,
		    updated_at = now()
		WHERE id = $1
	`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) MarkKubernetesOutboxFailed(ctx context.Context, id string, lastError string, retryAfter time.Duration) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE kubernetes_outbox
		SET status = 'failed',
		    attempts = attempts + 1,
		    last_error = $2,
		    next_attempt_at = now() + $3::interval,
		    locked_until = NULL,
		    updated_at = now()
		WHERE id = $1
	`, id, lastError, fmt.Sprintf(secondsFormat, int(retryAfter/time.Second)))
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) LatestAppliedAgentUpsert(ctx context.Context, agentID string) (*domain.KubernetesOutboxEvent, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, aggregate_type, aggregate_id::text, operation, payload, status,
		       attempts, last_error, next_attempt_at, locked_until, created_at, updated_at
		FROM kubernetes_outbox
		WHERE aggregate_type = 'agent' AND aggregate_id::text = $1
		  AND operation = 'upsert_agent' AND status = 'applied'
		ORDER BY updated_at DESC
		LIMIT 1
	`, agentID)
	event, err := scanKubernetesOutbox(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return event, err
}

func (p *PostgresStore) RecordWakeLatency(ctx context.Context, e *domain.WakeLatencyEvent) (bool, error) {
	var id string
	err := p.pool.QueryRow(ctx, `
		INSERT INTO wake_latency (
			agent_id, squad_id, task_id, wake_requested_at, cr_applied_at,
			container_started_at, claimed_at, queue_ms, scaleup_ms, claim_delay_ms,
			e2e_ms, cold_start
		)
		VALUES ($1::uuid, $2::uuid, nullif($3, '')::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (agent_id, container_started_at) DO NOTHING
		RETURNING id::text
	`, e.AgentID, e.SquadID, e.TaskID, e.WakeRequestedAt, e.CRAppliedAt,
		e.ContainerStartedAt, e.ClaimedAt, e.QueueMs, e.ScaleupMs, e.ClaimDelayMs,
		e.E2EMs, e.ColdStart).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // duplicate wake for this container start
	}
	if err != nil {
		return false, mapPgErr(err)
	}
	return true, nil
}

func (p *PostgresStore) ListWakeLatency(ctx context.Context, squadID string, since time.Time, limit int) ([]*domain.WakeLatencyEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, agent_id::text, squad_id::text, coalesce(task_id::text, ''),
		       wake_requested_at, cr_applied_at, container_started_at, claimed_at,
		       queue_ms, scaleup_ms, claim_delay_ms, e2e_ms, cold_start
		FROM wake_latency
		WHERE ($1 = '' OR squad_id::text = $1) AND claimed_at > $2
		ORDER BY claimed_at DESC
		LIMIT $3
	`, squadID, since, limit)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	out := []*domain.WakeLatencyEvent{}
	for rows.Next() {
		e := &domain.WakeLatencyEvent{}
		if err := rows.Scan(&e.ID, &e.AgentID, &e.SquadID, &e.TaskID, &e.WakeRequestedAt,
			&e.CRAppliedAt, &e.ContainerStartedAt, &e.ClaimedAt, &e.QueueMs, &e.ScaleupMs,
			&e.ClaimDelayMs, &e.E2EMs, &e.ColdStart); err != nil {
			return nil, mapPgErr(err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *PostgresStore) ListKubernetesOutbox(ctx context.Context, status domain.KubernetesOutboxStatus, limit int) ([]*domain.KubernetesOutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, aggregate_type, aggregate_id::text, operation, payload, status,
		       attempts, last_error, next_attempt_at, locked_until, created_at, updated_at
		FROM kubernetes_outbox
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at
		LIMIT $2
	`, status, limit)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	return scanKubernetesOutboxRows(rows)
}

func (p *PostgresStore) CreateMessage(ctx context.Context, m *domain.Message) (*domain.Message, error) {
	maxAttempts := m.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMessageMaxAttempts
	}
	expiresAt := m.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(defaultMessageTTL)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		INSERT INTO messages (
			from_type, from_id, to_agent_id, squad_id, type, payload, status, correlation_id, max_attempts, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, '')::uuid, $9, $10)
		RETURNING id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		          type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		          next_retry_at, expires_at, timeout_at, terminal_reason, created_at, delivered_at
	`, m.FromType, m.FromID, m.ToAgentID, m.SquadID, defaultMessageType(m.Type), defaultJSON(m.Payload, "{}"), defaultMessageStatus(m.Status), m.CorrelationID, maxAttempts, expiresAt)
	created, err := scanMessage(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) ListPendingMessages(ctx context.Context, agentID string) ([]*domain.Message, error) {
	if err := p.expireMessagesForAgent(ctx, agentID); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		       type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		       next_retry_at, expires_at, timeout_at, terminal_reason, created_at, delivered_at
		FROM messages
		WHERE to_agent_id = $1 AND status = $2 AND next_retry_at <= now()
		ORDER BY created_at, id
	`, agentID, domain.MessagePending)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (p *PostgresStore) HasPendingMessages(ctx context.Context, agentID string) (bool, error) {
	if err := p.expireMessagesForAgent(ctx, agentID); err != nil {
		return false, err
	}
	var exists bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages WHERE to_agent_id = $1 AND status = $2
		)
	`, agentID, domain.MessagePending).Scan(&exists)
	return exists, mapPgErr(err)
}

func (p *PostgresStore) ListAgentMessageHistory(ctx context.Context, agentID string) ([]*domain.Message, error) {
	if err := p.expireMessagesForAgent(ctx, agentID); err != nil {
		return nil, err
	}
	// S-162: messages at or before the agent's chat_reset_at boundary are
	// excluded — the thread was archived to agent_memory and the user
	// cleared it. chat_reset_at IS NULL means "never reset".
	rows, err := p.pool.Query(ctx, `
		SELECT m.id::text, m.from_type, m.from_id::text, m.to_agent_id::text, m.squad_id::text,
		       m.type, m.payload, m.status, coalesce(m.correlation_id::text, ''), m.attempts, m.max_attempts,
		       m.next_retry_at, m.expires_at, m.timeout_at, m.terminal_reason, m.created_at, m.delivered_at
		FROM messages m
		JOIN agents a ON a.id = m.to_agent_id
		WHERE m.to_agent_id = $1
		  AND (a.chat_reset_at IS NULL OR m.created_at > a.chat_reset_at)
		ORDER BY m.created_at, m.id
	`, agentID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// CountMessagesByCorrelation (S-164) counts messages sharing a correlation_id
// so the send path can cap reply-chain depth. Empty IDs are rejected rather
// than silently matching uncorrelated traffic.
func (p *PostgresStore) CountMessagesByCorrelation(ctx context.Context, correlationID string) (int, error) {
	if strings.TrimSpace(correlationID) == "" {
		return 0, ErrInvalidInput
	}
	var count int
	err := p.pool.QueryRow(ctx, `
		SELECT count(*) FROM messages WHERE correlation_id = $1::uuid
	`, correlationID).Scan(&count)
	if err != nil {
		return 0, mapPgErr(err)
	}
	return count, nil
}

// ResetAgentChat (S-162) archives the current chat transcript into
// agent_memory and moves the agent's chat_reset_at boundary to now, in
// one transaction. Returns the number of messages archived (i.e. those
// at or before the new boundary) and the boundary timestamp. The
// transcript text is rendered by the caller; memory carries it with
// provenance 'chat_reset' so the agent can still recall it semantically.
func (p *PostgresStore) ResetAgentChat(ctx context.Context, agentID, squadID, transcript string, metadata json.RawMessage, embedding []float64, embeddingModel string) (int, time.Time, error) {
	resetAt := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, time.Time{}, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var archived int
	err = tx.QueryRow(ctx, `
		SELECT count(*)
		FROM messages m
		JOIN agents a ON a.id = m.to_agent_id
		WHERE m.to_agent_id = $1
		  AND (a.chat_reset_at IS NULL OR m.created_at > a.chat_reset_at)
	`, agentID).Scan(&archived)
	if err != nil {
		return 0, time.Time{}, mapPgErr(err)
	}

	if archived > 0 {
		_, err = tx.Exec(ctx, `
			INSERT INTO agent_memory (
				agent_id, squad_id, content, raw_content, trust_level, provenance,
				review_status, embedding, embedding_model, source_task_id, metadata
			)
			VALUES (
				nullif($1, '')::uuid, nullif($2, '')::uuid, $3, '', 'distilled', 'chat_reset',
				'approved', nullif($5, '')::vector, $6, NULL, $4
			)`, agentID, squadID, transcript, defaultJSON(metadata, "{}"), vectorLiteral(embedding), embeddingModel)
		if err != nil {
			return 0, time.Time{}, mapPgErr(err)
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE agents SET chat_reset_at = $2, updated_at = now() WHERE id = $1`, agentID, resetAt); err != nil {
		return 0, time.Time{}, mapPgErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, time.Time{}, mapPgErr(err)
	}
	return archived, resetAt, nil
}

// CancelChatTurn (S-175) marks the newest still-live user chat message for
// the agent as cancelled. "Live" = pending/delivered, after the chat-reset
// boundary, and not yet answered by the agent (a human-chat reply carries
// the trigger message id as its correlation_id). Returns ErrNotFound when
// there is no live turn.
func (p *PostgresStore) CancelChatTurn(ctx context.Context, agentID string) (*domain.Message, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE messages
		SET status = $2, terminal_reason = $3
		WHERE id = (
			SELECT m.id FROM messages m
			JOIN agents a ON a.id = m.to_agent_id
			WHERE m.to_agent_id = $1
			  AND m.from_type = 'user'
			  AND m.status IN ($4, $5)
			  AND (a.chat_reset_at IS NULL OR m.created_at > a.chat_reset_at)
			  AND NOT EXISTS (
				SELECT 1 FROM messages r
				WHERE r.from_type = 'agent' AND r.correlation_id = m.id
			  )
			ORDER BY m.created_at DESC, m.id DESC
			LIMIT 1
		)
		RETURNING id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		          type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		          next_retry_at, expires_at, timeout_at, terminal_reason, created_at, delivered_at
	`, agentID, domain.MessageCancelled, "cancelled by user", domain.MessagePending, domain.MessageDelivered)
	updated, err := scanMessage(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, updated.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) GetMessage(ctx context.Context, messageID string) (*domain.Message, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		       type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		       next_retry_at, expires_at, timeout_at, terminal_reason, created_at, delivered_at
		FROM messages
		WHERE id = $1
	`, messageID)
	return scanMessage(row)
}

func (p *PostgresStore) AckMessage(ctx context.Context, agentID string, messageID string) (*domain.Message, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE messages
		SET status = CASE WHEN status = $3 THEN $4 ELSE status END,
		    delivered_at = CASE WHEN status = $3 AND delivered_at IS NULL THEN now() ELSE delivered_at END
		WHERE id = $1 AND to_agent_id = $2
		RETURNING id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		          type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		          next_retry_at, expires_at, timeout_at, terminal_reason, created_at, delivered_at
	`, messageID, agentID, domain.MessagePending, domain.MessageDelivered)
	updated, err := scanMessage(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, messageID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) UpdateMessagePayload(ctx context.Context, messageID string, payload json.RawMessage, status domain.MessageStatus) (*domain.Message, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE messages
		SET payload = $2,
		    status = $3,
		    delivered_at = CASE WHEN $3 = 'delivered' AND delivered_at IS NULL THEN now() ELSE delivered_at END
		WHERE id = $1
		RETURNING id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		          type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		          next_retry_at, expires_at, timeout_at, terminal_reason, created_at, delivered_at
	`, messageID, payload, status)
	updated, err := scanMessage(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, messageID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) FailMessage(ctx context.Context, agentID string, messageID string, reason string) (*domain.Message, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE messages
		SET attempts = CASE WHEN status = $3 THEN attempts + 1 ELSE attempts END,
		    status = CASE
		        WHEN status <> $3 THEN status
		        WHEN expires_at <= now() THEN $4
		        WHEN attempts + 1 >= max_attempts THEN $5
		        ELSE status
		    END,
		    next_retry_at = CASE
		        WHEN status = $3 AND attempts + 1 < max_attempts AND expires_at > now() THEN now() + $6::interval
		        ELSE next_retry_at
		    END,
		    terminal_reason = CASE
		        WHEN status = $3 AND (attempts + 1 >= max_attempts OR expires_at <= now()) THEN left($7, $8)
		        ELSE terminal_reason
		    END,
		    delivered_at = CASE
		        WHEN status = $3 AND (attempts + 1 >= max_attempts OR expires_at <= now()) THEN now()
		        ELSE delivered_at
		    END
		WHERE id = $1 AND to_agent_id = $2
		RETURNING id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		          type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		          next_retry_at, expires_at, timeout_at, terminal_reason, created_at, delivered_at
	`, messageID, agentID, domain.MessagePending, domain.MessageExpired, domain.MessageDead, defaultMessageRetryDelay, trimMessageReason(reason), maxMessageTerminalReason)
	updated, err := scanMessage(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, messageID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) CreateInboxMessage(ctx context.Context, msg *domain.InboxMessage) (*domain.InboxMessage, error) {
	kind := msg.Kind
	if kind == "" {
		kind = domain.InboxActionRequired
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		INSERT INTO inbox_messages (squad_id, user_id, from_agent_id, task_id, kind, message, subject, body)
		VALUES (NULLIF($1, '')::uuid, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7, $8)
		RETURNING id::text, coalesce(squad_id::text, ''), user_id::text, coalesce(from_agent_id::text, ''),
		          coalesce(task_id::text, ''), kind, message, subject, body, read_at, created_at
	`, msg.SquadID, msg.UserID, msg.FromAgentID, msg.TaskID, kind, msg.Message, msg.Subject, msg.Body)
	created, err := scanInboxMessage(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, created.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) ListInboxMessages(ctx context.Context, userID string, unreadOnly bool, limit int) ([]*domain.InboxMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, coalesce(squad_id::text, ''), user_id::text, coalesce(from_agent_id::text, ''),
		       coalesce(task_id::text, ''), kind, message, subject, body, read_at, created_at
		FROM inbox_messages
		WHERE user_id = $1 AND (NOT $2::boolean OR read_at IS NULL)
		ORDER BY created_at DESC
		LIMIT $3
	`, userID, unreadOnly, limit)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	out := []*domain.InboxMessage{}
	for rows.Next() {
		msg, err := scanInboxMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgErr(err)
	}
	return out, nil
}

func (p *PostgresStore) MarkInboxMessageRead(ctx context.Context, userID string, id string) (*domain.InboxMessage, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE inbox_messages
		SET read_at = COALESCE(read_at, now())
		WHERE id = $1 AND user_id = $2
		RETURNING id::text, coalesce(squad_id::text, ''), user_id::text, coalesce(from_agent_id::text, ''),
		          coalesce(task_id::text, ''), kind, message, subject, body, read_at, created_at
	`, id, userID)
	updated, err := scanInboxMessage(row)
	if err != nil {
		return nil, err
	}
	if err := p.writePendingAuditsTx(ctx, tx, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) GetInboxMessage(ctx context.Context, id string) (*domain.InboxMessage, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, coalesce(squad_id::text, ''), user_id::text, coalesce(from_agent_id::text, ''),
		       coalesce(task_id::text, ''), kind, message, subject, body, read_at, created_at
		FROM inbox_messages WHERE id = $1
	`, id)
	return scanInboxMessage(row)
}

// DeleteInboxMessage (S-193) permanently removes an inbox message.
// Empty userID skips recipient scoping (platform-admin path, enforced
// by the handler).
func (p *PostgresStore) DeleteInboxMessage(ctx context.Context, id string, userID string) error {
	var tag pgconn.CommandTag
	var err error
	if userID == "" {
		tag, err = p.pool.Exec(ctx, `DELETE FROM inbox_messages WHERE id = $1`, id)
	} else {
		tag, err = p.pool.Exec(ctx, `DELETE FROM inbox_messages WHERE id = $1 AND user_id = $2`, id, userID)
	}
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateNotification (S-193) files a recipient-scoped bell alert.
func (p *PostgresStore) CreateNotification(ctx context.Context, n *domain.Notification) (*domain.Notification, error) {
	severity := n.Severity
	if severity == "" {
		severity = domain.NotificationWarning
	}
	row := p.pool.QueryRow(ctx, `
		INSERT INTO notifications (user_id, squad_id, task_id, agent_id, type, severity, message)
		VALUES ($1, NULLIF($2, '')::uuid, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7)
		RETURNING id::text, user_id::text, coalesce(squad_id::text, ''), coalesce(task_id::text, ''),
		          coalesce(agent_id::text, ''), type, severity, message, read_at, created_at
	`, n.UserID, n.SquadID, n.TaskID, n.AgentID, n.Type, severity, n.Message)
	return scanNotification(row)
}

func (p *PostgresStore) ListNotifications(ctx context.Context, userID string, unreadOnly bool, limit int) ([]*domain.Notification, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, user_id::text, coalesce(squad_id::text, ''), coalesce(task_id::text, ''),
		       coalesce(agent_id::text, ''), type, severity, message, read_at, created_at
		FROM notifications
		WHERE user_id = $1 AND (NOT $2::boolean OR read_at IS NULL)
		ORDER BY created_at DESC
		LIMIT $3
	`, userID, unreadOnly, limit)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	out := []*domain.Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgErr(err)
	}
	return out, nil
}

// HasRecentNotificationForTask (S-197) backs the stuck-scanner dedupe:
// one prior task_stuck row with created_at >= since suppresses re-filing.
func (p *PostgresStore) HasRecentNotificationForTask(ctx context.Context, taskID string, typ domain.NotificationType, since time.Time) (bool, error) {
	if taskID == "" {
		return false, nil
	}
	var exists bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM notifications
			WHERE task_id = $1::uuid AND type = $2 AND created_at >= $3
		)
	`, taskID, typ, since).Scan(&exists)
	if err != nil {
		return false, mapPgErr(err)
	}
	return exists, nil
}

// ListStaleInProgressTasks (S-197) finds in-progress tasks whose every
// liveness signal — task row, task thread messages, execution heartbeats
// — predates cutoff. NOT EXISTS keeps it a single indexed-ish sweep; the
// thread-activity check rides the squad_id index on messages.
func (p *PostgresStore) ListStaleInProgressTasks(ctx context.Context, cutoff time.Time) ([]*domain.Task, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT t.id::text, t.board_id::text, t.squad_id::text, t.title, t.description, t.status,
		       coalesce(t.assignee_agent_id::text, ''), t.created_by_type, t.created_by_id::text,
		       t.position, t.created_at, t.updated_at, coalesce(t.origin_message_id, ''), coalesce(t.workspace_resource_id, ''), coalesce(t.workspace_branch, ''), coalesce(t.workspace_commit_sha, ''), coalesce(t.result, ''), coalesce(t.result_status, ''), t.result_at, t.task_number
		FROM tasks t
		WHERE t.status = $1
		  AND t.updated_at < $2
		  AND NOT EXISTS (
		      SELECT 1 FROM messages m
		      WHERE m.squad_id = t.squad_id
		        AND m.created_at >= $2
		        AND m.payload->>'task_id' = t.id::text
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM task_executions e
		      WHERE e.task_id = t.id AND e.updated_at >= $2
		  )
		ORDER BY t.updated_at
	`, domain.TaskInProgress, cutoff)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	tasks := []*domain.Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, mapPgErr(rows.Err())
}

func (p *PostgresStore) MarkNotificationRead(ctx context.Context, userID string, id string) (*domain.Notification, error) {
	row := p.pool.QueryRow(ctx, `
		UPDATE notifications
		SET read_at = COALESCE(read_at, now())
		WHERE id = $1 AND user_id = $2
		RETURNING id::text, user_id::text, coalesce(squad_id::text, ''), coalesce(task_id::text, ''),
		          coalesce(agent_id::text, ''), type, severity, message, read_at, created_at
	`, id, userID)
	return scanNotification(row)
}

func (p *PostgresStore) MarkAllNotificationsRead(ctx context.Context, userID string) (int, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE notifications SET read_at = now()
		WHERE user_id = $1 AND read_at IS NULL
	`, userID)
	if err != nil {
		return 0, mapPgErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// DeleteReadNotificationsBefore (S-198) purges READ notifications created
// before cutoff. The read_at IS NOT NULL predicate is the guard: unread
// alerts are never touched, and inbox messages live in a separate table
// this sweep cannot reach (S-193: explicit user delete only).
func (p *PostgresStore) DeleteReadNotificationsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM notifications
		WHERE read_at IS NOT NULL AND created_at < $1
	`, cutoff.UTC())
	if err != nil {
		return 0, mapPgErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// GetNotificationPreferences (S-199) reads the user's mute list from
// user_notification_preferences. No row ⇒ empty MutedTypes (all types
// enabled by default — the default is deliberately not materialized).
func (p *PostgresStore) GetNotificationPreferences(ctx context.Context, userID string) (*domain.NotificationPreferences, error) {
	var muted []string
	err := p.pool.QueryRow(ctx, `
		SELECT muted_types FROM user_notification_preferences WHERE user_id = $1
	`, userID).Scan(&muted)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.NotificationPreferences{MutedTypes: []domain.NotificationType{}}, nil
	}
	if err != nil {
		return nil, mapPgErr(err)
	}
	out := make([]domain.NotificationType, 0, len(muted))
	for _, t := range muted {
		out = append(out, domain.NotificationType(t))
	}
	return &domain.NotificationPreferences{MutedTypes: out}, nil
}

// SetNotificationPreferences (S-199) upserts the user's mute list.
// A FK violation on users(id) maps through mapPgErr to ErrNotFound.
func (p *PostgresStore) SetNotificationPreferences(ctx context.Context, userID string, muted []domain.NotificationType) (*domain.NotificationPreferences, error) {
	mutedStr := make([]string, 0, len(muted))
	for _, t := range muted {
		mutedStr = append(mutedStr, string(t))
	}
	var stored []string
	err := p.pool.QueryRow(ctx, `
		INSERT INTO user_notification_preferences (user_id, muted_types, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE
		SET muted_types = EXCLUDED.muted_types, updated_at = now()
		RETURNING muted_types
	`, userID, mutedStr).Scan(&stored)
	if err != nil {
		return nil, mapPgErr(err)
	}
	out := make([]domain.NotificationType, 0, len(stored))
	for _, t := range stored {
		out = append(out, domain.NotificationType(t))
	}
	return &domain.NotificationPreferences{MutedTypes: out}, nil
}

func scanNotification(row scanner) (*domain.Notification, error) {
	var n domain.Notification
	var readAt sql.NullTime
	if err := row.Scan(
		&n.ID,
		&n.UserID,
		&n.SquadID,
		&n.TaskID,
		&n.AgentID,
		&n.Type,
		&n.Severity,
		&n.Message,
		&readAt,
		&n.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if readAt.Valid {
		t := readAt.Time
		n.ReadAt = &t
	}
	return &n, nil
}

func (p *PostgresStore) WaitForAgentWork(ctx context.Context, agentID string, timeout time.Duration) (bool, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return false, mapPgErr(err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `LISTEN skquad_agent_work`); err != nil {
		return false, mapPgErr(err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `UNLISTEN skquad_agent_work`)
	}()

	available, err := p.hasReadyWork(ctx, agentID)
	if err != nil || available || timeout <= 0 {
		return available, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		notification, err := conn.Conn().WaitForNotification(waitCtx)
		if err != nil {
			return classifyAgentWorkWaitError(err, ctx)
		}
		if notification.Channel == agentWorkNotifyChannel && notification.Payload == agentID {
			return true, nil
		}
	}
}

// classifyAgentWorkWaitError maps a WaitForNotification error to the
// WaitForAgentWork result: wait deadline/cancellation is a normal "no work
// arrived" outcome, anything else is a real failure.
func classifyAgentWorkWaitError(err error, ctx context.Context) (bool, error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	return false, mapPgErr(err)
}

func (p *PostgresStore) hasReadyWork(ctx context.Context, agentID string) (bool, error) {
	var available bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM tasks
			WHERE assignee_agent_id = $1
			  AND status IN ($2, $3)
		) OR EXISTS (
			SELECT 1
			FROM messages
			WHERE to_agent_id = $1
			  AND status = $4
			  AND next_retry_at <= now()
			  AND expires_at > now()
		)
	`, agentID, domain.TaskTodo, domain.TaskInProgress, domain.MessagePending).Scan(&available)
	return available, mapPgErr(err)
}

func (p *PostgresStore) expireMessagesForAgent(ctx context.Context, agentID string) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE messages
		SET status = $2,
		    terminal_reason = CASE WHEN terminal_reason = '' THEN 'message expired before delivery' ELSE terminal_reason END,
		    delivered_at = CASE WHEN delivered_at IS NULL THEN now() ELSE delivered_at END
		WHERE to_agent_id = $1 AND status = $3 AND expires_at <= now()
	`, agentID, domain.MessageExpired, domain.MessagePending)
	return mapPgErr(err)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanUser(row scanner) (*domain.User, error) {
	var u domain.User
	if err := row.Scan(
		&u.ID,
		&u.OIDCIssuer,
		&u.OIDCSubject,
		&u.Email,
		&u.EmailVerified,
		&u.Name,
		&u.Role,
		&u.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &u, nil
}

func scanSquad(row scanner) (*domain.Squad, error) {
	var s domain.Squad
	if err := row.Scan(
		&s.ID,
		&s.Name,
		&s.Mission,
		&s.Prompt,
		&s.OperatingModel,
		&s.OwnerID,
		&s.Namespace,
		&s.Status,
		&s.CreatedAt,
		&s.UpdatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &s, nil
}

func scanAgent(row scanner) (*domain.Agent, error) {
	var a domain.Agent
	var chatResetAt sql.NullTime
	if err := row.Scan(
		&a.ID,
		&a.SquadID,
		&a.Name,
		&a.Role,
		&a.SystemPrompt,
		&a.IdentityID,
		&a.AIModelID,
		&a.FallbackAIModelID,
		&a.Permissions,
		&a.IdleTimeoutSec,
		&a.ThinkingLevel,
		&a.StorageEnabled,
		&a.StorageSize,
		&a.DeploymentName,
		&chatResetAt,
		&a.Status,
		&a.CreatedAt,
		&a.UpdatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if chatResetAt.Valid {
		a.ChatResetAt = chatResetAt.Time
	}
	return &a, nil
}

func scanAgentIdentity(row scanner) (*domain.AgentIdentity, error) {
	var i domain.AgentIdentity
	var rotatedAt sql.NullTime
	if err := row.Scan(
		&i.ID,
		&i.AgentID,
		&i.CredentialRef,
		&i.CredentialHash,
		&i.VirtualKeyRef,
		&i.CreatedBy,
		&i.CreatedAt,
		&rotatedAt,
		&i.GatewayKeyToken,
		&i.GatewayKeyStatus,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if rotatedAt.Valid {
		i.RotatedAt = rotatedAt.Time
	}
	return &i, nil
}

func scanKubernetesOutboxRows(rows pgx.Rows) ([]*domain.KubernetesOutboxEvent, error) {
	events := []*domain.KubernetesOutboxEvent{}
	for rows.Next() {
		event, err := scanKubernetesOutbox(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, mapPgErr(rows.Err())
}

func scanKubernetesOutbox(row scanner) (*domain.KubernetesOutboxEvent, error) {
	var event domain.KubernetesOutboxEvent
	var lockedUntil sql.NullTime
	if err := row.Scan(
		&event.ID,
		&event.AggregateType,
		&event.AggregateID,
		&event.Operation,
		&event.Payload,
		&event.Status,
		&event.Attempts,
		&event.LastError,
		&event.NextAttemptAt,
		&lockedUntil,
		&event.CreatedAt,
		&event.UpdatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if lockedUntil.Valid {
		event.LockedUntil = lockedUntil.Time
	}
	return &event, nil
}

func scanBoard(row scanner) (*domain.Board, error) {
	var b domain.Board
	if err := row.Scan(&b.ID, &b.SquadID, &b.CreatedAt); err != nil {
		return nil, mapPgErr(err)
	}
	return &b, nil
}

func scanGrant(row scanner) (*domain.AccessGrant, error) {
	var g domain.AccessGrant
	if err := row.Scan(
		&g.ID,
		&g.SquadID,
		&g.GranteeType,
		&g.GranteeID,
		&g.Permissions,
		&g.GrantedBy,
		&g.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &g, nil
}

func scanAIProvider(row scanner) (*domain.AIProvider, error) {
	var p domain.AIProvider
	if err := row.Scan(
		&p.ID,
		&p.Name,
		&p.Kind,
		&p.BaseURL,
		&p.APIKeyRef,
		&p.Status,
		&p.RegisteredBy,
		&p.CreatedAt,
		&p.APIKeyMask,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &p, nil
}

func scanAIModel(row scanner) (*domain.AIModel, error) {
	var m domain.AIModel
	var updatedAt sql.NullTime
	if err := row.Scan(
		&m.ID,
		&m.ProviderID,
		&m.DisplayName,
		&m.ModelName,
		&m.ContextWindow,
		&m.SupportsTools,
		&m.SupportsVision,
		&m.Pricing,
		&m.LongContextThresholdTokens,
		&m.Status,
		&m.RegisteredBy,
		&m.CreatedAt,
		&updatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if updatedAt.Valid {
		m.UpdatedAt = updatedAt.Time
	}
	return &m, nil
}

func scanUserModelGrant(row scanner) (*domain.UserModelGrant, error) {
	var g domain.UserModelGrant
	if err := row.Scan(
		&g.ID,
		&g.GranteeUserID,
		&g.AIModelID,
		&g.GrantedBy,
		&g.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &g, nil
}

func scanResource(row scanner) (*domain.RegistryResource, error) {
	var r domain.RegistryResource
	if err := row.Scan(
		&r.ID,
		&r.Type,
		&r.Name,
		&r.Description,
		&r.Endpoint,
		&r.AuthRef,
		&r.Manifest,
		&r.Status,
		&r.RegisteredBy,
		&r.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &r, nil
}

func scanAgentPermission(row scanner) (*domain.AgentPermission, error) {
	var p domain.AgentPermission
	if err := row.Scan(
		&p.ID,
		&p.AgentID,
		&p.ResourceType,
		&p.ResourceID,
		&p.GrantedBy,
		&p.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &p, nil
}

func scanTask(row scanner) (*domain.Task, error) {
	var t domain.Task
	var resultAt sql.NullTime
	if err := row.Scan(
		&t.ID,
		&t.BoardID,
		&t.SquadID,
		&t.Title,
		&t.Description,
		&t.Status,
		&t.AssigneeAgentID,
		&t.CreatedByType,
		&t.CreatedByID,
		&t.Position,
		&t.CreatedAt,
		&t.UpdatedAt,
		&t.OriginMessageID,
		&t.WorkspaceResourceID,
		&t.WorkspaceBranch,
		&t.WorkspaceCommitSHA,
		&t.Result,
		&t.ResultStatus,
		&resultAt,
		&t.TaskNumber,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if resultAt.Valid {
		t.ResultAt = resultAt.Time
	}
	return &t, nil
}

// SetTaskExecutionPromptSHA records the composed-prompt sha on the agent's
// active execution for a task (S-PROMPT WP5 run-audit, ADR-0011 D5).
func (p *PostgresStore) SetTaskExecutionPromptSHA(ctx context.Context, agentID string, taskID string, promptSHA string) (*domain.TaskExecution, error) {
	row := p.pool.QueryRow(ctx, `
		UPDATE task_executions
		SET prompt_sha = $3, updated_at = now()
		WHERE agent_id = $1 AND task_id = $2 AND status = $4
		RETURNING id::text, task_id::text, agent_id::text, worker_id, fencing_token,
		          status, lease_expires_at, coalesce(result_status, ''), result_summary,
		          prompt_sha, started_at, completed_at, updated_at
	`, agentID, taskID, promptSHA, domain.TaskExecutionActive)
	return scanTaskExecution(row)
}

// GetLatestTaskExecution returns the most recent execution row for a task
// regardless of status (S-PROMPT WP5 task-detail run-audit).
func (p *PostgresStore) GetLatestTaskExecution(ctx context.Context, taskID string) (*domain.TaskExecution, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, task_id::text, agent_id::text, worker_id, fencing_token,
		       status, lease_expires_at, coalesce(result_status, ''), result_summary,
		       prompt_sha, started_at, completed_at, updated_at
		FROM task_executions
		WHERE task_id = $1
		ORDER BY started_at DESC, id
		LIMIT 1
	`, taskID)
	return scanTaskExecution(row)
}

func scanTaskExecution(row scanner) (*domain.TaskExecution, error) {
	var exec domain.TaskExecution
	var completedAt sql.NullTime
	if err := row.Scan(
		&exec.ID,
		&exec.TaskID,
		&exec.AgentID,
		&exec.WorkerID,
		&exec.FencingToken,
		&exec.Status,
		&exec.LeaseExpiresAt,
		&exec.ResultStatus,
		&exec.ResultSummary,
		&exec.PromptSHA,
		&exec.StartedAt,
		&completedAt,
		&exec.UpdatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if completedAt.Valid {
		exec.CompletedAt = completedAt.Time
	}
	return &exec, nil
}

func attachTaskExecution(task *domain.Task, exec *domain.TaskExecution) *domain.Task {
	if task == nil || exec == nil {
		return task
	}
	task.ExecutionID = exec.ID
	task.WorkerID = exec.WorkerID
	task.FencingToken = exec.FencingToken
	task.LeaseExpiresAt = exec.LeaseExpiresAt
	return task
}

func scanAgentMemory(row scanner) (*domain.AgentMemory, error) {
	var memory domain.AgentMemory
	var embeddingText string
	if err := row.Scan(
		&memory.ID,
		&memory.AgentID,
		&memory.SquadID,
		&memory.Content,
		&memory.RawContent,
		&memory.TrustLevel,
		&memory.Provenance,
		&memory.ReviewStatus,
		&embeddingText,
		&memory.EmbeddingModel,
		&memory.SourceTaskID,
		&memory.Metadata,
		&memory.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	memory.Embedding = parseVectorText(embeddingText)
	return &memory, nil
}

func scanMessage(row scanner) (*domain.Message, error) {
	var msg domain.Message
	var deliveredAt sql.NullTime
	var timeoutAt sql.NullTime
	if err := row.Scan(
		&msg.ID,
		&msg.FromType,
		&msg.FromID,
		&msg.ToAgentID,
		&msg.SquadID,
		&msg.Type,
		&msg.Payload,
		&msg.Status,
		&msg.CorrelationID,
		&msg.Attempts,
		&msg.MaxAttempts,
		&msg.NextRetryAt,
		&msg.ExpiresAt,
		&timeoutAt,
		&msg.TerminalReason,
		&msg.CreatedAt,
		&deliveredAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if timeoutAt.Valid {
		msg.TimeoutAt = timeoutAt.Time
	}
	if deliveredAt.Valid {
		msg.DeliveredAt = deliveredAt.Time
	}
	return &msg, nil
}

func scanInboxMessage(row scanner) (*domain.InboxMessage, error) {
	var msg domain.InboxMessage
	var readAt sql.NullTime
	if err := row.Scan(
		&msg.ID,
		&msg.SquadID,
		&msg.UserID,
		&msg.FromAgentID,
		&msg.TaskID,
		&msg.Kind,
		&msg.Message,
		&msg.Subject,
		&msg.Body,
		&readAt,
		&msg.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if readAt.Valid {
		t := readAt.Time
		msg.ReadAt = &t
	}
	return &msg, nil
}

func scanMessages(rows pgx.Rows) ([]*domain.Message, error) {
	messages := make([]*domain.Message, 0)
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, mapPgErr(rows.Err())
}

func scanAuditEntry(row scanner) (*domain.AuditEntry, error) {
	var entry domain.AuditEntry
	if err := row.Scan(
		&entry.ID,
		&entry.ActorType,
		&entry.ActorID,
		&entry.Action,
		&entry.ResourceType,
		&entry.ResourceID,
		&entry.SquadID,
		&entry.Metadata,
		&entry.Timestamp,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &entry, nil
}

func mapPgErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	// Foreign key violation: RESTRICTed deletes (e.g. a provider still
	// referenced by ai_models) surface as a detectable conflict.
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrConflict
	}
	return err
}

func defaultJSON(value []byte, fallback string) []byte {
	if len(value) == 0 {
		return []byte(fallback)
	}
	return value
}

func defaultSquadStatus(status domain.SquadStatus) domain.SquadStatus {
	if status == "" {
		return domain.SquadActive
	}
	return status
}

func defaultAgentStatus(status domain.AgentStatus) domain.AgentStatus {
	if status == "" {
		return domain.AgentIdle
	}
	return status
}

// defaultStorageSize keeps the persisted storage_size aligned with the
// CRD-level default (S-135/S-138): an empty size is stored as "2Gi" so
// the DB row always carries a concrete quantity even when the caller
// enabled storage without naming a size.
func defaultStorageSize(size string) string {
	if strings.TrimSpace(size) == "" {
		return "2Gi"
	}
	return size
}

func defaultTaskStatus(status domain.TaskStatus) domain.TaskStatus {
	if status == "" {
		return domain.TaskTodo
	}
	return status
}

func defaultMessageType(messageType domain.MessageType) domain.MessageType {
	if messageType == "" {
		return domain.MessageConsult
	}
	return messageType
}

func defaultMessageStatus(status domain.MessageStatus) domain.MessageStatus {
	if status == "" {
		return domain.MessagePending
	}
	return status
}

func defaultResourceStatus(status domain.ResourceStatus) domain.ResourceStatus {
	if status == "" {
		return domain.ResourceActive
	}
	return status
}

func nullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func defaultCurrency(currency string) string {
	if currency == "" {
		return "USD"
	}
	return currency
}

func nullableTimeText(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// --- S-158: prompt templates -------------------------------------------------

const promptTemplateColumns = `id::text, name, description, content, applies_to, created_by::text, created_at, updated_at`

func scanPromptTemplate(row scanner) (*domain.PromptTemplate, error) {
	var t domain.PromptTemplate
	if err := row.Scan(
		&t.ID,
		&t.Name,
		&t.Description,
		&t.Content,
		&t.AppliesTo,
		&t.CreatedBy,
		&t.CreatedAt,
		&t.UpdatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &t, nil
}

func (p *PostgresStore) CreatePromptTemplate(ctx context.Context, t *domain.PromptTemplate) (*domain.PromptTemplate, error) {
	row := p.pool.QueryRow(ctx, `
		INSERT INTO prompt_templates (name, description, content, applies_to, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+promptTemplateColumns,
		t.Name, t.Description, t.Content, t.AppliesTo, t.CreatedBy)
	created, err := scanPromptTemplate(row)
	if err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) ListPromptTemplates(ctx context.Context) ([]*domain.PromptTemplate, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+promptTemplateColumns+`
		FROM prompt_templates
		ORDER BY lower(name)`)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	out := make([]*domain.PromptTemplate, 0)
	for rows.Next() {
		t, err := scanPromptTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, mapPgErr(rows.Err())
}

func (p *PostgresStore) GetPromptTemplate(ctx context.Context, id string) (*domain.PromptTemplate, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT `+promptTemplateColumns+`
		FROM prompt_templates
		WHERE id = $1`, id)
	return scanPromptTemplate(row)
}

func (p *PostgresStore) UpdatePromptTemplate(ctx context.Context, t *domain.PromptTemplate) (*domain.PromptTemplate, error) {
	row := p.pool.QueryRow(ctx, `
		UPDATE prompt_templates
		SET name = $2, description = $3, content = $4, applies_to = $5, updated_at = now()
		WHERE id = $1
		RETURNING `+promptTemplateColumns,
		t.ID, t.Name, t.Description, t.Content, t.AppliesTo)
	updated, err := scanPromptTemplate(row)
	if err != nil {
		return nil, mapPgErr(err)
	}
	return updated, nil
}

func (p *PostgresStore) DeletePromptTemplate(ctx context.Context, id string) error {
	res, err := p.pool.Exec(ctx, `DELETE FROM prompt_templates WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// BulkDeletePromptTemplates (S-214) deletes many templates in a single
// statement — atomic by construction. Ids are compared as text so a
// malformed id can't blow up the uuid cast; unknown ids simply don't
// match and are excluded from the returned count.
func (p *PostgresStore) BulkDeletePromptTemplates(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, ErrInvalidInput
	}
	res, err := p.pool.Exec(ctx, `DELETE FROM prompt_templates WHERE id::text = ANY($1)`, ids)
	if err != nil {
		return 0, mapPgErr(err)
	}
	return int(res.RowsAffected()), nil
}

// --- S-194: image uploads -------------------------------------------------

func (p *PostgresStore) CreateUpload(ctx context.Context, u *domain.Upload) (*domain.Upload, error) {
	if u == nil || u.SquadID == "" {
		return nil, ErrInvalidInput
	}
	row := p.pool.QueryRow(ctx, `
		INSERT INTO uploads (squad_id, uploader_id, filename, content_type, size_bytes, data)
		VALUES ($1, nullif($2, '')::uuid, $3, $4, $5, $6)
		RETURNING id::text, squad_id::text, coalesce(uploader_id::text, ''), filename, content_type, size_bytes, created_at
	`, u.SquadID, u.UploaderID, u.Filename, u.ContentType, u.SizeBytes, u.Data)
	var out domain.Upload
	if err := row.Scan(&out.ID, &out.SquadID, &out.UploaderID, &out.Filename, &out.ContentType, &out.SizeBytes, &out.CreatedAt); err != nil {
		return nil, mapPgErr(err)
	}
	out.Data = u.Data
	return &out, nil
}

func (p *PostgresStore) GetUpload(ctx context.Context, uploadID string) (*domain.Upload, error) {
	if _, err := uuid.Parse(uploadID); err != nil {
		return nil, ErrNotFound
	}
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, squad_id::text, coalesce(uploader_id::text, ''), filename, content_type, size_bytes, data, created_at
		FROM uploads WHERE id = $1
	`, uploadID)
	var out domain.Upload
	if err := row.Scan(&out.ID, &out.SquadID, &out.UploaderID, &out.Filename, &out.ContentType, &out.SizeBytes, &out.Data, &out.CreatedAt); err != nil {
		return nil, mapPgErr(err)
	}
	return &out, nil
}
