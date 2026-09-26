package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func TestAdminCanPromoteAndDemoteUser(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer boss": {
			Issuer: testIssuer, Subject: "subject-boss", Email: "boss@example.com",
			EmailVerified: true, Name: "Boss", Groups: []string{platformAdminGroup},
		},
		"Bearer alice": {
			Issuer: testIssuer, Subject: "subject-alice", Email: "alice@example.com",
			EmailVerified: true, Name: "Alice", Groups: []string{"everyone"},
		},
	})

	// Bootstrap: boss is admin via first-login group claim; alice is plain.
	var boss domain.User
	doJSONAuth(t, handler, "Bearer boss", http.MethodGet, pathAuthMe, nil, http.StatusOK, &boss)
	require.Equal(t, domain.RolePlatformAdmin, boss.Role)

	var aliceBootstrap domain.User
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, pathAuthMe, nil, http.StatusOK, &aliceBootstrap)
	require.Equal(t, domain.RoleUser, aliceBootstrap.Role)

	var users []map[string]any
	doJSONAuth(t, handler, "Bearer boss", http.MethodGet, "/api/v1/users", nil, http.StatusOK, &users)
	aliceID := ""
	for _, u := range users {
		if u["email"] == "alice@example.com" {
			aliceID = u["id"].(string)
		}
	}
	require.NotEmpty(t, aliceID, "alice provisioned by first login")

	// Promote alice.
	var promoted map[string]any
	doJSONAuth(t, handler, "Bearer boss", http.MethodPatch, pathUsersPrefix+aliceID+"/role",
		map[string]string{"role": "platform_admin"}, http.StatusOK, &promoted)
	require.Equal(t, "platform_admin", promoted["role"])

	var aliceMe domain.User
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, pathAuthMe, nil, http.StatusOK, &aliceMe)
	require.Equal(t, domain.RolePlatformAdmin, aliceMe.Role)

	// Demote alice (two admins exist, so this is allowed).
	var demoted map[string]any
	doJSONAuth(t, handler, "Bearer boss", http.MethodPatch, pathUsersPrefix+aliceID+"/role",
		map[string]string{"role": "user"}, http.StatusOK, &demoted)
	require.Equal(t, "user", demoted["role"])

	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, pathAuthMe, nil, http.StatusOK, &aliceMe)
	require.Equal(t, domain.RoleUser, aliceMe.Role)

	// Both changes must be audited.
	var audit []domain.AuditEntry
	doJSONAuth(t, handler, "Bearer boss", http.MethodGet, "/api/v1/audit?limit=100", nil, http.StatusOK, &audit)
	roleChanges := 0
	for _, e := range audit {
		if e.Action == "user.role_changed" && e.ResourceID == aliceID {
			roleChanges++
		}
	}
	require.Equal(t, 2, roleChanges, "promote + demote both audited")
}

func TestSetUserRoleRequiresPlatformAdmin(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), headerOIDC{
		"Bearer boss": {
			Issuer: testIssuer, Subject: "subject-boss", Email: "boss@example.com",
			EmailVerified: true, Name: "Boss", Groups: []string{platformAdminGroup},
		},
		"Bearer alice": {
			Issuer: testIssuer, Subject: "subject-alice", Email: "alice@example.com",
			EmailVerified: true, Name: "Alice", Groups: []string{"everyone"},
		},
	})

	var alice domain.User
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, pathAuthMe, nil, http.StatusOK, &alice)

	var body map[string]map[string]string
	doJSONAuth(t, handler, "Bearer alice", http.MethodPatch, pathUsersPrefix+alice.ID+"/role",
		map[string]string{"role": "platform_admin"}, http.StatusForbidden, &body)
	require.Equal(t, "forbidden", body["error"]["code"])
}

func TestSetUserRoleRejectsUnknownRole(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer boss": {
			Issuer: testIssuer, Subject: "subject-boss", Email: "boss@example.com",
			EmailVerified: true, Name: "Boss", Groups: []string{platformAdminGroup},
		},
		"Bearer alice": {
			Issuer: testIssuer, Subject: "subject-alice", Email: "alice@example.com",
			EmailVerified: true, Name: "Alice", Groups: []string{"everyone"},
		},
	})

	var alice domain.User
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, pathAuthMe, nil, http.StatusOK, &alice)

	var body map[string]map[string]string
	doJSONAuth(t, handler, "Bearer boss", http.MethodPatch, pathUsersPrefix+alice.ID+"/role",
		map[string]string{"role": "superuser"}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])

	// Unknown user => 404.
	doJSONAuth(t, handler, "Bearer boss", http.MethodPatch, pathUsersPrefix+"00000000-0000-0000-0000-000000000000/role",
		map[string]string{"role": "user"}, http.StatusNotFound, &body)
}

func TestLastPlatformAdminCannotBeDemoted(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer boss": {
			Issuer: testIssuer, Subject: "subject-boss", Email: "boss@example.com",
			EmailVerified: true, Name: "Boss", Groups: []string{platformAdminGroup},
		},
		"Bearer alice": {
			Issuer: testIssuer, Subject: "subject-alice", Email: "alice@example.com",
			EmailVerified: true, Name: "Alice", Groups: []string{"everyone"},
		},
	})

	var boss domain.User
	doJSONAuth(t, handler, "Bearer boss", http.MethodGet, pathAuthMe, nil, http.StatusOK, &boss)

	// Boss is the only admin; self-demotion must be refused.
	var body map[string]map[string]string
	doJSONAuth(t, handler, "Bearer boss", http.MethodPatch, pathUsersPrefix+boss.ID+"/role",
		map[string]string{"role": "user"}, http.StatusConflict, &body)
	require.Equal(t, "last_admin", body["error"]["code"])
}

// The core regression this design hinges on: an admin demoted in-app stays
// demoted even though they are still a member of the bound IdP group.
func TestDemotedAdminStaysDemotedDespiteGroupMembership(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer boss": {
			Issuer: testIssuer, Subject: "subject-boss", Email: "boss@example.com",
			EmailVerified: true, Name: "Boss", Groups: []string{platformAdminGroup},
		},
		"Bearer exadmin": {
			Issuer: testIssuer, Subject: "subject-exadmin", Email: "exadmin@example.com",
			EmailVerified: true, Name: "Ex Admin", Groups: []string{platformAdminGroup},
		},
	})

	// exadmin bootstrapped as admin via first-login group claim.
	var ex domain.User
	doJSONAuth(t, handler, "Bearer exadmin", http.MethodGet, pathAuthMe, nil, http.StatusOK, &ex)
	require.Equal(t, domain.RolePlatformAdmin, ex.Role)

	// Boss demotes exadmin in-app.
	var demoteOut map[string]any
	doJSONAuth(t, handler, "Bearer boss", http.MethodPatch, pathUsersPrefix+ex.ID+"/role",
		map[string]string{"role": "user"}, http.StatusOK, &demoteOut)

	// exadmin logs in again — still in the admin group — but stays plain.
	var exAgain domain.User
	doJSONAuth(t, handler, "Bearer exadmin", http.MethodGet, pathAuthMe, nil, http.StatusOK, &exAgain)
	require.Equal(t, ex.ID, exAgain.ID)
	require.Equal(t, domain.RoleUser, exAgain.Role, "in-app demotion must stick across logins")
}
