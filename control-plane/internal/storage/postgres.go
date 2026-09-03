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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
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
		SELECT id::text, squad_id::text, name, role, coalesce(identity_id::text, ''),
		       coalesce(default_provider::text, ''), default_model, permissions, idle_timeout_sec, status, created_at, updated_at
		FROM agents
		WHERE id = $1
	`, id)
	return scanAgent(row)
}

func getAgentIdentityTx(ctx context.Context, tx pgx.Tx, agentID string) (*domain.AgentIdentity, error) {
	row := tx.QueryRow(ctx, `
		SELECT id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at
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

	var users []*domain.User
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
		INSERT INTO squads (name, mission, operating_model, owner_id, namespace, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, name, mission, operating_model, owner_id::text, namespace, status, created_at, updated_at
	`, s.Name, s.Mission, defaultJSON(s.OperatingModel, "{}"), s.OwnerID, s.Namespace, defaultSquadStatus(s.Status))
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
	if err := p.enqueueSquadOutboxTx(ctx, tx, domain.KubernetesOpUpsertSquad, created); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetSquad(ctx context.Context, id string) (*domain.Squad, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, name, mission, operating_model, owner_id::text, namespace, status, created_at, updated_at
		FROM squads
		WHERE id = $1
	`, id)
	return scanSquad(row)
}

func (p *PostgresStore) GetSquadByName(ctx context.Context, ownerID, name string) (*domain.Squad, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, name, mission, operating_model, owner_id::text, namespace, status, created_at, updated_at
		FROM squads
		WHERE owner_id = $1 AND lower(name) = lower($2)
	`, ownerID, name)
	return scanSquad(row)
}

func (p *PostgresStore) UpdateSquad(ctx context.Context, s *domain.Squad) (*domain.Squad, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE squads
		SET name = $2,
		    mission = $3,
		    operating_model = $4,
		    status = $5,
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, name, mission, operating_model, owner_id::text, namespace, status, created_at, updated_at
	`, s.ID, s.Name, s.Mission, defaultJSON(s.OperatingModel, "{}"), defaultSquadStatus(s.Status))
	updated, err := scanSquad(row)
	if err != nil {
		return nil, err
	}
	if err := p.enqueueSquadOutboxTx(ctx, tx, domain.KubernetesOpUpsertSquad, updated); err != nil {
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
		SELECT id::text, name, mission, operating_model, owner_id::text, namespace, status, created_at, updated_at
		FROM squads
		WHERE id = $1
	`, id)
	squad, err := scanSquad(row)
	if err != nil {
		return err
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
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) ListSquads(ctx context.Context, ownerID string) ([]*domain.Squad, error) {
	var rows pgx.Rows
	var err error
	if ownerID == "" {
		rows, err = p.pool.Query(ctx, `
			SELECT id::text, name, mission, operating_model, owner_id::text, namespace, status, created_at, updated_at
			FROM squads
			ORDER BY name
		`)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT id::text, name, mission, operating_model, owner_id::text, namespace, status, created_at, updated_at
			FROM squads
			WHERE owner_id = $1
			ORDER BY name
		`, ownerID)
	}
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	var squads []*domain.Squad
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
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		INSERT INTO agents (squad_id, name, role, default_provider, default_model, permissions, idle_timeout_sec, status)
		VALUES ($1, $2, $3, nullif($4, '')::uuid, $5, $6, $7, $8)
		RETURNING id::text, squad_id::text, name, role, coalesce(identity_id::text, ''),
		          coalesce(default_provider::text, ''), default_model, permissions, idle_timeout_sec, status, created_at, updated_at
	`, a.SquadID, a.Name, a.Role, a.DefaultProvider, a.DefaultModel, defaultJSON(a.Permissions, "[]"), a.IdleTimeoutSec, defaultAgentStatus(a.Status))
	created, err := scanAgent(row)
	if err != nil {
		return nil, err
	}
	if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpUpsertAgent, created); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, squad_id::text, name, role, coalesce(identity_id::text, ''),
		       coalesce(default_provider::text, ''), default_model, permissions, idle_timeout_sec, status, created_at, updated_at
		FROM agents
		WHERE id = $1
	`, id)
	return scanAgent(row)
}

func (p *PostgresStore) UpdateAgent(ctx context.Context, a *domain.Agent) (*domain.Agent, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
		UPDATE agents
		SET name = $2,
		    role = $3,
		    default_provider = nullif($4, '')::uuid,
		    default_model = $5,
		    permissions = $6,
		    idle_timeout_sec = $7,
		    status = $8,
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, squad_id::text, name, role, coalesce(identity_id::text, ''),
		          coalesce(default_provider::text, ''), default_model, permissions, idle_timeout_sec, status, created_at, updated_at
	`, a.ID, a.Name, a.Role, a.DefaultProvider, a.DefaultModel, defaultJSON(a.Permissions, "[]"), a.IdleTimeoutSec, defaultAgentStatus(a.Status))
	updated, err := scanAgent(row)
	if err != nil {
		return nil, err
	}
	if err := p.enqueueAgentOutboxTx(ctx, tx, domain.KubernetesOpUpsertAgent, updated); err != nil {
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
		SELECT id::text, squad_id::text, name, role, coalesce(identity_id::text, ''),
		       coalesce(default_provider::text, ''), default_model, permissions, idle_timeout_sec, status, created_at, updated_at
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
	return mapPgErr(tx.Commit(ctx))
}

func (p *PostgresStore) ListAgents(ctx context.Context, squadID string) ([]*domain.Agent, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, squad_id::text, name, role, coalesce(identity_id::text, ''),
		       coalesce(default_provider::text, ''), default_model, permissions, idle_timeout_sec, status, created_at, updated_at
		FROM agents
		WHERE squad_id = $1
		ORDER BY name
	`, squadID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	var agents []*domain.Agent
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
		RETURNING id::text, squad_id::text, name, role, coalesce(identity_id::text, ''),
		          coalesce(default_provider::text, ''), default_model, permissions, idle_timeout_sec, status, created_at, updated_at
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
		RETURNING id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at
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
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return created, nil
}

func (p *PostgresStore) GetAgentIdentity(ctx context.Context, agentID string) (*domain.AgentIdentity, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at
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
		    rotated_at = now()
		WHERE agent_id = $1
		RETURNING id::text, agent_id::text, credential_ref, credential_hash, coalesce(virtual_key_ref, ''), created_by::text, created_at, rotated_at
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
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return identity, nil
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

	var grants []*domain.AccessGrant
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

func (p *PostgresStore) CreateLLMProvider(ctx context.Context, provider *domain.LLMProvider) (*domain.LLMProvider, error) {
	row := p.pool.QueryRow(ctx, `
		INSERT INTO llm_providers (name, kind, base_url, api_key_ref, default_model, models, pricing, status, registered_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id::text, name, kind, base_url, api_key_ref, default_model, models, pricing, status, registered_by::text, created_at
	`, provider.Name, provider.Kind, provider.BaseURL, provider.APIKeyRef, provider.DefaultModel, defaultJSON(provider.Models, "[]"), defaultJSON(provider.Pricing, "{}"), defaultResourceStatus(provider.Status), provider.RegisteredBy)
	return scanLLMProvider(row)
}

func (p *PostgresStore) GetLLMProvider(ctx context.Context, id string) (*domain.LLMProvider, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, name, kind, base_url, api_key_ref, default_model, models, pricing, status, registered_by::text, created_at
		FROM llm_providers
		WHERE id = $1
	`, id)
	return scanLLMProvider(row)
}

func (p *PostgresStore) UpdateLLMProvider(ctx context.Context, provider *domain.LLMProvider) (*domain.LLMProvider, error) {
	row := p.pool.QueryRow(ctx, `
		UPDATE llm_providers
		SET name = $2,
		    kind = $3,
		    base_url = $4,
		    api_key_ref = $5,
		    default_model = $6,
		    models = $7,
		    pricing = $8,
		    status = $9
		WHERE id = $1
		RETURNING id::text, name, kind, base_url, api_key_ref, default_model, models, pricing, status, registered_by::text, created_at
	`, provider.ID, provider.Name, provider.Kind, provider.BaseURL, provider.APIKeyRef, provider.DefaultModel, defaultJSON(provider.Models, "[]"), defaultJSON(provider.Pricing, "{}"), defaultResourceStatus(provider.Status))
	return scanLLMProvider(row)
}

func (p *PostgresStore) DeprecateLLMProvider(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE llm_providers
		SET status = $2
		WHERE id = $1
	`, id, domain.ResourceDeprecated)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) ListLLMProviders(ctx context.Context) ([]*domain.LLMProvider, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, name, kind, base_url, api_key_ref, default_model, models, pricing, status, registered_by::text, created_at
		FROM llm_providers
		ORDER BY name
	`)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	var providers []*domain.LLMProvider
	for rows.Next() {
		provider, err := scanLLMProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, mapPgErr(rows.Err())
}

func (p *PostgresStore) CreateResource(ctx context.Context, resource *domain.RegistryResource) (*domain.RegistryResource, error) {
	row := p.pool.QueryRow(ctx, `
		INSERT INTO registry_resources (
			type, name, description, endpoint, auth_ref, manifest, status, registered_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id::text, type, name, description, endpoint, auth_ref, manifest, status, registered_by::text, created_at
	`, resource.Type, resource.Name, resource.Description, resource.Endpoint, resource.AuthRef, defaultJSON(resource.Manifest, "{}"), defaultResourceStatus(resource.Status), resource.RegisteredBy)
	return scanResource(row)
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
	row := p.pool.QueryRow(ctx, `
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
	return scanResource(row)
}

func (p *PostgresStore) DeprecateResource(ctx context.Context, typ domain.ResourceType, id string) error {
	tag, err := p.pool.Exec(ctx, `
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
	return nil
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

	var resources []*domain.RegistryResource
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

	var perms []*domain.AgentPermission
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
			cost, currency, timestamp
		)
		VALUES ($1, $2, nullif($3, '')::uuid, nullif($4, '')::uuid, $5, $6, $7, $8, $9, coalesce(nullif($10, ''), now()::text)::timestamptz)
	`, event.AgentID, event.SquadID, event.TaskID, event.ProviderID, event.Model, event.InputTokens, event.OutputTokens, event.Cost, defaultCurrency(event.Currency), nullableTimeText(event.Timestamp))
	return mapPgErr(err)
}

func (p *PostgresStore) SumMetering(ctx context.Context, squadID, agentID string) (*domain.MeteringEvent, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT coalesce(sum(input_tokens), 0)::integer,
		       coalesce(sum(output_tokens), 0)::integer,
		       coalesce(sum(cost), 0)::double precision,
		       coalesce(max(currency), 'USD'),
		       max(timestamp)
		FROM metering
		WHERE ($1 = '' OR squad_id = nullif($1, '')::uuid)
		  AND ($2 = '' OR agent_id = nullif($2, '')::uuid)
	`, squadID, agentID)

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

	var entries []*domain.AuditEntry
	for rows.Next() {
		entry, err := scanAuditEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, mapPgErr(rows.Err())
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
	row := p.pool.QueryRow(ctx, `
		INSERT INTO tasks (
			board_id, squad_id, title, description, status, assignee_agent_id,
			created_by_type, created_by_id, position
		)
		VALUES (
			$1, $2, $3, $4, $5, nullif($6, '')::uuid,
			$7, $8,
			coalesce((SELECT max(position) + 1 FROM tasks WHERE board_id = $1 AND status = $5), 1)
		)
		RETURNING id::text, board_id::text, squad_id::text, title, description, status,
		          coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		          position, created_at, updated_at
	`, t.BoardID, t.SquadID, t.Title, t.Description, defaultTaskStatus(t.Status), t.AssigneeAgentID, t.CreatedByType, t.CreatedByID)
	return scanTask(row)
}

func (p *PostgresStore) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id::text, board_id::text, squad_id::text, title, description, status,
		       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		       position, created_at, updated_at
		FROM tasks
		WHERE id = $1
	`, id)
	return scanTask(row)
}

func (p *PostgresStore) UpdateTask(ctx context.Context, t *domain.Task) (*domain.Task, error) {
	row := p.pool.QueryRow(ctx, `
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
		          position, created_at, updated_at
	`, t.ID, t.Title, t.Description, t.AssigneeAgentID, defaultTaskStatus(t.Status))
	return scanTask(row)
}

func (p *PostgresStore) DeleteTask(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM tasks WHERE id = $1`, id)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) ListTasks(ctx context.Context, boardID string, status domain.TaskStatus) ([]*domain.Task, error) {
	var rows pgx.Rows
	var err error
	if status == "" {
		rows, err = p.pool.Query(ctx, `
			SELECT id::text, board_id::text, squad_id::text, title, description, status,
			       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
			       position, created_at, updated_at
			FROM tasks
			WHERE board_id = $1
			ORDER BY status, position
		`, boardID)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT id::text, board_id::text, squad_id::text, title, description, status,
			       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
			       position, created_at, updated_at
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
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, board_id::text, squad_id::text, title, description, status,
		       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		       position, created_at, updated_at
		FROM tasks
		WHERE assignee_agent_id = $1
		ORDER BY status, position
	`, agentID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()

	var tasks []*domain.Task
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
		       position, created_at, updated_at
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
		          position, created_at, updated_at
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
		          started_at, completed_at, updated_at
	`, taskID, agentID, workerID, fmt.Sprintf("%d seconds", int(leaseFor/time.Second)))
	return scanTaskExecution(row)
}

func (p *PostgresStore) ListBoardTaskExecutions(ctx context.Context, boardID string) ([]*domain.TaskExecution, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT e.id::text, e.task_id::text, e.agent_id::text, e.worker_id, e.fencing_token,
		       e.status, e.lease_expires_at, coalesce(e.result_status, ''), e.result_summary,
		       e.started_at, e.completed_at, e.updated_at
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
		          started_at, completed_at, updated_at
	`, executionID, agentID, fencingToken, fmt.Sprintf("%d seconds", int(leaseFor/time.Second)))
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
func (p *PostgresStore) ReapExpiredTaskExecutions(ctx context.Context, cutoff time.Time) (int, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, mapPgErr(err)
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
		RETURNING task_id::text
	`, domain.TaskExecutionActive, domain.TaskExecutionExpired, cutoff)
	if err != nil {
		return 0, mapPgErr(err)
	}
	taskIDs := []string{}
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return 0, mapPgErr(err)
		}
		taskIDs = append(taskIDs, taskID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, mapPgErr(err)
	}
	if len(taskIDs) == 0 {
		return 0, mapPgErr(tx.Commit(ctx))
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
		return 0, mapPgErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, mapPgErr(err)
	}
	return len(taskIDs), nil
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
		    updated_at = now()
		WHERE id = (SELECT id FROM existing)
		RETURNING id::text, board_id::text, squad_id::text, title, description, status,
		          coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		          position, created_at, updated_at
	`, taskID, agentID, status)
	task, err := scanTask(row)
	if err != nil {
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

	var memories []*domain.AgentMemory
	for rows.Next() {
		memory, err := scanAgentMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, mapPgErr(rows.Err())
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
	`, limit, fmt.Sprintf("%d seconds", int(leaseFor/time.Second)))
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
	`, id, lastError, fmt.Sprintf("%d seconds", int(retryAfter/time.Second)))
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
	row := p.pool.QueryRow(ctx, `
		INSERT INTO messages (
			from_type, from_id, to_agent_id, squad_id, type, payload, status, correlation_id, max_attempts, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, '')::uuid, $9, $10)
		RETURNING id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		          type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		          next_retry_at, expires_at, terminal_reason, created_at, delivered_at
	`, m.FromType, m.FromID, m.ToAgentID, m.SquadID, defaultMessageType(m.Type), defaultJSON(m.Payload, "{}"), defaultMessageStatus(m.Status), m.CorrelationID, maxAttempts, expiresAt)
	return scanMessage(row)
}

func (p *PostgresStore) ListPendingMessages(ctx context.Context, agentID string) ([]*domain.Message, error) {
	if err := p.expireMessagesForAgent(ctx, agentID); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		       type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		       next_retry_at, expires_at, terminal_reason, created_at, delivered_at
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
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		       type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		       next_retry_at, expires_at, terminal_reason, created_at, delivered_at
		FROM messages
		WHERE to_agent_id = $1
		ORDER BY created_at, id
	`, agentID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (p *PostgresStore) AckMessage(ctx context.Context, agentID string, messageID string) (*domain.Message, error) {
	row := p.pool.QueryRow(ctx, `
		UPDATE messages
		SET status = CASE WHEN status = $3 THEN $4 ELSE status END,
		    delivered_at = CASE WHEN status = $3 AND delivered_at IS NULL THEN now() ELSE delivered_at END
		WHERE id = $1 AND to_agent_id = $2
		RETURNING id::text, from_type, from_id::text, to_agent_id::text, squad_id::text,
		          type, payload, status, coalesce(correlation_id::text, ''), attempts, max_attempts,
		          next_retry_at, expires_at, terminal_reason, created_at, delivered_at
	`, messageID, agentID, domain.MessagePending, domain.MessageDelivered)
	return scanMessage(row)
}

func (p *PostgresStore) FailMessage(ctx context.Context, agentID string, messageID string, reason string) (*domain.Message, error) {
	row := p.pool.QueryRow(ctx, `
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
		          next_retry_at, expires_at, terminal_reason, created_at, delivered_at
	`, messageID, agentID, domain.MessagePending, domain.MessageExpired, domain.MessageDead, defaultMessageRetryDelay, trimMessageReason(reason), maxMessageTerminalReason)
	return scanMessage(row)
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
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				return false, nil
			}
			return false, mapPgErr(err)
		}
		if notification.Channel == agentWorkNotifyChannel && notification.Payload == agentID {
			return true, nil
		}
	}
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
	if err := row.Scan(
		&a.ID,
		&a.SquadID,
		&a.Name,
		&a.Role,
		&a.IdentityID,
		&a.DefaultProvider,
		&a.DefaultModel,
		&a.Permissions,
		&a.IdleTimeoutSec,
		&a.Status,
		&a.CreatedAt,
		&a.UpdatedAt,
	); err != nil {
		return nil, mapPgErr(err)
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

func scanLLMProvider(row scanner) (*domain.LLMProvider, error) {
	var p domain.LLMProvider
	if err := row.Scan(
		&p.ID,
		&p.Name,
		&p.Kind,
		&p.BaseURL,
		&p.APIKeyRef,
		&p.DefaultModel,
		&p.Models,
		&p.Pricing,
		&p.Status,
		&p.RegisteredBy,
		&p.CreatedAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &p, nil
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
	); err != nil {
		return nil, mapPgErr(err)
	}
	return &t, nil
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
		&msg.TerminalReason,
		&msg.CreatedAt,
		&deliveredAt,
	); err != nil {
		return nil, mapPgErr(err)
	}
	if deliveredAt.Valid {
		msg.DeliveredAt = deliveredAt.Time
	}
	return &msg, nil
}

func scanMessages(rows pgx.Rows) ([]*domain.Message, error) {
	var messages []*domain.Message
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
