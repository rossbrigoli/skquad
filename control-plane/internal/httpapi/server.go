// Package httpapi exposes the skquad control-plane REST API.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/auth"
	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	maxAgentMemoryContentChars = 4000
	defaultTaskExecutionLease  = 2 * time.Minute
)

var errNoGatewayModels = errors.New("no active LLM provider models granted")

// Store is the persistence surface required by the current API slice.
type Store interface {
	storage.UserStore
	storage.SquadStore
	storage.AgentStore
	storage.BoardStore
	storage.GrantStore
	storage.RegistryStore
	storage.PermissionStore
	storage.MeteringStore
	storage.AuditStore
	storage.TaskStore
	storage.AgentMemoryStore
	storage.MessageStore
	storage.WorkNotificationStore
}

// Server owns HTTP routing and request-scoped dependencies.
type Server struct {
	cfg        *config.Config
	store      Store
	oidcAuth   OIDCAuthenticator
	crWriter   CRWriter
	llmGateway LLMGatewayProvisioner
}

// OIDCAuthenticator authenticates OIDC Authorization headers.
type OIDCAuthenticator interface {
	Authenticate(ctx context.Context, authorization string) (*auth.Profile, error)
}

// CRWriter mirrors persisted squad/agent state into Kubernetes custom
// resources for the operator.
type CRWriter interface {
	UpsertSquad(ctx context.Context, squad *domain.Squad) error
	DeleteSquad(ctx context.Context, squad *domain.Squad) error
	UpsertAgent(ctx context.Context, agent *domain.Agent, identity *domain.AgentIdentity) error
	DeleteAgent(ctx context.Context, agent *domain.Agent) error
	WriteAgentCredential(ctx context.Context, credentialRef string, agentID string, token string) error
	DeleteAgentCredential(ctx context.Context, credentialRef string) error
}

// LLMGatewayProvisioner issues agent-scoped LiteLLM virtual keys.
type LLMGatewayProvisioner interface {
	ProvisionAgentKey(ctx context.Context, req GatewayKeyRequest) (string, error)
}

// GatewayKeyRequest describes the access a new runtime virtual key should have.
type GatewayKeyRequest struct {
	AgentID string
	SquadID string
	Models  []string
}

// New returns an HTTP handler for the control-plane API.
func New(cfg *config.Config, store Store) http.Handler {
	return NewWithOIDCAuthenticator(cfg, store, nil)
}

// NewWithCRWriter returns an HTTP handler that mirrors squad/agent mutations
// to Kubernetes CRs.
func NewWithCRWriter(cfg *config.Config, store Store, crWriter CRWriter) http.Handler {
	return newServer(cfg, store, nil, crWriter)
}

// NewWithDependencies returns an HTTP handler with explicit optional
// integrations for tests and production startup.
func NewWithDependencies(cfg *config.Config, store Store, oidcAuth OIDCAuthenticator, crWriter CRWriter) http.Handler {
	return newServer(cfg, store, oidcAuth, crWriter)
}

// NewWithOIDCAuthenticator returns an HTTP handler using oidcAuth when
// SKQUAD_AUTH_MODE=oidc.
func NewWithOIDCAuthenticator(cfg *config.Config, store Store, oidcAuth OIDCAuthenticator) http.Handler {
	return newServer(cfg, store, oidcAuth, nil)
}

func newServer(cfg *config.Config, store Store, oidcAuth OIDCAuthenticator, crWriter CRWriter) http.Handler {
	if crWriter == nil {
		crWriter = noopCRWriter{}
	}
	llmGateway := LLMGatewayProvisioner(noopLLMGateway{})
	if cfg != nil && cfg.LiteLLMAdminURL != "" && cfg.LiteLLMMasterKey != "" {
		llmGateway = newLiteLLMGatewayClient(cfg.LiteLLMAdminURL, cfg.LiteLLMMasterKey)
	}
	s := &Server{cfg: cfg, store: store, oidcAuth: oidcAuth, crWriter: crWriter, llmGateway: llmGateway}

	r := chi.NewRouter()
	r.Get("/healthz", s.health)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/gateway/metering", s.ingestGatewayMetering)

		r.Route("/agents/me", func(r chi.Router) {
			r.Use(s.authenticateAgent)

			r.Get("/tasks", s.listCurrentAgentTasks)
			r.Get("/resources", s.listCurrentAgentResources)
			r.Get("/messages", s.listCurrentAgentMessages)
			r.Get("/messages/history", s.listCurrentAgentMessageHistory)
			r.Post("/messages", s.createCurrentAgentMessage)
			r.Post("/messages/{messageID}/ack", s.ackCurrentAgentMessage)
			r.Post("/messages/{messageID}/fail", s.failCurrentAgentMessage)
			r.Get("/work/wait", s.waitCurrentAgentWork)
			r.Post("/tasks/claim", s.claimCurrentAgentTask)
			r.Get("/tasks/{taskID}/context", s.getCurrentAgentTaskContext)
			r.Post("/tasks/{taskID}/start", s.startCurrentAgentTask)
			r.Post("/tasks/{taskID}/complete", s.completeCurrentAgentTask)
			r.Post("/tasks/{taskID}/block", s.blockCurrentAgentTask)
			r.Post("/heartbeat", s.currentAgentHeartbeat)
		})

		r.Group(func(r chi.Router) {
			r.Use(s.authenticate)

			r.Get("/auth/me", s.me)

			r.Post("/squads", s.createSquad)
			r.Get("/squads", s.listSquads)
			r.Get("/squads/{squadID}", s.getSquad)
			r.Patch("/squads/{squadID}", s.updateSquad)
			r.Delete("/squads/{squadID}", s.deleteSquad)
			r.Post("/squads/{squadID}/access-grants", s.createGrant)
			r.Get("/squads/{squadID}/access-grants", s.listGrants)
			r.Delete("/access-grants/{grantID}", s.deleteGrant)

			r.Post("/squads/{squadID}/agents", s.createAgent)
			r.Get("/squads/{squadID}/agents", s.listAgents)
			r.Get("/agents/{agentID}", s.getAgent)
			r.Patch("/agents/{agentID}", s.updateAgent)
			r.Delete("/agents/{agentID}", s.deleteAgent)
			r.Post("/agents/{agentID}/chat", s.createAgentChatMessage)
			r.Get("/agents/{agentID}/chat", s.listAgentChatMessages)
			r.Post("/agents/{agentID}/identity", s.createAgentIdentity)
			r.Post("/agents/{agentID}/identity/rotate", s.rotateAgentIdentity)
			r.Get("/agents/{agentID}/permissions", s.listAgentPermissions)
			r.Put("/agents/{agentID}/permissions", s.setAgentPermissions)

			r.Get("/squads/{squadID}/board", s.getBoard)
			r.Get("/squads/{squadID}/metering", s.getSquadMetering)
			r.Get("/squads/{squadID}/audit", s.listSquadAudit)
			r.Post("/squads/{squadID}/board/tasks", s.createTask)
			r.Get("/tasks/{taskID}", s.getTask)
			r.Patch("/tasks/{taskID}", s.updateTask)
			r.Post("/tasks/{taskID}/move", s.moveTask)
			r.Delete("/tasks/{taskID}", s.deleteTask)
			r.Get("/agents/{agentID}/metering", s.getAgentMetering)

			r.Post("/registry/llm-providers", s.createLLMProvider)
			r.Get("/registry/llm-providers", s.listLLMProviders)
			r.Get("/registry/llm-providers/{providerID}", s.getLLMProvider)
			r.Patch("/registry/llm-providers/{providerID}", s.updateLLMProvider)
			r.Post("/registry/llm-providers/{providerID}/deprecate", s.deprecateLLMProvider)

			r.Post("/registry/{registryType}", s.createRegistryResource)
			r.Get("/registry/{registryType}", s.listRegistryResources)
			r.Get("/registry/{registryType}/{resourceID}", s.getRegistryResource)
			r.Patch("/registry/{registryType}/{resourceID}", s.updateRegistryResource)
			r.Post("/registry/{registryType}/{resourceID}/deprecate", s.deprecateRegistryResource)

			r.Get("/metering/summary", s.getMeteringSummary)
			r.Get("/audit", s.listAudit)
		})
	})

	return r
}

type noopCRWriter struct{}

func (noopCRWriter) UpsertSquad(context.Context, *domain.Squad) error { return nil }
func (noopCRWriter) DeleteSquad(context.Context, *domain.Squad) error { return nil }
func (noopCRWriter) UpsertAgent(context.Context, *domain.Agent, *domain.AgentIdentity) error {
	return nil
}
func (noopCRWriter) DeleteAgent(context.Context, *domain.Agent) error { return nil }
func (noopCRWriter) WriteAgentCredential(context.Context, string, string, string) error {
	return nil
}
func (noopCRWriter) DeleteAgentCredential(context.Context, string) error { return nil }

type noopLLMGateway struct{}

func (noopLLMGateway) ProvisionAgentKey(context.Context, GatewayKeyRequest) (string, error) {
	return generateCredential()
}

type principalKey struct{}
type agentPrincipalKey struct{}

type agentPrincipal struct {
	Agent    *domain.Agent
	Identity *domain.AgentIdentity
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch s.cfg.AuthMode {
		case config.AuthDev:
			u := &domain.User{
				Email: s.cfg.DevEmail,
				Name:  s.cfg.DevName,
				Role:  domain.RolePlatformAdmin,
			}
			user, err := s.store.UpsertUser(r.Context(), u)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to load dev principal")
				return
			}
			if err := s.store.SetUserRole(r.Context(), user.ID, domain.RolePlatformAdmin); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to promote dev principal")
				return
			}
			user.Role = domain.RolePlatformAdmin
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, user)))
		case config.AuthOIDC:
			if s.oidcAuth == nil {
				writeError(w, http.StatusInternalServerError, "internal", "OIDC authentication is not configured")
				return
			}
			profile, err := s.oidcAuth.Authenticate(r.Context(), r.Header.Get("Authorization"))
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
				return
			}
			user, err := s.store.UpsertUser(r.Context(), &domain.User{
				OIDCIssuer:    profile.Issuer,
				OIDCSubject:   profile.Subject,
				Email:         profile.Email,
				EmailVerified: profile.EmailVerified,
				Name:          profile.Name,
				Role:          domain.RoleUser,
			})
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to load authenticated principal")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, user)))
		default:
			writeError(w, http.StatusInternalServerError, "internal", "unsupported auth mode")
		}
	})
}

func currentUser(ctx context.Context) *domain.User {
	u, _ := ctx.Value(principalKey{}).(*domain.User)
	return u
}

func currentAgent(ctx context.Context) *agentPrincipal {
	p, _ := ctx.Value(agentPrincipalKey{}).(*agentPrincipal)
	return p
}

func (s *Server) authenticateAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimSpace(r.Header.Get("X-Skquad-Agent-ID"))
		if agentID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing agent id")
			return
		}
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		agent, err := s.store.GetAgent(r.Context(), agentID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		identity, err := s.store.GetAgentIdentity(r.Context(), agent.ID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if !matchesAgentCredential(token, identity) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid agent credential")
			return
		}
		principal := &agentPrincipal{Agent: agent, Identity: identity}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentPrincipalKey{}, principal)))
	})
}

func (s *Server) requirePlatformAdmin(w http.ResponseWriter, r *http.Request) bool {
	if currentUser(r.Context()).Role != domain.RolePlatformAdmin {
		writeError(w, http.StatusForbidden, "forbidden", "platform admin role is required")
		return false
	}
	return true
}

func (s *Server) requireGatewayCallback(w http.ResponseWriter, r *http.Request) bool {
	expected := ""
	if s.cfg != nil {
		expected = strings.TrimSpace(s.cfg.GatewayCallbackToken)
	}
	if expected == "" {
		writeError(w, http.StatusNotFound, "not_found", "gateway callback endpoint is not configured")
		return false
	}
	actual := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if actual == "" {
		actual = strings.TrimSpace(r.Header.Get("X-Skquad-Callback-Token"))
	}
	if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "gateway callback token is invalid")
		return false
	}
	return true
}

func registryTypeFromRequest(w http.ResponseWriter, r *http.Request) (domain.ResourceType, bool) {
	switch chi.URLParam(r, "registryType") {
	case "skills":
		return domain.ResSkill, true
	case "tools":
		return domain.ResTool, true
	case "apis":
		return domain.ResAPI, true
	case "knowledge-bases":
		return domain.ResKnowledgeBase, true
	case "project-workspaces":
		return domain.ResProjectWorkspace, true
	default:
		writeError(w, http.StatusNotFound, "not_found", "registry resource type not found")
		return "", false
	}
}

func resourceTypeFromString(value string) (domain.ResourceType, bool) {
	switch domain.ResourceType(value) {
	case domain.ResLLMProvider, domain.ResSkill, domain.ResTool, domain.ResAPI, domain.ResKnowledgeBase, domain.ResProjectWorkspace:
		return domain.ResourceType(value), true
	default:
		return "", false
	}
}

func validateName(w http.ResponseWriter, name string) bool {
	return validateRequired(w, "name", name)
}

func validateRequired(w http.ResponseWriter, field, value string) bool {
	if strings.TrimSpace(value) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", field+" is required")
		return false
	}
	return true
}

func defaultRawJSON(value json.RawMessage, fallback string) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(fallback)
	}
	return value
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r.Context()))
}

func (s *Server) createLLMProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req struct {
		Name         string          `json:"name"`
		Kind         string          `json:"kind"`
		BaseURL      string          `json:"base_url"`
		APIKeyRef    string          `json:"api_key_ref"`
		DefaultModel string          `json:"default_model"`
		Models       json.RawMessage `json:"models"`
		Pricing      json.RawMessage `json:"pricing"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) || !validateRequired(w, "kind", req.Kind) || !validateRequired(w, "base_url", req.BaseURL) {
		return
	}
	if len(req.Models) == 0 {
		req.Models = json.RawMessage(`[]`)
	}
	if len(req.Pricing) == 0 {
		req.Pricing = json.RawMessage(`{}`)
	}
	u := currentUser(r.Context())
	provider := &domain.LLMProvider{
		Name:         strings.TrimSpace(req.Name),
		Kind:         strings.TrimSpace(req.Kind),
		BaseURL:      strings.TrimSpace(req.BaseURL),
		APIKeyRef:    req.APIKeyRef,
		DefaultModel: strings.TrimSpace(req.DefaultModel),
		Models:       req.Models,
		Pricing:      req.Pricing,
		Status:       domain.ResourceActive,
		RegisteredBy: u.ID,
	}
	created, err := s.store.CreateLLMProvider(r.Context(), provider)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "registry.llm_provider.create", string(domain.ResLLMProvider), created.ID, "", nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listLLMProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListLLMProviders(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, providers)
}

func (s *Server) getLLMProvider(w http.ResponseWriter, r *http.Request) {
	provider, err := s.store.GetLLMProvider(r.Context(), chi.URLParam(r, "providerID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, provider)
}

func (s *Server) updateLLMProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	provider, err := s.store.GetLLMProvider(r.Context(), chi.URLParam(r, "providerID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	var req struct {
		Name         *string          `json:"name"`
		Kind         *string          `json:"kind"`
		BaseURL      *string          `json:"base_url"`
		APIKeyRef    *string          `json:"api_key_ref"`
		DefaultModel *string          `json:"default_model"`
		Models       *json.RawMessage `json:"models"`
		Pricing      *json.RawMessage `json:"pricing"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		if !validateName(w, *req.Name) {
			return
		}
		provider.Name = strings.TrimSpace(*req.Name)
	}
	if req.Kind != nil {
		if !validateRequired(w, "kind", *req.Kind) {
			return
		}
		provider.Kind = strings.TrimSpace(*req.Kind)
	}
	if req.BaseURL != nil {
		if !validateRequired(w, "base_url", *req.BaseURL) {
			return
		}
		provider.BaseURL = strings.TrimSpace(*req.BaseURL)
	}
	if req.APIKeyRef != nil {
		provider.APIKeyRef = *req.APIKeyRef
	}
	if req.DefaultModel != nil {
		provider.DefaultModel = strings.TrimSpace(*req.DefaultModel)
	}
	if req.Models != nil {
		provider.Models = *req.Models
	}
	if req.Pricing != nil {
		provider.Pricing = *req.Pricing
	}
	updated, err := s.store.UpdateLLMProvider(r.Context(), provider)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "registry.llm_provider.update", string(domain.ResLLMProvider), updated.ID, "", nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deprecateLLMProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	if err := s.store.DeprecateLLMProvider(r.Context(), chi.URLParam(r, "providerID")); err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "registry.llm_provider.deprecate", string(domain.ResLLMProvider), chi.URLParam(r, "providerID"), "", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createRegistryResource(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	typ, ok := registryTypeFromRequest(w, r)
	if !ok {
		return
	}
	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Endpoint    string          `json:"endpoint"`
		AuthRef     string          `json:"auth_ref"`
		Manifest    json.RawMessage `json:"manifest"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	if len(req.Manifest) == 0 {
		req.Manifest = json.RawMessage(`{}`)
	}
	u := currentUser(r.Context())
	resource := &domain.RegistryResource{
		Type:         typ,
		Name:         strings.TrimSpace(req.Name),
		Description:  req.Description,
		Endpoint:     req.Endpoint,
		AuthRef:      req.AuthRef,
		Manifest:     req.Manifest,
		Status:       domain.ResourceActive,
		RegisteredBy: u.ID,
	}
	created, err := s.store.CreateResource(r.Context(), resource)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "registry.resource.create", string(typ), created.ID, "", nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listRegistryResources(w http.ResponseWriter, r *http.Request) {
	typ, ok := registryTypeFromRequest(w, r)
	if !ok {
		return
	}
	resources, err := s.store.ListResources(r.Context(), typ)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resources)
}

func (s *Server) getRegistryResource(w http.ResponseWriter, r *http.Request) {
	typ, ok := registryTypeFromRequest(w, r)
	if !ok {
		return
	}
	resource, err := s.store.GetResource(r.Context(), typ, chi.URLParam(r, "resourceID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resource)
}

func (s *Server) updateRegistryResource(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	typ, ok := registryTypeFromRequest(w, r)
	if !ok {
		return
	}
	resource, err := s.store.GetResource(r.Context(), typ, chi.URLParam(r, "resourceID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	var req struct {
		Name        *string          `json:"name"`
		Description *string          `json:"description"`
		Endpoint    *string          `json:"endpoint"`
		AuthRef     *string          `json:"auth_ref"`
		Manifest    *json.RawMessage `json:"manifest"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		if !validateName(w, *req.Name) {
			return
		}
		resource.Name = strings.TrimSpace(*req.Name)
	}
	if req.Description != nil {
		resource.Description = *req.Description
	}
	if req.Endpoint != nil {
		resource.Endpoint = *req.Endpoint
	}
	if req.AuthRef != nil {
		resource.AuthRef = *req.AuthRef
	}
	if req.Manifest != nil {
		resource.Manifest = *req.Manifest
	}
	updated, err := s.store.UpdateResource(r.Context(), resource)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "registry.resource.update", string(typ), updated.ID, "", nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deprecateRegistryResource(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	typ, ok := registryTypeFromRequest(w, r)
	if !ok {
		return
	}
	if err := s.store.DeprecateResource(r.Context(), typ, chi.URLParam(r, "resourceID")); err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "registry.resource.deprecate", string(typ), chi.URLParam(r, "resourceID"), "", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createSquad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string          `json:"name"`
		Mission        string          `json:"mission"`
		OperatingModel json.RawMessage `json:"operating_model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if len(req.OperatingModel) == 0 {
		req.OperatingModel = json.RawMessage(`{}`)
	}

	u := currentUser(r.Context())
	squad := &domain.Squad{
		Name:           req.Name,
		Mission:        req.Mission,
		OperatingModel: req.OperatingModel,
		OwnerID:        u.ID,
		Namespace:      namespaceFor(req.Name),
		Status:         domain.SquadActive,
	}
	created, err := s.store.CreateSquad(r.Context(), squad)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "squad.create", "squad", created.ID, created.ID, nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listSquads(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	ownerID := u.ID
	if u.Role == domain.RolePlatformAdmin && r.URL.Query().Get("all") == "true" {
		ownerID = ""
	}
	squads, err := s.store.ListSquads(r.Context(), ownerID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, squads)
}

func (s *Server) getSquad(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadAccessibleSquad(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, squad)
}

func (s *Server) updateSquad(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}

	var req struct {
		Name           *string          `json:"name"`
		Mission        *string          `json:"mission"`
		OperatingModel *json.RawMessage `json:"operating_model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "name must not be empty")
			return
		}
		squad.Name = name
	}
	if req.Mission != nil {
		squad.Mission = *req.Mission
	}
	if req.OperatingModel != nil {
		squad.OperatingModel = *req.OperatingModel
	}

	updated, err := s.store.UpdateSquad(r.Context(), squad)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "squad.update", "squad", updated.ID, updated.ID, nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteSquad(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSquad(r.Context(), squad.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "squad.delete", "squad", squad.ID, squad.ID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createGrant(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}
	var req struct {
		GranteeType domain.GranteeType `json:"grantee_type"`
		GranteeID   string             `json:"grantee_id"`
		Permissions string             `json:"permissions"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.GranteeType != domain.GranteeUser && req.GranteeType != domain.GranteeAgent {
		writeError(w, http.StatusBadRequest, "bad_request", "grantee_type is invalid")
		return
	}
	req.GranteeID = strings.TrimSpace(req.GranteeID)
	if req.GranteeID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "grantee_id is required")
		return
	}
	if req.Permissions == "" {
		req.Permissions = "talk"
	}
	if req.GranteeType == domain.GranteeUser {
		if _, err := s.store.GetUser(r.Context(), req.GranteeID); err != nil {
			writeStorageError(w, err)
			return
		}
	} else if _, err := s.store.GetAgent(r.Context(), req.GranteeID); err != nil {
		writeStorageError(w, err)
		return
	}

	u := currentUser(r.Context())
	grant := &domain.AccessGrant{
		SquadID:     squad.ID,
		GranteeType: req.GranteeType,
		GranteeID:   req.GranteeID,
		Permissions: req.Permissions,
		GrantedBy:   u.ID,
	}
	if err := s.recordUserAuditRequired(r, "access_grant.create", "access_grant", req.GranteeID, squad.ID, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to audit access grant creation")
		return
	}
	created, err := s.store.CreateGrant(r.Context(), grant)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listGrants(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}
	grants, err := s.store.ListGrants(r.Context(), squad.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) deleteGrant(w http.ResponseWriter, r *http.Request) {
	grant, err := s.store.GetGrant(r.Context(), chi.URLParam(r, "grantID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if _, ok := s.ensureSquadAccess(w, r, grant.SquadID, true); !ok {
		return
	}
	if err := s.recordUserAuditRequired(r, "access_grant.delete", "access_grant", grant.ID, grant.SquadID, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to audit access grant deletion")
		return
	}
	if err := s.store.RevokeGrant(r.Context(), grant.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}
	var req struct {
		Name              string          `json:"name"`
		Role              string          `json:"role"`
		SystemPrompt      string          `json:"system_prompt"`
		DefaultProviderID string          `json:"default_provider_id"`
		DefaultModel      string          `json:"default_model"`
		Permissions       json.RawMessage `json:"permissions"`
		IdleTimeoutSec    int             `json:"idle_timeout_sec"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if len(req.Permissions) == 0 {
		req.Permissions = json.RawMessage(`[]`)
	}
	if req.IdleTimeoutSec <= 0 {
		req.IdleTimeoutSec = int(s.cfg.DefaultIdleTimeout / time.Second)
	}

	agent := &domain.Agent{
		SquadID:         squad.ID,
		Name:            req.Name,
		Role:            req.Role,
		SystemPrompt:    strings.TrimSpace(req.SystemPrompt),
		DefaultProvider: strings.TrimSpace(req.DefaultProviderID),
		DefaultModel:    strings.TrimSpace(req.DefaultModel),
		Permissions:     req.Permissions,
		IdleTimeoutSec:  req.IdleTimeoutSec,
		Status:          domain.AgentIdle,
	}
	created, err := s.store.CreateAgent(r.Context(), agent)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "agent.create", "agent", created.ID, squad.ID, nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadAccessibleSquad(w, r)
	if !ok {
		return
	}
	agents, err := s.store.ListAgents(r.Context(), squad.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadAccessibleAgent(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadOwnedAgent(w, r)
	if !ok {
		return
	}
	var req struct {
		Name              *string          `json:"name"`
		Role              *string          `json:"role"`
		SystemPrompt      *string          `json:"system_prompt"`
		DefaultProviderID *string          `json:"default_provider_id"`
		DefaultModel      *string          `json:"default_model"`
		Permissions       *json.RawMessage `json:"permissions"`
		IdleTimeoutSec    *int             `json:"idle_timeout_sec"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "name must not be empty")
			return
		}
		agent.Name = name
	}
	if req.Role != nil {
		agent.Role = *req.Role
	}
	if req.SystemPrompt != nil {
		agent.SystemPrompt = strings.TrimSpace(*req.SystemPrompt)
	}
	if req.DefaultProviderID != nil {
		agent.DefaultProvider = strings.TrimSpace(*req.DefaultProviderID)
	}
	if req.DefaultModel != nil {
		agent.DefaultModel = strings.TrimSpace(*req.DefaultModel)
	}
	if req.Permissions != nil {
		agent.Permissions = *req.Permissions
	}
	if req.IdleTimeoutSec != nil {
		if *req.IdleTimeoutSec <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "idle_timeout_sec must be positive")
			return
		}
		agent.IdleTimeoutSec = *req.IdleTimeoutSec
	}

	updated, err := s.store.UpdateAgent(r.Context(), agent)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "agent.update", "agent", updated.ID, updated.SquadID, nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadOwnedAgent(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteAgent(r.Context(), agent.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "agent.delete", "agent", agent.ID, agent.SquadID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createAgentIdentity(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadOwnedAgent(w, r)
	if !ok {
		return
	}
	squad, err := s.store.GetSquad(r.Context(), agent.SquadID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	u := currentUser(r.Context())
	identity := &domain.AgentIdentity{
		AgentID:        agent.ID,
		CredentialRef:  generatedCredentialRef(squad.Namespace, agent.ID),
		CredentialHash: "",
		VirtualKeyRef:  generatedVirtualKeyRef(squad.Namespace, agent.ID),
		CreatedBy:      u.ID,
	}
	credential, err := generateCredential()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to generate agent credential")
		return
	}
	virtualKey, err := s.provisionAgentVirtualKey(r.Context(), agent)
	if err != nil {
		if errors.Is(err, errNoGatewayModels) {
			writeError(w, http.StatusConflict, "no_llm_models_granted", "agent has no active granted LLM provider models")
			return
		}
		writeError(w, http.StatusBadGateway, "llm_gateway_unavailable", "failed to provision LLM gateway virtual key")
		return
	}
	identity.CredentialHash = hashCredential(credential)
	if err := s.crWriter.WriteAgentCredential(r.Context(), identity.CredentialRef, agent.ID, credential); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to write agent credential secret")
		return
	}
	if err := s.crWriter.WriteAgentCredential(r.Context(), identity.VirtualKeyRef, agent.ID, virtualKey); err != nil {
		_ = s.crWriter.DeleteAgentCredential(r.Context(), identity.CredentialRef)
		writeError(w, http.StatusInternalServerError, "internal", "failed to write agent virtual-key secret")
		return
	}
	created, err := s.store.CreateAgentIdentity(r.Context(), identity)
	if err != nil {
		_ = s.crWriter.DeleteAgentCredential(r.Context(), identity.CredentialRef)
		_ = s.crWriter.DeleteAgentCredential(r.Context(), identity.VirtualKeyRef)
		writeStorageError(w, err)
		return
	}
	s.recordUserAudit(r, "agent_identity.create", "agent_identity", created.ID, agent.SquadID, nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) rotateAgentIdentity(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadOwnedAgent(w, r)
	if !ok {
		return
	}
	squad, err := s.store.GetSquad(r.Context(), agent.SquadID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	existing, err := s.store.GetAgentIdentity(r.Context(), agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	credential, err := generateCredential()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to generate agent credential")
		return
	}
	virtualKey, err := s.provisionAgentVirtualKey(r.Context(), agent)
	if err != nil {
		if errors.Is(err, errNoGatewayModels) {
			writeError(w, http.StatusConflict, "no_llm_models_granted", "agent has no active granted LLM provider models")
			return
		}
		writeError(w, http.StatusBadGateway, "llm_gateway_unavailable", "failed to provision LLM gateway virtual key")
		return
	}
	credentialRef := generatedCredentialRef(squad.Namespace, agent.ID)
	virtualKeyRef := generatedVirtualKeyRef(squad.Namespace, agent.ID)
	if err := s.crWriter.WriteAgentCredential(r.Context(), credentialRef, agent.ID, credential); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to write agent credential secret")
		return
	}
	if err := s.crWriter.WriteAgentCredential(r.Context(), virtualKeyRef, agent.ID, virtualKey); err != nil {
		_ = s.crWriter.DeleteAgentCredential(r.Context(), credentialRef)
		writeError(w, http.StatusInternalServerError, "internal", "failed to write agent virtual-key secret")
		return
	}
	identity, err := s.store.RotateAgentIdentity(r.Context(), agent.ID, credentialRef, hashCredential(credential), virtualKeyRef)
	if err != nil {
		_ = s.crWriter.DeleteAgentCredential(r.Context(), credentialRef)
		_ = s.crWriter.DeleteAgentCredential(r.Context(), virtualKeyRef)
		writeStorageError(w, err)
		return
	}
	_ = s.crWriter.DeleteAgentCredential(r.Context(), existing.CredentialRef)
	_ = s.crWriter.DeleteAgentCredential(r.Context(), existing.VirtualKeyRef)
	s.recordUserAudit(r, "agent_identity.rotate", "agent_identity", identity.ID, agent.SquadID, nil)
	writeJSON(w, http.StatusOK, identity)
}

func (s *Server) provisionAgentVirtualKey(ctx context.Context, agent *domain.Agent) (string, error) {
	models, err := s.allowedGatewayModels(ctx, agent)
	if err != nil {
		return "", err
	}
	if s.cfg != nil && s.cfg.LiteLLMAdminURL != "" && s.cfg.LiteLLMMasterKey != "" && len(models) == 0 {
		return "", fmt.Errorf("%w to agent %s", errNoGatewayModels, agent.ID)
	}
	return s.llmGateway.ProvisionAgentKey(ctx, GatewayKeyRequest{
		AgentID: agent.ID,
		SquadID: agent.SquadID,
		Models:  models,
	})
}

func (s *Server) allowedGatewayModels(ctx context.Context, agent *domain.Agent) ([]string, error) {
	perms, err := s.store.ListAgentPermissions(ctx, agent.ID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var models []string
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			return
		}
		seen[model] = true
		models = append(models, model)
	}
	for _, perm := range perms {
		if perm.ResourceType != domain.ResLLMProvider {
			continue
		}
		provider, err := s.store.GetLLMProvider(ctx, perm.ResourceID)
		if err != nil {
			return nil, err
		}
		if provider.Status != domain.ResourceActive {
			continue
		}
		add(provider.DefaultModel)
		var providerModels []string
		if len(provider.Models) > 0 {
			if err := json.Unmarshal(provider.Models, &providerModels); err == nil {
				for _, model := range providerModels {
					add(model)
				}
			}
		}
	}
	return models, nil
}

func (s *Server) listAgentPermissions(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadOwnedAgent(w, r)
	if !ok {
		return
	}
	perms, err := s.store.ListAgentPermissions(r.Context(), agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, perms)
}

func (s *Server) setAgentPermissions(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadOwnedAgent(w, r)
	if !ok {
		return
	}
	var req []struct {
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	u := currentUser(r.Context())
	perms := make([]domain.AgentPermission, 0, len(req))
	seen := map[string]bool{}
	for _, item := range req {
		typ, ok := resourceTypeFromString(item.ResourceType)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "resource_type is invalid")
			return
		}
		resourceID := strings.TrimSpace(item.ResourceID)
		if resourceID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "resource_id is required")
			return
		}
		if err := s.ensureRegistryResourceExists(r.Context(), typ, resourceID); err != nil {
			writeStorageError(w, err)
			return
		}
		key := string(typ) + ":" + resourceID
		if seen[key] {
			continue
		}
		seen[key] = true
		perms = append(perms, domain.AgentPermission{
			AgentID:      agent.ID,
			ResourceType: typ,
			ResourceID:   resourceID,
			GrantedBy:    u.ID,
		})
	}
	metadata, _ := json.Marshal(map[string]int{"count": len(perms)})
	if err := s.recordUserAuditRequired(r, "agent_permissions.set", "agent", agent.ID, agent.SquadID, metadata); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to audit agent permission update")
		return
	}
	if err := s.store.SetAgentPermissions(r.Context(), agent.ID, perms); err != nil {
		writeStorageError(w, err)
		return
	}
	current, err := s.store.ListAgentPermissions(r.Context(), agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, current)
}

func (s *Server) ensureRegistryResourceExists(ctx context.Context, typ domain.ResourceType, resourceID string) error {
	if typ == domain.ResLLMProvider {
		_, err := s.store.GetLLMProvider(ctx, resourceID)
		return err
	}
	_, err := s.store.GetResource(ctx, typ, resourceID)
	return err
}

func (s *Server) getBoard(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadAccessibleSquad(w, r)
	if !ok {
		return
	}
	board, err := s.store.GetBoard(r.Context(), squad.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	tasks, err := s.store.ListTasks(r.Context(), board.ID, "")
	if err != nil {
		writeStorageError(w, err)
		return
	}
	// Tasks carry no lease columns of their own; the live execution attempt is a
	// separate row. Attach it here so one board request is enough for clients to
	// show what agents are working on right now.
	executions, err := s.store.ListBoardTaskExecutions(r.Context(), board.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	attachExecutionState(tasks, executions)
	writeJSON(w, http.StatusOK, map[string]any{
		"board": board,
		"tasks": tasks,
	})
}

// attachExecutionState stamps each task with its newest active execution
// attempt. Tasks without an attempt keep zero values, which clients read as
// "not in flight".
func attachExecutionState(tasks []*domain.Task, executions []*domain.TaskExecution) {
	newest := make(map[string]*domain.TaskExecution, len(executions))
	for _, exec := range executions {
		if exec == nil {
			continue
		}
		current, ok := newest[exec.TaskID]
		if !ok || exec.StartedAt.After(current.StartedAt) {
			newest[exec.TaskID] = exec
		}
	}
	for _, task := range tasks {
		if task == nil {
			continue
		}
		exec, ok := newest[task.ID]
		if !ok {
			continue
		}
		task.ExecutionID = exec.ID
		task.WorkerID = exec.WorkerID
		task.LeaseExpiresAt = exec.LeaseExpiresAt
		// FencingToken is deliberately not copied: it authorises runtime
		// heartbeat/complete calls, so it belongs to the claiming worker and must
		// not leak to everyone who can read the board.
	}
}

func (s *Server) getSquadMetering(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedOrAdminSquad(w, r)
	if !ok {
		return
	}
	usage, err := s.store.SumMetering(r.Context(), squad.ID, "")
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) getAgentMetering(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadOwnedOrAdminAgent(w, r)
	if !ok {
		return
	}
	usage, err := s.store.SumMetering(r.Context(), "", agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) getMeteringSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	usage, err := s.store.SumMetering(r.Context(), "", "")
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

type gatewayMeteringRequest struct {
	Status       string    `json:"status"`
	AgentID      string    `json:"agent_id"`
	SquadID      string    `json:"squad_id"`
	TaskID       string    `json:"task_id"`
	ProviderID   string    `json:"provider_id"`
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Cost         float64   `json:"cost"`
	Currency     string    `json:"currency"`
	Error        string    `json:"error"`
	Timestamp    time.Time `json:"timestamp"`
}

func (s *Server) ingestGatewayMetering(w http.ResponseWriter, r *http.Request) {
	if !s.requireGatewayCallback(w, r) {
		return
	}
	var req gatewayMeteringRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Status = strings.TrimSpace(req.Status)
	if req.Status == "" {
		req.Status = "success"
	}
	if req.Status != "success" && req.Status != "failure" {
		writeError(w, http.StatusBadRequest, "bad_request", "status must be success or failure")
		return
	}
	if req.AgentID == "" || req.SquadID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent_id and squad_id are required")
		return
	}
	agent, err := s.store.GetAgent(r.Context(), req.AgentID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if agent.SquadID != req.SquadID {
		writeError(w, http.StatusForbidden, "forbidden", "agent does not belong to squad")
		return
	}

	metadata, _ := json.Marshal(map[string]any{
		"model":         req.Model,
		"provider_id":   req.ProviderID,
		"task_id":       req.TaskID,
		"input_tokens":  req.InputTokens,
		"output_tokens": req.OutputTokens,
		"cost":          req.Cost,
		"currency":      defaultMeteringCurrency(req.Currency),
		"error":         trimRunes(req.Error, 512),
	})
	if req.Status == "failure" {
		_ = s.recordSystemAudit(r.Context(), "llm.failure", "agent", req.AgentID, req.SquadID, metadata)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if req.InputTokens < 0 || req.OutputTokens < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "token counts must not be negative")
		return
	}
	if req.Cost < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "cost must not be negative")
		return
	}
	if err := s.store.RecordMetering(r.Context(), &domain.MeteringEvent{
		AgentID:      req.AgentID,
		SquadID:      req.SquadID,
		TaskID:       req.TaskID,
		ProviderID:   req.ProviderID,
		Model:        req.Model,
		InputTokens:  req.InputTokens,
		OutputTokens: req.OutputTokens,
		Cost:         req.Cost,
		Currency:     req.Currency,
		Timestamp:    req.Timestamp,
	}); err != nil {
		writeStorageError(w, err)
		return
	}
	_ = s.recordSystemAudit(r.Context(), "llm.metering.ingest", "agent", req.AgentID, req.SquadID, metadata)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) listSquadAudit(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedOrAdminSquad(w, r)
	if !ok {
		return
	}
	entries, err := s.store.ListAudit(r.Context(), squad.ID, auditLimit(r))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	entries, err := s.store.ListAudit(r.Context(), r.URL.Query().Get("squad_id"), auditLimit(r))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}
	board, err := s.store.GetBoard(r.Context(), squad.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	var req struct {
		Title           string            `json:"title"`
		Description     string            `json:"description"`
		AssigneeAgentID string            `json:"assignee_agent_id"`
		Metadata        map[string]string `json:"metadata"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "title is required")
		return
	}
	if req.AssigneeAgentID != "" {
		agent, err := s.store.GetAgent(r.Context(), req.AssigneeAgentID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if agent.SquadID != squad.ID {
			writeError(w, http.StatusBadRequest, "bad_request", "assignee_agent_id must belong to this squad")
			return
		}
	}
	u := currentUser(r.Context())
	task := &domain.Task{
		BoardID:         board.ID,
		SquadID:         squad.ID,
		Title:           req.Title,
		Description:     req.Description,
		Status:          domain.TaskTodo,
		AssigneeAgentID: req.AssigneeAgentID,
		CreatedByType:   "user",
		CreatedByID:     u.ID,
	}
	created, err := s.store.CreateTask(r.Context(), task)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if created.AssigneeAgentID != "" {
		if err := s.syncAgentStatusFromPendingWork(r.Context(), created.AssigneeAgentID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to update assigned agent state")
			return
		}
	}
	s.recordUserAudit(r, "task.create", "task", created.ID, squad.ID, nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listCurrentAgentTasks(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	tasks, err := s.store.ListAgentTasks(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) listCurrentAgentResources(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	resources, err := s.currentAgentResources(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resources)
}

func (s *Server) currentAgentResources(ctx context.Context, agentID string) ([]agentRuntimeResource, error) {
	perms, err := s.store.ListAgentPermissions(ctx, agentID)
	if err != nil {
		return nil, err
	}
	resources := []agentRuntimeResource{}
	for _, perm := range perms {
		resource, ok, err := s.agentRuntimeResource(ctx, perm)
		if err != nil {
			return nil, err
		}
		if ok {
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

type agentTaskContext struct {
	Task      *domain.Task           `json:"task"`
	Resources []agentRuntimeResource `json:"resources"`
	Memory    []*domain.AgentMemory  `json:"memory"`
	Limits    map[string]int         `json:"limits"`
}

func (s *Server) getCurrentAgentTaskContext(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	task, err := s.store.GetTask(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if task.AssigneeAgentID != principal.Agent.ID {
		writeError(w, http.StatusForbidden, "forbidden", "task is not assigned to this agent")
		return
	}
	resources, err := s.currentAgentResources(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	memoryLimit := boundedIntQuery(r, "memory_limit", 10, 20)
	memories, err := s.store.ListAgentMemory(r.Context(), principal.Agent.ID, task.SquadID, nil, memoryLimit)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentTaskContext{
		Task:      task,
		Resources: resources,
		Memory:    memories,
		Limits: map[string]int{
			"memory_limit":              memoryLimit,
			"memory_content_chars":      maxAgentMemoryContentChars,
			"memory_embeddings_enabled": boolAsInt(s.cfg.MemoryEmbeddingsEnabled),
		},
	})
}

func (s *Server) listCurrentAgentMessages(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	messages, err := s.store.ListPendingMessages(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

// listCurrentAgentMessageHistory returns the full chat history addressed to the
// current agent (all statuses), oldest first. The runtime uses this to build a
// contextual prompt when replying to user chat messages.
func (s *Server) listCurrentAgentMessageHistory(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	messages, err := s.store.ListAgentMessageHistory(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

type agentWorkWaitResponse struct {
	WorkAvailable bool `json:"work_available"`
}

func (s *Server) waitCurrentAgentWork(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	timeout := durationSecondsQuery(r, "timeout_seconds", 25*time.Second, 60*time.Second)
	available, err := s.store.WaitForAgentWork(r.Context(), principal.Agent.ID, timeout)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentWorkWaitResponse{WorkAvailable: available})
}

type messageRequest struct {
	ToAgentID     string             `json:"to_agent_id"`
	ToID          string             `json:"to_id"`
	Type          domain.MessageType `json:"type"`
	Payload       json.RawMessage    `json:"payload"`
	Message       string             `json:"message"`
	CorrelationID string             `json:"correlation_id"`
	MaxAttempts   int                `json:"max_attempts"`
	TTLSeconds    int                `json:"ttl_seconds"`
}

type messageFailureRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) createCurrentAgentMessage(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	var req messageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	targetID := strings.TrimSpace(req.ToAgentID)
	if targetID == "" {
		targetID = strings.TrimSpace(req.ToID)
	}
	if targetID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "to_agent_id is required")
		return
	}
	messageType := req.Type
	if messageType == "" {
		messageType = domain.MessageConsult
	}
	if !messageType.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", "type is invalid")
		return
	}
	target, err := s.store.GetAgent(r.Context(), targetID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if target.SquadID != principal.Agent.SquadID {
		messageAction := requiredMessageAction(messageType)
		ok, err := s.store.AgentMayMessageSquad(r.Context(), principal.Agent.ID, target.SquadID, messageAction)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if !ok {
			s.recordAgentAudit(r, principal.Agent.ID, "message.denied", "agent", target.ID, target.SquadID, nil)
			writeError(w, http.StatusForbidden, "forbidden", "agent cannot message the target squad")
			return
		}
	}
	created, err := s.store.CreateMessage(r.Context(), &domain.Message{
		FromType:      "agent",
		FromID:        principal.Agent.ID,
		ToAgentID:     target.ID,
		SquadID:       target.SquadID,
		Type:          messageType,
		Payload:       messagePayload(req),
		Status:        domain.MessagePending,
		CorrelationID: strings.TrimSpace(req.CorrelationID),
		MaxAttempts:   req.MaxAttempts,
		ExpiresAt:     messageExpiresAt(req),
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), target.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update target agent state")
		return
	}
	s.recordAgentAudit(r, principal.Agent.ID, "message.send", "message", created.ID, target.SquadID, nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) ackCurrentAgentMessage(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	updated, err := s.store.AckMessage(r.Context(), principal.Agent.ID, chi.URLParam(r, "messageID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
		return
	}
	s.recordAgentAudit(r, principal.Agent.ID, "message.ack", "message", updated.ID, updated.SquadID, nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) failCurrentAgentMessage(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	var req messageFailureRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		req.Reason = "runtime message handler failed"
	}
	updated, err := s.store.FailMessage(r.Context(), principal.Agent.ID, chi.URLParam(r, "messageID"), req.Reason)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
		return
	}
	s.recordAgentAudit(r, principal.Agent.ID, "message.fail", "message", updated.ID, updated.SquadID, nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) createAgentChatMessage(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadAgentForAction(w, r, "talk")
	if !ok {
		return
	}
	var req messageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	messageType := req.Type
	if messageType == "" {
		messageType = domain.MessageConsult
	}
	if !messageType.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", "type is invalid")
		return
	}
	u := currentUser(r.Context())
	created, err := s.store.CreateMessage(r.Context(), &domain.Message{
		FromType:      "user",
		FromID:        u.ID,
		ToAgentID:     target.ID,
		SquadID:       target.SquadID,
		Type:          messageType,
		Payload:       messagePayload(req),
		Status:        domain.MessagePending,
		CorrelationID: strings.TrimSpace(req.CorrelationID),
		MaxAttempts:   req.MaxAttempts,
		ExpiresAt:     messageExpiresAt(req),
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), target.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update target agent state")
		return
	}
	s.recordUserAudit(r, "message.create", "message", created.ID, target.SquadID, nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listAgentChatMessages(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadAccessibleAgent(w, r)
	if !ok {
		return
	}
	messages, err := s.store.ListAgentMessageHistory(r.Context(), target.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	u := currentUser(r.Context())
	squad, err := s.store.GetSquad(r.Context(), target.SquadID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if squad.OwnerID != u.ID && u.Role != domain.RolePlatformAdmin {
		filtered := []*domain.Message{}
		for _, msg := range messages {
			if msg.FromType == "user" && msg.FromID == u.ID {
				filtered = append(filtered, msg)
			}
		}
		messages = filtered
	}
	writeJSON(w, http.StatusOK, messages)
}

func messagePayload(req messageRequest) json.RawMessage {
	if len(req.Payload) > 0 {
		return defaultRawJSON(req.Payload, "{}")
	}
	if strings.TrimSpace(req.Message) == "" {
		return json.RawMessage(`{}`)
	}
	payload, err := json.Marshal(map[string]string{"message": req.Message})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return payload
}

func messageExpiresAt(req messageRequest) time.Time {
	if req.TTLSeconds <= 0 {
		return time.Time{}
	}
	return time.Now().UTC().Add(time.Duration(req.TTLSeconds) * time.Second)
}

func requiredMessageAction(messageType domain.MessageType) string {
	switch messageType {
	case domain.MessagePing:
		return "ping"
	case domain.MessageDelegate, domain.MessageHandoff:
		return "add_task"
	default:
		return "talk"
	}
}

type agentRuntimeResource struct {
	ResourceType domain.ResourceType `json:"resource_type"`
	ResourceID   string              `json:"resource_id"`
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	Endpoint     string              `json:"endpoint,omitempty"`
	Manifest     json.RawMessage     `json:"manifest"`
}

func (s *Server) agentRuntimeResource(ctx context.Context, perm *domain.AgentPermission) (agentRuntimeResource, bool, error) {
	if perm.ResourceType == domain.ResLLMProvider {
		provider, err := s.store.GetLLMProvider(ctx, perm.ResourceID)
		if err != nil {
			return agentRuntimeResource{}, false, err
		}
		if provider.Status != domain.ResourceActive {
			return agentRuntimeResource{}, false, nil
		}
		manifest, err := json.Marshal(map[string]any{
			"kind":          provider.Kind,
			"default_model": provider.DefaultModel,
			"models":        json.RawMessage(defaultRawJSON(provider.Models, "[]")),
		})
		if err != nil {
			return agentRuntimeResource{}, false, err
		}
		return agentRuntimeResource{
			ResourceType: perm.ResourceType,
			ResourceID:   provider.ID,
			Name:         provider.Name,
			Description:  provider.Kind,
			Endpoint:     provider.BaseURL,
			Manifest:     manifest,
		}, true, nil
	}
	resource, err := s.store.GetResource(ctx, perm.ResourceType, perm.ResourceID)
	if err != nil {
		return agentRuntimeResource{}, false, err
	}
	if resource.Status != domain.ResourceActive {
		return agentRuntimeResource{}, false, nil
	}
	return agentRuntimeResource{
		ResourceType: resource.Type,
		ResourceID:   resource.ID,
		Name:         resource.Name,
		Description:  resource.Description,
		Endpoint:     resource.Endpoint,
		Manifest:     defaultRawJSON(resource.Manifest, "{}"),
	}, true, nil
}

func (s *Server) claimCurrentAgentTask(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	task, err := s.store.ClaimNextTask(r.Context(), principal.Agent.ID, workerIDFromRequest(r, principal.Agent.ID), defaultTaskExecutionLease)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeStorageError(w, err)
		return
	}
	if err := s.setAgentStatusAndMirror(r.Context(), principal.Agent.ID, domain.AgentBusy); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
		return
	}
	s.recordAgentAudit(r, principal.Agent.ID, "task.claim", "task", task.ID, task.SquadID, nil)
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) startCurrentAgentTask(w http.ResponseWriter, r *http.Request) {
	s.setCurrentAgentTaskStatus(w, r, domain.TaskInProgress, domain.AgentBusy, "task.start")
}

func (s *Server) completeCurrentAgentTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status        domain.TaskStatus `json:"status"`
		Summary       string            `json:"summary"`
		PersistMemory bool              `json:"persist_memory"`
		ExecutionID   string            `json:"execution_id"`
		FencingToken  string            `json:"fencing_token"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	if req.Status == "" {
		req.Status = domain.TaskInReview
	}
	if req.Status != domain.TaskInReview && req.Status != domain.TaskDone {
		writeError(w, http.StatusBadRequest, "bad_request", "status must be in-review or done")
		return
	}
	executionID, fencingToken, ok := requireExecutionFence(w, req.ExecutionID, req.FencingToken)
	if !ok {
		return
	}
	principal := currentAgent(r.Context())
	taskID := chi.URLParam(r, "taskID")
	summary := trimRunes(strings.TrimSpace(req.Summary), maxAgentMemoryContentChars)
	updated, err := s.store.CompleteTaskExecution(r.Context(), principal.Agent.ID, taskID, executionID, fencingToken, req.Status, summary)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
		return
	}
	s.recordAgentAudit(r, principal.Agent.ID, "task.complete", "task", updated.ID, updated.SquadID, nil)
	if req.PersistMemory && strings.TrimSpace(req.Summary) != "" {
		metadata, err := json.Marshal(map[string]any{
			"kind":         "task_completion",
			"task_status":  string(req.Status),
			"execution_id": executionID,
			"source":       "runtime_completion_summary",
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to prepare memory metadata")
			return
		}
		if _, err := s.store.CreateAgentMemory(r.Context(), &domain.AgentMemory{
			AgentID:      principal.Agent.ID,
			SquadID:      updated.SquadID,
			SourceTaskID: updated.ID,
			Content:      summary,
			RawContent:   strings.TrimSpace(req.Summary),
			TrustLevel:   "raw_model_output",
			Provenance:   "task_completion",
			ReviewStatus: "pending_review",
			Metadata:     metadata,
		}); err != nil {
			auditMetadata, _ := json.Marshal(map[string]string{"error": err.Error(), "execution_id": executionID})
			s.recordAgentAudit(r, principal.Agent.ID, "task.memory_persist_failed", "task", updated.ID, updated.SquadID, auditMetadata)
		}
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) blockCurrentAgentTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Summary      string `json:"summary"`
		ExecutionID  string `json:"execution_id"`
		FencingToken string `json:"fencing_token"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	executionID, fencingToken, ok := requireExecutionFence(w, req.ExecutionID, req.FencingToken)
	if !ok {
		return
	}
	principal := currentAgent(r.Context())
	updated, err := s.store.CompleteTaskExecution(
		r.Context(),
		principal.Agent.ID,
		chi.URLParam(r, "taskID"),
		executionID,
		fencingToken,
		domain.TaskBlocked,
		trimRunes(strings.TrimSpace(req.Summary), maxAgentMemoryContentChars),
	)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
		return
	}
	s.recordAgentAudit(r, principal.Agent.ID, "task.block", "task", updated.ID, updated.SquadID, nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) currentAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	var req struct {
		Status       domain.AgentStatus `json:"status"`
		ExecutionID  string             `json:"execution_id"`
		FencingToken string             `json:"fencing_token"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	if req.Status == "" {
		req.Status = principal.Agent.Status
	}
	if req.Status != domain.AgentIdle && req.Status != domain.AgentBusy && req.Status != domain.AgentError {
		writeError(w, http.StatusBadRequest, "bad_request", "status is invalid")
		return
	}
	if req.Status == domain.AgentBusy && strings.TrimSpace(req.ExecutionID) != "" {
		if strings.TrimSpace(req.FencingToken) == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "fencing_token is required with execution_id")
			return
		}
		if _, err := s.store.HeartbeatTaskExecution(r.Context(), principal.Agent.ID, strings.TrimSpace(req.ExecutionID), strings.TrimSpace(req.FencingToken), defaultTaskExecutionLease); err != nil {
			writeStorageError(w, err)
			return
		}
	}
	status := req.Status
	if status == domain.AgentIdle {
		pending, err := s.agentHasPendingWork(r.Context(), principal.Agent.ID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if pending {
			status = domain.AgentBusy
		}
	}
	if err := s.setAgentStatusAndMirror(r.Context(), principal.Agent.ID, status); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
		return
	}
	agent, err := s.store.GetAgent(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) setCurrentAgentTaskStatus(w http.ResponseWriter, r *http.Request, taskStatus domain.TaskStatus, agentStatus domain.AgentStatus, action string) {
	updated, ok := s.updateCurrentAgentTaskStatus(w, r, taskStatus, agentStatus, action)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) updateCurrentAgentTaskStatus(w http.ResponseWriter, r *http.Request, taskStatus domain.TaskStatus, agentStatus domain.AgentStatus, action string) (*domain.Task, bool) {
	principal := currentAgent(r.Context())
	task, err := s.store.GetTask(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if task.AssigneeAgentID != principal.Agent.ID {
		writeError(w, http.StatusForbidden, "forbidden", "task is not assigned to this agent")
		return nil, false
	}
	task.Status = taskStatus
	updated, err := s.store.UpdateTask(r.Context(), task)
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if agentStatus == domain.AgentIdle {
		if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
			return nil, false
		}
	} else if err := s.setAgentStatusAndMirror(r.Context(), principal.Agent.ID, agentStatus); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update agent state")
		return nil, false
	}
	s.recordAgentAudit(r, principal.Agent.ID, action, "task", updated.ID, updated.SquadID, nil)
	return updated, true
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadAccessibleTask(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadOwnedTask(w, r)
	if !ok {
		return
	}
	previousAssignee := task.AssigneeAgentID
	var req struct {
		Title           *string `json:"title"`
		Description     *string `json:"description"`
		AssigneeAgentID *string `json:"assignee_agent_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "title must not be empty")
			return
		}
		task.Title = title
	}
	if req.Description != nil {
		task.Description = *req.Description
	}
	if req.AssigneeAgentID != nil {
		if *req.AssigneeAgentID != "" {
			agent, err := s.store.GetAgent(r.Context(), *req.AssigneeAgentID)
			if err != nil {
				writeStorageError(w, err)
				return
			}
			if agent.SquadID != task.SquadID {
				writeError(w, http.StatusBadRequest, "bad_request", "assignee_agent_id must belong to this squad")
				return
			}
		}
		task.AssigneeAgentID = *req.AssigneeAgentID
	}

	updated, err := s.store.UpdateTask(r.Context(), task)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAffectedAgentsFromTaskChange(r.Context(), previousAssignee, updated.AssigneeAgentID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update assigned agent state")
		return
	}
	s.recordUserAudit(r, "task.update", "task", updated.ID, updated.SquadID, nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) moveTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadOwnedTask(w, r)
	if !ok {
		return
	}
	var req struct {
		Status domain.TaskStatus `json:"status"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Status.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", "status is invalid")
		return
	}
	previousAssignee := task.AssigneeAgentID
	task.Status = req.Status
	updated, err := s.store.UpdateTask(r.Context(), task)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAffectedAgentsFromTaskChange(r.Context(), previousAssignee, updated.AssigneeAgentID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update assigned agent state")
		return
	}
	s.recordUserAudit(r, "task.move", "task", updated.ID, updated.SquadID, nil)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadOwnedTask(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTask(r.Context(), task.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	if task.AssigneeAgentID != "" {
		if err := s.syncAgentStatusFromPendingWork(r.Context(), task.AssigneeAgentID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to update assigned agent state")
			return
		}
	}
	s.recordUserAudit(r, "task.delete", "task", task.ID, task.SquadID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) loadOwnedOrAdminSquad(w http.ResponseWriter, r *http.Request) (*domain.Squad, bool) {
	squad, err := s.store.GetSquad(r.Context(), chi.URLParam(r, "squadID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	u := currentUser(r.Context())
	if squad.OwnerID == u.ID || u.Role == domain.RolePlatformAdmin {
		return squad, true
	}
	writeError(w, http.StatusForbidden, "forbidden", "you do not own this squad")
	return nil, false
}

func (s *Server) loadOwnedOrAdminAgent(w http.ResponseWriter, r *http.Request) (*domain.Agent, bool) {
	agent, err := s.store.GetAgent(r.Context(), chi.URLParam(r, "agentID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if _, ok := s.ensureOwnedOrAdminSquad(w, r, agent.SquadID); !ok {
		return nil, false
	}
	return agent, true
}

func (s *Server) loadOwnedSquad(w http.ResponseWriter, r *http.Request) (*domain.Squad, bool) {
	squad, ok := s.loadAccessibleSquad(w, r)
	if !ok {
		return nil, false
	}
	if squad.OwnerID != currentUser(r.Context()).ID {
		writeError(w, http.StatusForbidden, "forbidden", "you do not own this squad")
		return nil, false
	}
	return squad, true
}

func (s *Server) loadAccessibleSquad(w http.ResponseWriter, r *http.Request) (*domain.Squad, bool) {
	squad, err := s.store.GetSquad(r.Context(), chi.URLParam(r, "squadID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	u := currentUser(r.Context())
	if squad.OwnerID == u.ID || u.Role == domain.RolePlatformAdmin {
		return squad, true
	}
	ok, err := s.store.UserMayAccessSquad(r.Context(), u.ID, squad.ID, "read")
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if !ok {
		s.recordUserAudit(r, "access.denied", "squad", squad.ID, squad.ID, nil)
		writeError(w, http.StatusForbidden, "forbidden", "you do not have access to this squad")
		return nil, false
	}
	return squad, true
}

func (s *Server) loadAccessibleAgent(w http.ResponseWriter, r *http.Request) (*domain.Agent, bool) {
	return s.loadAgentForAction(w, r, "read")
}

func (s *Server) loadAgentForAction(w http.ResponseWriter, r *http.Request, action string) (*domain.Agent, bool) {
	agent, err := s.store.GetAgent(r.Context(), chi.URLParam(r, "agentID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if _, ok := s.ensureSquadActionAccess(w, r, agent.SquadID, action, false); !ok {
		return nil, false
	}
	return agent, true
}

func (s *Server) loadOwnedAgent(w http.ResponseWriter, r *http.Request) (*domain.Agent, bool) {
	agent, err := s.store.GetAgent(r.Context(), chi.URLParam(r, "agentID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if _, ok := s.ensureSquadAccess(w, r, agent.SquadID, true); !ok {
		return nil, false
	}
	return agent, true
}

func (s *Server) loadAccessibleTask(w http.ResponseWriter, r *http.Request) (*domain.Task, bool) {
	task, err := s.store.GetTask(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if _, ok := s.ensureSquadAccess(w, r, task.SquadID, false); !ok {
		return nil, false
	}
	return task, true
}

func (s *Server) loadOwnedTask(w http.ResponseWriter, r *http.Request) (*domain.Task, bool) {
	task, err := s.store.GetTask(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if _, ok := s.ensureSquadAccess(w, r, task.SquadID, true); !ok {
		return nil, false
	}
	return task, true
}

func (s *Server) ensureSquadAccess(w http.ResponseWriter, r *http.Request, squadID string, ownerOnly bool) (*domain.Squad, bool) {
	return s.ensureSquadActionAccess(w, r, squadID, "read", ownerOnly)
}

func (s *Server) ensureSquadActionAccess(w http.ResponseWriter, r *http.Request, squadID string, action string, ownerOnly bool) (*domain.Squad, bool) {
	squad, err := s.store.GetSquad(r.Context(), squadID)
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	u := currentUser(r.Context())
	if squad.OwnerID == u.ID || (!ownerOnly && u.Role == domain.RolePlatformAdmin) {
		return squad, true
	}
	if !ownerOnly {
		ok, err := s.store.UserMayAccessSquad(r.Context(), u.ID, squad.ID, action)
		if err != nil {
			writeStorageError(w, err)
			return nil, false
		}
		if ok {
			return squad, true
		}
	}
	if ownerOnly {
		s.recordUserAudit(r, "access.denied", "squad", squad.ID, squad.ID, nil)
		writeError(w, http.StatusForbidden, "forbidden", "you do not own this squad")
	} else {
		s.recordUserAudit(r, "access.denied", "squad", squad.ID, squad.ID, nil)
		writeError(w, http.StatusForbidden, "forbidden", "you do not have access to this squad")
	}
	return nil, false
}

func (s *Server) ensureOwnedOrAdminSquad(w http.ResponseWriter, r *http.Request, squadID string) (*domain.Squad, bool) {
	squad, err := s.store.GetSquad(r.Context(), squadID)
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	u := currentUser(r.Context())
	if squad.OwnerID == u.ID || u.Role == domain.RolePlatformAdmin {
		return squad, true
	}
	s.recordUserAudit(r, "access.denied", "squad", squad.ID, squad.ID, nil)
	writeError(w, http.StatusForbidden, "forbidden", "you do not own this squad")
	return nil, false
}

func (s *Server) recordUserAudit(r *http.Request, action, resourceType, resourceID, squadID string, metadata json.RawMessage) {
	_ = s.recordUserAuditRequired(r, action, resourceType, resourceID, squadID, metadata)
}

func (s *Server) recordUserAuditRequired(r *http.Request, action, resourceType, resourceID, squadID string, metadata json.RawMessage) error {
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	u := currentUser(r.Context())
	if u == nil {
		return nil
	}
	return s.store.RecordAudit(r.Context(), &domain.AuditEntry{
		ActorType:    "user",
		ActorID:      u.ID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		SquadID:      squadID,
		Metadata:     metadata,
	})
}

func (s *Server) recordAgentAudit(r *http.Request, agentID, action, resourceType, resourceID, squadID string, metadata json.RawMessage) {
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	_ = s.store.RecordAudit(r.Context(), &domain.AuditEntry{
		ActorType:    "agent",
		ActorID:      agentID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		SquadID:      squadID,
		Metadata:     metadata,
	})
}

func (s *Server) recordSystemAudit(ctx context.Context, action, resourceType, resourceID, squadID string, metadata json.RawMessage) error {
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	return s.store.RecordAudit(ctx, &domain.AuditEntry{
		ActorType:    "system",
		ActorID:      uuid.Nil.String(),
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		SquadID:      squadID,
		Metadata:     metadata,
	})
}

func (s *Server) syncAffectedAgentsFromTaskChange(ctx context.Context, beforeAgentID, afterAgentID string) error {
	if beforeAgentID != "" {
		if err := s.syncAgentStatusFromPendingWork(ctx, beforeAgentID); err != nil {
			return err
		}
	}
	if afterAgentID != "" && afterAgentID != beforeAgentID {
		if err := s.syncAgentStatusFromPendingWork(ctx, afterAgentID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) syncAgentStatusFromPendingWork(ctx context.Context, agentID string) error {
	pending, err := s.agentHasPendingWork(ctx, agentID)
	if err != nil {
		return err
	}
	if pending {
		return s.setAgentStatusAndMirror(ctx, agentID, domain.AgentBusy)
	}
	return s.setAgentStatusAndMirror(ctx, agentID, domain.AgentIdle)
}

func (s *Server) agentHasPendingWork(ctx context.Context, agentID string) (bool, error) {
	tasks, err := s.store.ListAgentTasks(ctx, agentID)
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		if task.Status == domain.TaskTodo || task.Status == domain.TaskInProgress {
			return true, nil
		}
	}
	hasMessages, err := s.store.HasPendingMessages(ctx, agentID)
	if err != nil {
		return false, err
	}
	return hasMessages, nil
}

func (s *Server) setAgentStatusAndMirror(ctx context.Context, agentID string, status domain.AgentStatus) error {
	return s.store.SetAgentStatus(ctx, agentID, status)
}

func auditLimit(r *http.Request) int {
	return boundedIntQuery(r, "limit", 100, 500)
}

func boundedIntQuery(r *http.Request, key string, fallback int, maximum int) int {
	limit, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || limit <= 0 {
		return fallback
	}
	if limit > maximum {
		return maximum
	}
	return limit
}

func durationSecondsQuery(r *http.Request, key string, fallback time.Duration, maximum time.Duration) time.Duration {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds < 0 {
		return fallback
	}
	duration := time.Duration(seconds * float64(time.Second))
	if duration > maximum {
		return maximum
	}
	return duration
}

func boolAsInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func trimRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func defaultMeteringCurrency(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "USD"
	}
	return value
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return false
	}
	return true
}

func writeStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, storage.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "resource already exists")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "unexpected storage error")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func namespaceFor(name string) string {
	parts := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	slug := strings.Join(parts, "-")
	if slug == "" {
		slug = "squad"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	return fmt.Sprintf("squad-%s-%s", slug, uuid.NewString()[:8])
}

func generatedCredentialRef(namespace, agentID string) string {
	return fmt.Sprintf("k8s://%s/agent-%s-credential-%s", namespace, agentID, uuid.NewString()[:8])
}

func workerIDFromRequest(r *http.Request, agentID string) string {
	workerID := strings.TrimSpace(r.Header.Get("X-Skquad-Worker-ID"))
	if workerID == "" {
		return agentID
	}
	return workerID
}

func requireExecutionFence(w http.ResponseWriter, executionID string, fencingToken string) (string, string, bool) {
	executionID = strings.TrimSpace(executionID)
	fencingToken = strings.TrimSpace(fencingToken)
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "execution_id is required")
		return "", "", false
	}
	if fencingToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "fencing_token is required")
		return "", "", false
	}
	return executionID, fencingToken, true
}

func generatedVirtualKeyRef(namespace, agentID string) string {
	return fmt.Sprintf("k8s://%s/agent-%s-virtual-key-%s", namespace, agentID, uuid.NewString()[:8])
}

func bearerToken(authorization string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
}

func matchesAgentCredential(token string, identity *domain.AgentIdentity) bool {
	if token == "" || identity == nil {
		return false
	}
	if identity.CredentialHash != "" {
		return subtle.ConstantTimeCompare([]byte(hashCredential(token)), []byte(identity.CredentialHash)) == 1
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(identity.CredentialRef)) == 1
}

func generateCredential() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func hashCredential(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
