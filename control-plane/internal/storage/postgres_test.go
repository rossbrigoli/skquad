package storage

import (
	"regexp"
	"testing"
)

func TestReadMigrationFilesSortsAndChecksumsEmbeddedSQL(t *testing.T) {
	t.Parallel()

	migrations, err := readMigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) < 2 {
		t.Fatalf("migration count = %d, want at least 2", len(migrations))
	}
	if got, want := migrations[0].Version, "0001_init.sql"; got != want {
		t.Fatalf("first migration = %q, want %q", got, want)
	}
	if got, want := migrations[1].Version, "0002_schema_migrations.sql"; got != want {
		t.Fatalf("second migration = %q, want %q", got, want)
	}
	checksumPattern := regexp.MustCompile(`^[a-f0-9]{64}$`)
	for _, migration := range migrations {
		if migration.SQL == "" {
			t.Fatalf("migration %s has empty SQL", migration.Version)
		}
		if !checksumPattern.MatchString(migration.Checksum) {
			t.Fatalf("migration %s checksum = %q, want sha256 hex", migration.Version, migration.Checksum)
		}
	}
}

func TestMigrationChecksumChangesWithContent(t *testing.T) {
	t.Parallel()

	first := migrationChecksum([]byte("select 1;"))
	second := migrationChecksum([]byte("select 2;"))
	if first == second {
		t.Fatalf("checksums matched for different SQL: %s", first)
	}
}

func TestAgentWorkNotifyMigrationDefinesTriggers(t *testing.T) {
	t.Parallel()

	migrations, err := readMigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range migrations {
		if migration.Version == "0004_agent_work_notify.sql" {
			sql = migration.SQL
			break
		}
	}
	if sql == "" {
		t.Fatal("0004_agent_work_notify.sql migration not found")
	}
	for _, want := range []string{
		"pg_notify('skquad_agent_work'",
		"trg_tasks_agent_work_notify",
		"trg_messages_agent_work_notify",
		"AFTER INSERT OR UPDATE OF assignee_agent_id, status ON tasks",
		"AFTER INSERT OR UPDATE OF status, next_retry_at ON messages",
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(sql) {
			t.Fatalf("migration missing %q", want)
		}
	}
}

func TestAgentStorageMigrationDefinesColumns(t *testing.T) {
	t.Parallel()

	migrations, err := readMigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range migrations {
		if migration.Version == "0015_agent_storage.sql" {
			sql = migration.SQL
			break
		}
	}
	if sql == "" {
		t.Fatal("0015_agent_storage.sql migration not found")
	}
	for _, want := range []string{
		"ALTER TABLE agents ADD COLUMN IF NOT EXISTS storage_enabled boolean NOT NULL DEFAULT false",
		"ALTER TABLE agents ADD COLUMN IF NOT EXISTS storage_size    text    NOT NULL DEFAULT '2Gi'",
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(sql) {
			t.Fatalf("0015 migration missing %q", want)
		}
	}
}

func TestBrowserActivationMigrationSeedsResourceAndGrants(t *testing.T) {
	t.Parallel()

	migrations, err := readMigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range migrations {
		if migration.Version == "0060_browser_activation.sql" {
			sql = migration.SQL
			break
		}
	}
	if sql == "" {
		t.Fatal("0060_browser_activation.sql migration not found")
	}
	for _, want := range []string{
		// Fixed seed uuid + mcp type + browser driver selection.
		"'736b7175-6164-4000-8000-0000000000b0'",
		`"driver": "browser"`,
		`"auth_kind": "bearer"`,
		// Browser-driver mcp resources must be internal-class.
		"'internal'",
		// Managed custody secret ref (key layout: bearer "token").
		"k8s://skquad-system/skquad-mcp-736b7175-6164-4000-8000-0000000000b0",
		// Flat browser ceiling materialized with protocol defaults.
		`"max_pages": 50`,
		`"max_screenshot_bytes": 2097152`,
		`"idle_timeout_s": 600`,
		`"max_session_minutes": 30`,
		`"max_sessions_per_agent": 1`,
		`"deny_hosts": ["*.internal", "metadata.google.internal", "*.metadata.goog", "169.254.169.254"]`,
		// Grants: every agent gets the resource via agent_permissions
		// (the gateway grant table), idempotent.
		"INSERT INTO agent_permissions (agent_id, resource_type, resource_id, granted_by, constraints)",
		"FROM agents a",
		"ON CONFLICT (agent_id, resource_type, resource_id) DO NOTHING",
		"ON CONFLICT (type, name) DO NOTHING",
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(sql) {
			t.Fatalf("0060 migration missing %q", want)
		}
	}
	if regexp.MustCompile(regexp.QuoteMeta("INSERT INTO standing_grants")).MatchString(sql) {
		t.Fatal("0060 must not seed standing_grants (TG-8 confirmation table, not the grant path)")
	}
}
