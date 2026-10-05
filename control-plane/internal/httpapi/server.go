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
	"log"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/auth"
	"github.com/rossbrigoli/skquad/control-plane/internal/breakglass"
	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/embeddings"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
	"github.com/rossbrigoli/skquad/control-plane/internal/promptcompo"
	"github.com/rossbrigoli/skquad/control-plane/internal/search"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	maxAgentMemoryContentChars = 4000
	defaultTaskExecutionLease  = 2 * time.Minute

	// Route path templates reused across chi route registrations (S-126:
	// S1192 duplicated literal). Kept as constants so the three HTTP verbs
	// for each resource share a single source of truth.
	routeAIProvider       = "/registry/ai-providers/{providerID}"
	routeAIModel          = "/ai-models/{modelID}"
	routeRegistryResource = "/registry/{registryType}/{resourceID}"

	// errWrapFormat wraps a sentinel error with a contextual field name.
	errWrapFormat               = "%w: %s"
	auditAccessDenied           = "access.denied"
	msgNotSquadOwner            = "you do not own this squad"
	msgTaskNotAssigned          = "task is not assigned to this agent"
	msgTypeInvalid              = "type is invalid"
	msgUpdateAgentState         = "failed to update agent state"
	msgUpdateAssignedAgentState = "failed to update assigned agent state"
	msgUpdateTargetAgentState   = "failed to update target agent state"
	routeAgent                  = "/agents/{agentID}"
	routeSquad                  = "/squads/{squadID}"
	routeTask                   = "/tasks/{taskID}"
)

// Binding errors (ADR-0010 D4/D5, WP3). These replace the old
// errNoGatewayModels / "no_llm_models_granted" S-104 error, which blamed
// a missing provider grant for what is really a missing or invalid model
// binding. writeBindingError maps them to HTTP semantics.
type bindingError struct {
	code    string
	message string
}

func (e *bindingError) Error() string { return e.message }

var (
	errModelNotBound   = &bindingError{"model_not_bound", "agent has no primary AI model bound"}
	errModelNotFound   = &bindingError{"model_not_found", "bound AI model does not exist"}
	errModelNotGranted = &bindingError{"model_not_granted", "bound AI model is not granted to the agent's owner"}
	errModelDeprecated = &bindingError{"model_deprecated", "bound AI model is deprecated"}
)

// writeBindingError maps a binding error to its HTTP response and returns
// true when err was a binding error. Provisioning failures that are not
// binding errors fall through to the caller's gateway-unavailable mapping.
func writeBindingError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errModelNotBound):
		writeError(w, http.StatusConflict, "model_not_bound", err.Error())
	case errors.Is(err, errModelNotFound):
		writeError(w, http.StatusBadRequest, "model_not_found", err.Error())
	case errors.Is(err, errModelNotGranted):
		writeError(w, http.StatusForbidden, "model_not_granted", err.Error())
	case errors.Is(err, errModelDeprecated):
		writeError(w, http.StatusConflict, "model_deprecated", err.Error())
	default:
		return false
	}
	return true
}

// Store is the persistence surface required by the current API slice.
type Store interface {
	storage.UserStore
	storage.SquadStore
	storage.AgentStore
	storage.BoardStore
	storage.GrantStore
	storage.RegistryStore
	storage.AIModelStore
	storage.PermissionStore
	storage.MeteringStore
	storage.WakeLatencyStore
	storage.KubernetesOutboxStore
	storage.AuditStore
	storage.TaskStore
	storage.AgentMemoryStore
	storage.MessageStore
	storage.InboxStore
	storage.NotificationStore
	storage.WorkNotificationStore
	storage.PromptTierStore
	storage.PromptTemplateStore
	storage.BuiltinToolStore
	storage.PlatformSettingsStore
	storage.BudgetStore
	storage.BudgetEnforcementStore
	storage.AgentMirrorQueue
	storage.UploadStore
}

// Server owns HTTP routing and request-scoped dependencies.
type Server struct {
	cfg        *config.Config
	store      Store
	oidcAuth   OIDCAuthenticator
	crWriter   CRWriter
	llmGateway LLMGatewayProvisioner
	// breakGlass is the OIDC-independent admin path. nil or disabled means the
	// endpoints return 404 and no break-glass bearer is ever accepted.
	breakGlass *breakglass.Auth
	// platformPrompt is the operator platform-tier prompt override loaded
	// once at startup from SKQUAD_PLATFORM_PROMPT_FILE ("" => embedded
	// default). WP2's composePromptForAgent passes it as the composer's
	// platformOverride argument (see platform_prompt.go).
	platformPrompt string
	// searchProviders backs the BT-2 web_search proxy (ADR-0012 §3).
	// Built at startup from control-plane secrets; keyed by provider name
	// (duckduckgo is always present — keyless). A provider whose API key
	// is unset is absent, which the proxy maps to 502.
	searchProviders map[string]search.Provider
	// providerKeys is the S-155 Secret backend for pasted provider API
	// keys. Built at startup when K8s connection config is present;
	// nil elsewhere (dev without a cluster). Creating/updating a provider
	// with a key while nil returns 503 rather than storing plaintext.
	providerKeys ProviderKeyStore
	// podRestarter is the S-162 restart path: deletes agent pods by
	// label through the K8s API. Built at startup when K8s connection
	// config is present; nil elsewhere (dev). Restart while nil → 503.
	podRestarter PodRestarter
	// gwModels is the S-GWREG gateway model-deployment registry (the
	// model_list half of the LiteLLM admin API). Built from the same
	// LiteLLMAdminURL/master-key config as llmGateway; nil when the
	// gateway is unconfigured (dev) — AI-model CRUD then skips gateway
	// provisioning with a warning instead of failing.
	gwModels GatewayModelRegistry
	// gwReloader rollout-restarts the gateway Deployment after model
	// deployment changes (litellm router only picks up DB-persisted
	// models on pod restart — docs/llm-gateway.md). Built from K8s
	// config; nil in dev → reload skipped with a warning.
	gwReloader GatewayReloader
	// embeddings (S-212) generates memory embeddings through the
	// LiteLLM gateway's /v1/embeddings endpoint. Built at startup only
	// when SKQUAD_MEMORY_EMBEDDINGS_ENABLED=true and a model + gateway
	// are configured; nil otherwise — write paths then store rows
	// without vectors and the search endpoint answers 503.
	embeddings EmbeddingsClient
	// embedderConfig (S-212, ADR-0013 §5) writes the admin's embedder
	// runtime override into the ConfigMap the operator reconciles.
	// Built at startup when K8s connection config is present; nil in
	// dev — PUT embedder_runtime then answers 503 instead of silently
	// storing a setting nothing acts on.
	embedderConfig EmbedderConfigWriter
}

// EmbedderConfigWriter persists the admin's embedder runtime choice to
// the operator-consumed override ConfigMap (ADR-0013 §5).
type EmbedderConfigWriter interface {
	SetEmbedderRuntime(ctx context.Context, runtime string) error
}

// EmbeddingsClient generates text embeddings via the gateway (S-212).
// Production implementation is *embeddings.Client; tests inject fakes
// (or point the config at an httptest fake gateway).
type EmbeddingsClient interface {
	Embed(ctx context.Context, text string) ([]float64, error)
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

// LLMGatewayProvisioner issues and maintains agent-scoped LiteLLM virtual keys.
type LLMGatewayProvisioner interface {
	// ProvisionAgentKey creates a new virtual key and returns the key
	// (handed to the agent) plus its token (kept for later update/revoke).
	ProvisionAgentKey(ctx context.Context, req GatewayKeyRequest) (key string, token string, err error)
	// UpdateAgentKey rewrites the model allow-list and fallback/router
	// configuration of an existing key.
	UpdateAgentKey(ctx context.Context, token string, req GatewayKeyRequest) error
	// RevokeAgentKey deletes a key so further model calls fail.
	RevokeAgentKey(ctx context.Context, token string) error
	// FindKeyByAlias returns the token of an existing key with the given
	// alias (found=false when absent). Used to adopt a gateway key whose
	// token the identity row lost (S-129).
	FindKeyByAlias(ctx context.Context, alias string) (token string, found bool, err error)
}

// GatewayKeyRequest describes the access a new runtime virtual key should have.
type GatewayKeyRequest struct {
	AgentID string
	SquadID string
	Models  []string
	// PrimaryModel is the LiteLLM model name of the agent's bound primary.
	// It keys the gateway's fallback mapping.
	PrimaryModel string
	// FallbackModel is the LiteLLM model name of the agent's bound
	// fallback. The gateway router performs the failover (ADR-0010 D6);
	// the runtime never sees this and never implements fallback itself.
	FallbackModel string
}

// New returns an HTTP handler for the control-plane API.
func New(cfg *config.Config, store Store) http.Handler {
	return NewWithOIDCAuthenticator(cfg, store, nil)
}

// NewWithCRWriter returns an HTTP handler that mirrors squad/agent mutations
// to Kubernetes CRs.
func NewWithCRWriter(cfg *config.Config, store Store, crWriter CRWriter) http.Handler {
	return newServer(cfg, store, serverDeps{crWriter: crWriter})
}

// NewWithDependencies returns an HTTP handler with explicit optional
// integrations for tests and production startup.
func NewWithDependencies(cfg *config.Config, store Store, oidcAuth OIDCAuthenticator, crWriter CRWriter, providerKeys ProviderKeyStore) http.Handler {
	return newServer(cfg, store, serverDeps{oidcAuth: oidcAuth, crWriter: crWriter, providerKeys: providerKeys})
}

// NewWithPodRestarter wires an explicit PodRestarter (S-162 tests).
func NewWithPodRestarter(cfg *config.Config, store Store, restarter PodRestarter) http.Handler {
	return newServer(cfg, store, serverDeps{restarter: restarter})
}

// NewWithOIDCAuthenticator returns an HTTP handler using oidcAuth when
// SKQUAD_AUTH_MODE=oidc.
func NewWithOIDCAuthenticator(cfg *config.Config, store Store, oidcAuth OIDCAuthenticator) http.Handler {
	return newServer(cfg, store, serverDeps{oidcAuth: oidcAuth})
}

// NewWithSearchProviders returns an HTTP handler whose web_search proxy
// uses the given providers (BT-2). A nil map builds the production set
// from config secrets (duckduckgo always; brave/perplexity only when
// their API keys are set). Tests inject stub providers here.
func NewWithSearchProviders(cfg *config.Config, store Store, providers map[string]search.Provider) http.Handler {
	return newServer(cfg, store, serverDeps{searchProviders: providers})
}

// NewWithProviderKeyStore returns an HTTP handler whose pasted provider
// API keys are stored through the given Secret store (S-155). Tests use
// this to inject a fake; production builds the store from config inside
// newServer.
func NewWithProviderKeyStore(cfg *config.Config, store Store, keys ProviderKeyStore) http.Handler {
	return newServer(cfg, store, serverDeps{providerKeys: keys})
}

// NewWithGatewayReload returns an HTTP handler using the given gateway
// rollout reloader (S-GWREG tests). The gateway model registry itself
// is built from cfg.LiteLLMAdminURL/master key (point those at an
// httptest fake gateway in tests); the reloader is injected here.
func NewWithGatewayReload(cfg *config.Config, store Store, oidcAuth OIDCAuthenticator, reloader GatewayReloader) http.Handler {
	return newServer(cfg, store, serverDeps{oidcAuth: oidcAuth, gwReloader: reloader})
}

// NewWithEmbedderRuntimeWriter wires an explicit EmbedderConfigWriter
// (S-212, ADR-0013 §5 tests). Production builds the writer from
// in-cluster K8s config inside newServer.
func NewWithEmbedderRuntimeWriter(cfg *config.Config, store Store, oidcAuth OIDCAuthenticator, crWriter CRWriter, writer EmbedderConfigWriter) http.Handler {
	return newServer(cfg, store, serverDeps{oidcAuth: oidcAuth, crWriter: crWriter, embedderConfig: writer})
}

// serverDeps bundles the optional integrations for newServer (S-189:
// params struct replaces the 10-arg signature). Zero values mean
// "not wired", exactly like the previous nil arguments.
type serverDeps struct {
	oidcAuth           OIDCAuthenticator
	crWriter           CRWriter
	searchProviders    map[string]search.Provider
	providerKeys       ProviderKeyStore
	restarter          PodRestarter
	gwReloader         GatewayReloader
	injectedEmbeddings EmbeddingsClient
	embedderConfig     EmbedderConfigWriter
}

// buildLLMGateway constructs the LiteLLM gateway provisioner and model
// registry from config (S-189 split out of newServer). Missing config
// leaves the noop provisioner in place. Misconfiguration with a partial
// setup panics — same as the pre-split inline code.
func buildLLMGateway(cfg *config.Config) (LLMGatewayProvisioner, GatewayModelRegistry) {
	llmGateway := LLMGatewayProvisioner(noopLLMGateway{})
	var gwModels GatewayModelRegistry
	if cfg == nil || cfg.LiteLLMAdminURL == "" || cfg.LiteLLMMasterKey == "" {
		return llmGateway, gwModels
	}
	gw, err := newLiteLLMGatewayClient(cfg.LiteLLMAdminURL, cfg.LiteLLMMasterKey)
	if err != nil {
		panic(fmt.Sprintf("litellm gateway client: %v", err))
	}
	// S-GWREG: same client, model-deployment surface (avoids a
	// second HTTP pool / duplicated master-key handling).
	return gw, gw
}

// resolveProviderKeys returns the explicit provider-key store when given,
// otherwise builds one from in-cluster K8s config (S-189 split).
func resolveProviderKeys(cfg *config.Config, explicit ProviderKeyStore) ProviderKeyStore {
	if explicit != nil || cfg == nil || !cfg.K8sEnabled || cfg.K8sAPIBase == "" || cfg.K8sTokenFile == "" {
		return explicit
	}
	keys, err := kube.NewSecretStore(cfg)
	if err != nil {
		log.Printf("provider key store unavailable (provider key paste disabled): %v", err)
		return nil
	}
	return keys
}

// resolvePodRestarter returns the explicit restarter when given, otherwise
// builds from in-cluster K8s config. Guard against the typed-nil trap:
// newPodRestarter returns a nil *kube.PodRestarter when config is absent,
// which must not be assigned to the non-nil PodRestarter interface.
func resolvePodRestarter(cfg *config.Config, explicit PodRestarter) PodRestarter {
	if explicit != nil {
		return explicit
	}
	built, err := newPodRestarter(cfg)
	if err != nil {
		log.Printf("pod restarter unavailable (restart agent disabled): %v", err)
		return nil
	}
	if built == nil {
		return nil
	}
	return built
}

// resolveGatewayReloader returns the explicit reloader when given,
// otherwise builds from in-cluster K8s config. Same typed-nil guard
// pattern as the pod restarter.
func resolveGatewayReloader(cfg *config.Config, explicit GatewayReloader) GatewayReloader {
	if explicit != nil {
		return explicit
	}
	built, err := newGatewayReloader(cfg)
	if err != nil {
		log.Printf("gateway reloader unavailable (model changes need a manual gateway restart): %v", err)
		return nil
	}
	if built == nil {
		return nil
	}
	return built
}

// resolveEmbedderConfig returns the explicit embedder runtime override
// writer when given, otherwise builds from in-cluster K8s config.
func resolveEmbedderConfig(cfg *config.Config, explicit EmbedderConfigWriter) EmbedderConfigWriter {
	if explicit != nil {
		return explicit
	}
	if cfg == nil || !cfg.K8sEnabled || cfg.K8sAPIBase == "" || cfg.K8sTokenFile == "" {
		return nil
	}
	built, err := kube.NewEmbedderConfigStore(cfg)
	if err != nil {
		log.Printf("embedder runtime override writer unavailable (PUT embedder_runtime disabled): %v", err)
		return nil
	}
	if built == nil {
		return nil
	}
	return built
}

// buildMemoryEmbeddings builds the memory-RAG embeddings client when the
// feature is enabled with a model and a reachable gateway config.
// Gateway base prefers the admin URL (same LiteLLM instance) and falls
// back to the agent-facing gateway URL; the master key authenticates the
// control plane as a platform caller.
func buildMemoryEmbeddings(cfg *config.Config) EmbeddingsClient {
	if cfg == nil || !cfg.MemoryEmbeddingsEnabled || cfg.MemoryEmbeddingModel == "" {
		return nil
	}
	gatewayBase := cfg.LiteLLMAdminURL
	if gatewayBase == "" {
		gatewayBase = cfg.LLMGatewayURL
	}
	if gatewayBase == "" || cfg.LiteLLMMasterKey == "" {
		return nil
	}
	embedder, err := embeddings.NewClient(gatewayBase, cfg.LiteLLMMasterKey, cfg.MemoryEmbeddingModel)
	if err != nil {
		log.Printf("memory embeddings client unavailable (memories stored without vectors): %v", err)
		return nil
	}
	return embedder
}

// buildBreakGlass builds the break-glass emergency admin path from config.
// Returns nil when disabled; panics when enabled-but-misconfigured so the
// failure surfaces at startup, not at 3am.
func buildBreakGlass(cfg *config.Config) *breakglass.Auth {
	if cfg == nil || !cfg.BreakGlassEnabled {
		return nil
	}
	bgCfg, err := cfg.BreakGlassConfig()
	if err != nil {
		panic(fmt.Sprintf("break-glass is enabled but misconfigured: %v", err))
	}
	bg, err := breakglass.New(*bgCfg)
	if err != nil {
		panic(fmt.Sprintf("break-glass is enabled but misconfigured: %v", err))
	}
	log.Printf("break-glass admin path ENABLED (username=%q, allowed_cidrs=%d, ttl=%s) - reachable only from the allowlisted networks",
		cfg.BreakGlassUsername, len(bgCfg.AllowedCIDRs), cfg.BreakGlassTokenTTL)
	return bg
}

func newServer(cfg *config.Config, store Store, deps serverDeps) http.Handler {
	crWriter := deps.crWriter
	if crWriter == nil {
		crWriter = noopCRWriter{}
	}
	llmGateway, gwModels := buildLLMGateway(cfg)
	s := &Server{cfg: cfg, store: store, oidcAuth: deps.oidcAuth, crWriter: crWriter, llmGateway: llmGateway}
	// S-155: provider-key Secret store. Explicit argument wins (tests);
	// otherwise build from in-cluster K8s config. Absent config is fine
	// (dev); handlers surface the gap when a key is actually pasted.
	s.providerKeys = resolveProviderKeys(cfg, deps.providerKeys)
	// S-162: pod restarter for the Restart Agent button. Explicit
	// argument wins (tests); otherwise build from in-cluster K8s config.
	s.podRestarter = resolvePodRestarter(cfg, deps.restarter)
	// S-GWREG: gateway rollout reloader. Explicit argument wins (tests);
	// otherwise build from in-cluster K8s config.
	s.gwReloader = resolveGatewayReloader(cfg, deps.gwReloader)
	s.gwModels = gwModels
	// S-212 (ADR-0013 §5): embedder runtime override writer. Built
	// from the same in-cluster K8s config as the provider-key store;
	// absent config (dev) leaves nil and the admin PUT answers 503
	// rather than storing a setting nothing acts on.
	s.embedderConfig = resolveEmbedderConfig(cfg, deps.embedderConfig)
	// S-212: embeddings client for memory RAG. Built only when the
	// feature is enabled with a model and a reachable gateway config.
	s.embeddings = buildMemoryEmbeddings(cfg)
	searchProviders := deps.searchProviders
	if searchProviders == nil {
		searchProviders = defaultSearchProviders(cfg)
	}
	s.searchProviders = searchProviders
	// S-212 test seam: an injected embeddings client wins over the
	// config-built one (mirrors the providerKeys/restarter pattern).
	if deps.injectedEmbeddings != nil {
		s.embeddings = deps.injectedEmbeddings
	}

	// Platform-prompt override is loaded at startup and fails loudly: an
	// operator who configured a platform prompt must never silently get
	// the embedded default instead (ADR-0011 D4).
	platformPrompt, err := loadPlatformPromptOverride()
	if err != nil {
		panic(fmt.Sprintf("platform prompt override is configured but unusable: %v", err))
	}
	s.platformPrompt = platformPrompt

	// Break-glass is built at startup. If it is ENABLED but misconfigured we fail
	// loudly here rather than quietly running with a broken emergency path — a
	// silent failure would only be discovered at 3am when it is actually needed.
	s.breakGlass = buildBreakGlass(cfg)

	r := chi.NewRouter()
	r.Get("/healthz", s.health)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/gateway/metering", s.ingestGatewayMetering)

		// Break-glass login. Registered outside the human-auth middleware because
		// it is the thing that has to work when that middleware cannot be satisfied.
		r.Route("/auth/breakglass", func(r chi.Router) {
			r.Get("/status", s.breakGlassStatus)
			r.Post("/login", s.breakGlassLogin)
		})

		r.Route("/agents/me", func(r chi.Router) {
			r.Use(s.authenticateAgent)

			r.Get("/tasks", s.listCurrentAgentTasks)
			r.Get("/resources", s.listCurrentAgentResources)
			r.Get("/messages", s.listCurrentAgentMessages)
			r.Get("/messages/history", s.listCurrentAgentMessageHistory)
			// S-164: squad-peer roster so runtimes can resolve a target agent
			// name ("Mary") for the send_message tool.
			r.Get("/peers", s.listMySquadPeers)
			r.Post("/messages", s.createCurrentAgentMessage)
			r.Post("/notify-owner", s.notifyOwnerFromAgent)
			// S-193: send_inbox builtin — agent-authored content to the
			// squad owner's inbox (kind=agent_message).
			r.Post("/inbox", s.sendInboxFromAgent)
			r.Post("/messages/{messageID}/ack", s.ackCurrentAgentMessage)
			r.Post("/messages/{messageID}/fail", s.failCurrentAgentMessage)
			// S-194: an agent can fetch images attached inside its own
			// squad (the chat/task payload URLs).
			r.Get("/uploads/{uploadID}", s.getMyUploadBytes)
			// S-200: the runtime reads its bound model's vision/tool
			// capability at wake to gate image passthrough.
			r.Get("/model", s.getMyModel)
			// S-175: the runtime polls a turn's status to notice user cancels.
			r.Get("/messages/{messageID}", s.getCurrentAgentMessage)
			r.Get("/work/wait", s.waitCurrentAgentWork)
			r.Post("/tasks/claim", s.claimCurrentAgentTask)
			r.Get("/tasks/{taskID}/context", s.getCurrentAgentTaskContext)
			r.Post("/tasks/{taskID}/start", s.startCurrentAgentTask)
			r.Post("/tasks/{taskID}/complete", s.completeCurrentAgentTask)
			r.Post("/tasks/{taskID}/block", s.blockCurrentAgentTask)
			r.Post("/tasks/{taskID}/workspace", s.reportCurrentAgentTaskWorkspace)
			// S-183: the runtime streams the agent's working turns into the
			// task thread while the task runs.
			r.Post("/tasks/{taskID}/thread", s.appendCurrentAgentTaskThread)
			r.Post("/heartbeat", s.currentAgentHeartbeat)
			// S-PROMPT WP2: composed effective prompt for the runtime
			// (ETag = composition sha256, ADR-0011 D4).
			r.Get("/prompt", s.getMyComposedPrompt)
			// BT-2: built-in tools config for the runtime (ETag, ADR-0012 §2).
			r.Get("/tools", s.getMyBuiltinTools)
			// S-212: semantic recall over the agent's own long-term memory.
			r.Get("/memory/search", s.searchMyAgentMemory)
		})

		// BT-2: built-in tool proxies (agent-credential auth). Keys stay
		// server-side; the runtime only ever sees results.
		r.Route("/tools", func(r chi.Router) {
			r.Use(s.authenticateAgent)
			r.Post("/web_search", s.agentWebSearch)
			// BT-6: web_fetch proxy — agent pods have no internet egress
			// (default-deny), so the guarded fetch runs here.
			r.Post("/web_fetch", s.agentWebFetch)
		})

		r.Group(func(r chi.Router) {
			r.Use(s.authenticate)

			r.Get("/auth/me", s.me)

			// S-130: component versions for the About page. Readable by any
			// authenticated user of any role.
			r.Get("/versions", s.getVersions)

			r.Get("/dashboard", s.getDashboard)
			// S-190: daily usage series + MTD totals for the dashboard
			// histograms and provider usage section.
			r.Get("/dashboard/usage", s.getDashboardUsage)
			r.Get("/inbox", s.listInbox)
			r.Post("/inbox/{messageID}/read", s.markInboxRead)
			// S-193: explicit user delete — the only removal path inbox
			// messages ever have.
			r.Delete("/inbox/{messageID}", s.deleteInboxMessage)
			// S-216: inbox attachments — recipient/admin-only metadata and
			// authenticated byte downloads (the agent upload path is the
			// multipart variant of POST /agents/me/inbox).
			r.Get("/inbox/{messageID}/attachments", s.listInboxMessageAttachments)
			r.Get("/inbox/{messageID}/attachments/{attachmentID}", s.serveInboxAttachment)
			// S-193: bell notifications (task failed/stuck, agent died,
			// task blocked). ?user_id= is the platform-admin filter.
			r.Get("/notifications", s.listNotifications)
			// S-199: per-user notification mute preferences (Settings screen).
			r.Get("/notifications/preferences", s.getNotificationPreferences)
			r.Put("/notifications/preferences", s.putNotificationPreferences)
			r.Post("/notifications/{notificationID}/read", s.markNotificationRead)
			r.Post("/notifications/read-all", s.markAllNotificationsRead)

			// S-194: image attachments (chat composer + task threads).
			r.Post("/uploads", s.createUpload)
			r.Get("/uploads/{uploadID}", s.getUpload)

			r.Post("/squads", s.createSquad)
			r.Get("/squads", s.listSquads)
			r.Get(routeSquad, s.getSquad)
			r.Patch(routeSquad, s.updateSquad)
			r.Delete(routeSquad, s.deleteSquad)
			r.Post("/squads/{squadID}/access-grants", s.createGrant)
			r.Get("/squads/{squadID}/access-grants", s.listGrants)
			r.Get("/squads/{squadID}/wake-latency", s.listSquadWakeLatency)
			r.Delete("/access-grants/{grantID}", s.deleteGrant)

			r.Post("/squads/{squadID}/agents", s.createAgent)
			r.Get("/squads/{squadID}/agents", s.listAgents)
			r.Get(routeAgent, s.getAgent)
			r.Patch(routeAgent, s.updateAgent)
			r.Delete(routeAgent, s.deleteAgent)
			r.Post("/agents/{agentID}/chat", s.createAgentChatMessage)
			r.Get("/agents/{agentID}/chat", s.listAgentChatMessages)
			// S-175: stop button — cancel the agent's in-flight chat turn.
			r.Post("/agents/{agentID}/chat/cancel", s.cancelAgentChatTurn)
			// S-162: reset the chat thread / restart the agent pod.
			r.Post("/agents/{agentID}/chat/reset", s.resetAgentChat)
			r.Post("/agents/{agentID}/restart", s.restartAgent)
			r.Post("/agents/{agentID}/wake", s.wakeAgent)
			r.Post("/agents/{agentID}/identity", s.createAgentIdentity)
			r.Post("/agents/{agentID}/identity/rotate", s.rotateAgentIdentity)
			r.Get("/agents/{agentID}/permissions", s.listAgentPermissions)
			r.Put("/agents/{agentID}/permissions", s.setAgentPermissions)

			r.Get("/squads/{squadID}/board", s.getBoard)
			r.Get("/squads/{squadID}/metering", s.getSquadMetering)
			r.Get("/squads/{squadID}/audit", s.listSquadAudit)
			r.Post("/squads/{squadID}/board/tasks", s.createTask)
			r.Get(routeTask, s.getTask)
			r.Get("/tasks/{taskID}/messages", s.listTaskMessages)
			r.Post("/tasks/{taskID}/messages", s.createTaskMessage)
			r.Patch(routeTask, s.updateTask)
			r.Post("/tasks/{taskID}/move", s.moveTask)
			r.Delete(routeTask, s.deleteTask)
			r.Get("/agents/{agentID}/metering", s.getAgentMetering)

			// S-174: agent inbox observability + owner dead-letter replay
			// (squad owner or platform_admin, enforced in-handler).
			r.Get("/agents/{agentID}/inbox", s.getAgentInbox)
			r.Post("/agents/{agentID}/messages/{messageID}/replay", s.replayAgentDeadMessage)

			// S-174: platform-admin dead-letter screen (list / replay any /
			// prune any).
			r.Get("/admin/dead-letters", s.listAdminDeadLetters)
			r.Post("/admin/dead-letters/{messageID}/replay", s.replayAdminDeadLetter)
			r.Delete("/admin/dead-letters/{messageID}", s.pruneAdminDeadLetter)

			// S-183: platform-admin settings (idle scale-to-zero minutes).
			r.Get("/admin/settings", s.getAdminSettings)
			r.Put("/admin/settings", s.putAdminSettings)

			// S-203 WP1: budget control plane (platform_admin only).
			r.Get("/admin/budgets", s.listBudgetsAdmin)
			r.Get("/admin/budgets/platform", s.getPlatformBudget)
			r.Put("/admin/budgets/platform", s.putPlatformBudget)
			r.Get("/admin/budgets/users/{userID}", s.getUserBudgetAdmin)
			r.Put("/admin/budgets/users/{userID}", s.putUserBudgetAdmin)

			r.Post("/registry/ai-providers", s.createAIProvider)
			r.Get("/registry/ai-providers", s.listAIProviders)
			r.Get(routeAIProvider, s.getAIProvider)
			r.Patch(routeAIProvider, s.updateAIProvider)
			r.Post("/registry/ai-providers/{providerID}/deprecate", s.deprecateAIProvider)
			r.Delete(routeAIProvider, s.deleteAIProvider)
			// S-125: live model list from the provider (OpenAI-compatible
			// passthrough) for the register-model dropdown.
			r.Get("/registry/ai-providers/{providerID}/models", s.listAIProviderModels)
			r.Get("/registry/ai-providers/{providerID}/model-metadata", s.getAIProviderModelMetadata)
			// S-180: pre-save Test buttons (provider connection + model round-trip).
			r.Post("/registry/ai-providers/test", s.testProviderConnection)

			r.Get("/ai-models", s.listAIModels)
			r.Post("/ai-models", s.createAIModel)
			// S-180: pre-save model round-trip test ("Reply exactly with PONG").
			r.Post("/ai-models/test", s.testModelRoundTrip)
			r.Get(routeAIModel, s.getAIModel)
			r.Patch(routeAIModel, s.updateAIModel)
			r.Post("/ai-models/{modelID}/deprecate", s.deprecateAIModel)
			r.Delete(routeAIModel, s.deleteAIModel)

			r.Get("/users", s.listUsers)
			r.Patch("/users/{userID}/role", s.setUserRole)
			r.Get("/users/{userID}/models", s.listUserModels)
			r.Put("/users/{userID}/models", s.setUserModels)
			r.Delete("/users/{userID}/models/{modelID}", s.revokeUserModel)

			// Self-service read for the agent UI: calling user's granted,
			// active models only (ADR-0010 D3).
			r.Get("/models/me", s.listMyModels)

			r.Post("/registry/{registryType}", s.createRegistryResource)
			r.Get("/registry/{registryType}", s.listRegistryResources)
			r.Get(routeRegistryResource, s.getRegistryResource)
			r.Patch(routeRegistryResource, s.updateRegistryResource)
			r.Post("/registry/{registryType}/{resourceID}/deprecate", s.deprecateRegistryResource)
			r.Delete(routeRegistryResource, s.deleteRegistryResource)

			r.Get("/metering/summary", s.getMeteringSummary)
			// S-203 WP1: scoped cost aggregation for the WP2 Cost
			// Management UI (admin sees all, users see owned+granted).
			r.Get("/costs/summary", s.getCostSummary)
			r.Get("/audit", s.listAudit)
			r.Post("/admin/gateway/keys/reconcile", s.reconcileGatewayKeys)
			// S-GWREG: model-deployment drift reconcile — registers active
			// registry models missing from the gateway, reports extras
			// without deleting them. platform_admin only, same as keys
			// reconcile.
			r.Post("/admin/gateway/models/reconcile", s.reconcileGatewayModels)

			// BT-2: built-in platform tools admin surface (platform_admin
			// only, ADR-0012 §2).
			r.Get("/admin/tools", s.listBuiltinToolsAdmin)
			r.Patch("/admin/tools/{name}", s.patchBuiltinTool)
			// S-232: explicit no-delete for built-ins (clear 405, not a hidden UI detail).
			r.Delete("/admin/tools/{name}", s.deleteBuiltinTool)

			// S-PROMPT WP2: prompt tier APIs (ADR-0011 §3).
			r.Get("/settings/prompt", s.getOrgPrompt)
			r.Put("/settings/prompt", s.putOrgPrompt)
			r.Get("/prompt/effective", s.getEffectivePrompt)
			r.Get("/prompt/revisions", s.listPromptRevisions)
			r.Post("/prompt/validate", s.validatePrompt)

			// S-158: prompt templates. Every authenticated user may list
			// them (create-time picker); only platform admins may mutate.
			r.Get("/prompt-templates", s.listPromptTemplates)
			r.Get(routePromptTemplateByID, s.getPromptTemplate)
			r.Post("/prompt-templates", s.createPromptTemplate)
			r.Patch(routePromptTemplateByID, s.updatePromptTemplate)
			r.Delete("/prompt-templates/bulk", s.bulkDeletePromptTemplates)
			r.Delete(routePromptTemplateByID, s.deletePromptTemplate)
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

func (noopLLMGateway) ProvisionAgentKey(context.Context, GatewayKeyRequest) (string, string, error) {
	key, err := generateCredential()
	if err != nil {
		return "", "", err
	}
	return key, "noop-" + key, nil
}

func (noopLLMGateway) UpdateAgentKey(context.Context, string, GatewayKeyRequest) error { return nil }
func (noopLLMGateway) RevokeAgentKey(context.Context, string) error                    { return nil }
func (noopLLMGateway) FindKeyByAlias(context.Context, string) (string, bool, error) {
	return "", false, nil
}

type principalKey struct{}
type agentPrincipalKey struct{}

type agentPrincipal struct {
	Agent    *domain.Agent
	Identity *domain.AgentIdentity
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Break-glass is checked AHEAD of the AuthMode switch so an operator can
		// get in regardless of mode and regardless of Dex being reachable.
		if s.breakGlass != nil && s.breakGlass.Enabled() && s.tryBreakGlassAuth(w, r, next) {
			return
		}
		switch s.cfg.AuthMode {
		case config.AuthDev:
			s.serveDevAuth(w, r, next)
		case config.AuthOIDC:
			s.serveOIDCAuth(w, r, next)
		default:
			writeError(w, http.StatusInternalServerError, "internal", "unsupported auth mode")
		}
	})
}

// tryBreakGlassAuth handles a break-glass bearer token when one is present.
// It returns true when the request was fully handled (allowed or rejected);
// false means the token is not a break-glass token and normal auth proceeds.
func (s *Server) tryBreakGlassAuth(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	tok := bearerToken(r.Header.Get("Authorization"))
	if !breakglass.IsBreakGlassToken(tok) {
		return false
	}
	claims, err := s.breakGlass.VerifyToken(tok, time.Now())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired break-glass token")
		return true
	}
	user := &domain.User{
		ID:            claims.ID,
		Email:         claims.Email,
		Name:          "break-glass",
		Role:          domain.RolePlatformAdmin,
		OIDCIssuer:    "local",
		OIDCSubject:   "breakglass",
		EmailVerified: true,
	}
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, user)))
	return true
}

// serveDevAuth provisions the configured dev principal and serves the request.
func (s *Server) serveDevAuth(w http.ResponseWriter, r *http.Request, next http.Handler) {
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
	s.ensureDefaultBudget(r, user.ID)
	if err := s.store.SetUserRole(r.Context(), user.ID, domain.RolePlatformAdmin); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to promote dev principal")
		return
	}
	user.Role = domain.RolePlatformAdmin
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, user)))
}

// serveOIDCAuth authenticates the bearer token against the OIDC provider,
// bootstraps the Layer-1 role from group claims on first login, and
// serves the request.
func (s *Server) serveOIDCAuth(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if s.oidcAuth == nil {
		writeError(w, http.StatusInternalServerError, "internal", "OIDC authentication is not configured")
		return
	}
	profile, err := s.oidcAuth.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return
	}
	// Role binding: platform_admin only via a configured IdP group.
	// Everyone else lands as RoleUser.
	desiredRole := domain.RoleUser
	if s.cfg.AdminGroupMatched(profile.Groups) {
		desiredRole = domain.RolePlatformAdmin
	}
	user, err := s.store.UpsertUser(r.Context(), &domain.User{
		OIDCIssuer:    profile.Issuer,
		OIDCSubject:   profile.Subject,
		Email:         profile.Email,
		EmailVerified: profile.EmailVerified,
		Name:          profile.Name,
		Role:          desiredRole,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to load authenticated principal")
		return
	}
	s.ensureDefaultBudget(r, user.ID)
	// Group claims bootstrap the role on FIRST login (INSERT) only.
	// After the row exists, the role is app-managed exclusively via
	// PATCH /api/v1/users/{userID}/role: in-app promotions and
	// demotions stick across logins, and a stale or misconfigured
	// SKQUAD_OIDC_ADMIN_GROUPS can neither re-promote an existing
	// row nor demote one. Recovery with no usable admin is what the
	// break-glass path is for.
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, user)))
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

// resourceTypeFromString maps a grant request to a GRANTABLE resource type.
// ai_provider is deliberately absent (ADR-0010 / S-107): model access is
// granted to users via AI Models, not to agents via providers. The constant
// itself stays until WP8 drops the DB CHECK constraint.
func resourceTypeFromString(value string) (domain.ResourceType, bool) {
	switch domain.ResourceType(value) {
	case domain.ResSkill, domain.ResTool, domain.ResAPI, domain.ResKnowledgeBase, domain.ResProjectWorkspace:
		return domain.ResourceType(value), true
	default:
		return "", false
	}
}

func validateName(w http.ResponseWriter, name string) bool {
	return validateRequired(w, "name", name)
}

// validateGitWorkspace enforces the v1 contract for a git-backed project
// workspace: it must declare kind=git, a repo URL in endpoint, a default
// branch, and a credential ref (auth_ref) the operator mounts into agent
// pods. The credential type (HTTPS token vs SSH key) is opaque here — the
// operator resolves auth_ref to a Secret, so both work.
func validateGitWorkspace(endpoint, authRef string, manifest json.RawMessage) (string, bool) {
	var m struct {
		Kind          string `json:"kind"`
		DefaultBranch string `json:"default_branch"`
	}
	if len(manifest) > 0 {
		if err := json.Unmarshal(manifest, &m); err != nil {
			return "manifest must be valid JSON", false
		}
	}
	if strings.TrimSpace(m.Kind) != "git" {
		return "workspace manifest.kind must be \"git\"", false
	}
	if strings.TrimSpace(endpoint) == "" {
		return "workspace endpoint (repo URL) is required", false
	}
	if strings.TrimSpace(m.DefaultBranch) == "" {
		return "workspace manifest.default_branch is required", false
	}
	if strings.TrimSpace(authRef) == "" {
		return "workspace auth_ref (git credential reference) is required", false
	}
	return "", true
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

// getVersions serves the running component versions for the About page
// (S-130). Values come from env (SKQUAD_*_VERSION), which the Helm chart
// fills from the image tags it deploys; "unknown" means not supplied.
// S-141 adds the release commit SHA alongside the semantic versions.
func (s *Server) getVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"api_server":    s.cfg.APIServerVersion,
		"operator":      s.cfg.OperatorVersion,
		"agent_runtime": s.cfg.AgentRuntimeVersion,
		"llm_gateway":   s.cfg.LLMGatewayVersion,
		"web_ui":        s.cfg.WebUIVersion,
		"commit":        s.cfg.GitCommit,
	})
}

func (s *Server) createAIProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		BaseURL string `json:"base_url"`
		// APIKey is the pasted provider key (S-155). Write-only: stored
		// in a managed Kubernetes Secret, never echoed back.
		APIKey string `json:"api_key"`
		// LegacyAPIKeyRef keeps old clients from hard-failing; the value
		// is treated as a pasted key (wrapped into a Secret), never as a
		// caller-chosen reference.
		LegacyAPIKeyRef string `json:"api_key_ref,omitempty"`
		// WP8 (0014): legacy "default_model"/"models" (and S-128's
		// "pricing") are no longer honored on providers. decodeJSON
		// rejects unknown fields, so the deprecated keys are accepted
		// here as blank fields and discarded.
		LegacyDefaultModel json.RawMessage `json:"default_model,omitempty"`
		LegacyModels       json.RawMessage `json:"models,omitempty"`
		LegacyPricing      json.RawMessage `json:"pricing,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) || !validateRequired(w, "kind", req.Kind) || !validateRequired(w, "base_url", req.BaseURL) {
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		key = strings.TrimSpace(req.LegacyAPIKeyRef)
	}
	u := currentUser(r.Context())
	provider := &domain.AIProvider{
		Name:         strings.TrimSpace(req.Name),
		Kind:         strings.TrimSpace(req.Kind),
		BaseURL:      strings.TrimSpace(req.BaseURL),
		Status:       domain.ResourceActive,
		RegisteredBy: u.ID,
	}
	ctx := s.pendingUserAuditCtx(r, "registry.ai_provider.create", string(domain.ResAIProvider), "", "", nil)
	key = s.devFallbackProviderKey(provider, key)
	created, err := s.store.CreateAIProvider(ctx, provider)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if key != "" {
		updated, ok := s.persistProviderKeyCreate(w, ctx, created, key)
		if !ok {
			return
		}
		created = updated
	}
	writeJSON(w, http.StatusCreated, providerJSON(created))
}

// devFallbackProviderKey keeps out-of-cluster dev working: with no K8s
// Secret store configured the pasted key is stored literally (S-189
// split out of createAIProvider). Returns the key that still needs
// Secret-store persistence ("" when fully handled here).
func (s *Server) devFallbackProviderKey(provider *domain.AIProvider, key string) string {
	if key == "" || s.providerKeys != nil {
		return key
	}
	// Dev fallback (no K8s store configured): keep today's literal
	// behavior so out-of-cluster dev works. Production always has
	// the Secret store and never lands here.
	log.Printf("provider %q: no Kubernetes secret store configured — storing the API key literally (dev mode only)", provider.Name)
	provider.APIKeyRef = key
	provider.APIKeyMask = maskProviderKey(key)
	return ""
}

// persistProviderKeyCreate stores the provider key in the managed Secret
// store right after row creation, rolling the row back if the Secret
// write fails (never leave a provider row whose key failed to land).
func (s *Server) persistProviderKeyCreate(w http.ResponseWriter, ctx context.Context, created *domain.AIProvider, key string) (*domain.AIProvider, bool) {
	if err := s.setProviderKey(ctx, created, key); err != nil {
		// Compensate: never leave a provider row whose key failed to
		// land in the Secret store.
		if delErr := s.store.DeleteAIProvider(ctx, created.ID); delErr != nil {
			log.Printf("provider %s: rollback after secret failure: %v", created.ID, delErr)
		}
		writeError(w, http.StatusBadGateway, "provider_key_store_failed", "could not store the provider key as a Kubernetes Secret")
		return nil, false
	}
	updated, err := s.store.UpdateAIProvider(ctx, created)
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	return updated, true
}

func (s *Server) listAIProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListAIProviders(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, providerJSONList(providers))
}

func (s *Server) getAIProvider(w http.ResponseWriter, r *http.Request) {
	provider, err := s.store.GetAIProvider(r.Context(), chi.URLParam(r, "providerID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, providerJSON(provider))
}

func (s *Server) updateAIProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	provider, err := s.store.GetAIProvider(r.Context(), chi.URLParam(r, "providerID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	var req updateAIProviderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !applyProviderFieldUpdates(w, provider, &req) {
		return
	}
	if !s.applyProviderKeyUpdate(w, r, provider, &req) {
		return
	}
	updated, err := s.store.UpdateAIProvider(s.pendingUserAuditCtx(r, "registry.ai_provider.update", string(domain.ResAIProvider), provider.ID, "", nil), provider)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, providerJSON(updated))
}

// updateAIProviderRequest is the JSON body for PUT /registry/ai-providers/{id}.
type updateAIProviderRequest struct {
	Name    *string `json:"name"`
	Kind    *string `json:"kind"`
	BaseURL *string `json:"base_url"`
	// APIKey (S-155): omitted = keep the stored key; non-empty =
	// replace it; empty string = clear the key and delete the
	// managed Secret.
	APIKey *string `json:"api_key"`
	// LegacyAPIKeyRef (S-155): a cached old web bundle still sends
	// the previously-visible value here — which was the literal key.
	// Treat it as a pasted key, never as a caller-chosen reference.
	LegacyAPIKeyRef *string `json:"api_key_ref"`
	// WP8 (0014): legacy "default_model"/"models" accepted-and-
	// discarded on update too (see create for why).
	LegacyDefaultModel json.RawMessage `json:"default_model,omitempty"`
	LegacyModels       json.RawMessage `json:"models,omitempty"`
	LegacyPricing      json.RawMessage `json:"pricing,omitempty"`
}

// applyProviderFieldUpdates validates and stages the optional scalar
// fields of a provider update (S-189 split out of updateAIProvider).
func applyProviderFieldUpdates(w http.ResponseWriter, provider *domain.AIProvider, req *updateAIProviderRequest) bool {
	if req.Name != nil {
		if !validateName(w, *req.Name) {
			return false
		}
		provider.Name = strings.TrimSpace(*req.Name)
	}
	if req.Kind != nil {
		if !validateRequired(w, "kind", *req.Kind) {
			return false
		}
		provider.Kind = strings.TrimSpace(*req.Kind)
	}
	if req.BaseURL != nil {
		if !validateRequired(w, "base_url", *req.BaseURL) {
			return false
		}
		provider.BaseURL = strings.TrimSpace(*req.BaseURL)
	}
	return true
}

// applyProviderKeyUpdate handles the S-155 key semantics on update:
// omitted keeps the stored key, non-empty replaces it, empty clears the
// key and deletes the managed Secret.
func (s *Server) applyProviderKeyUpdate(w http.ResponseWriter, r *http.Request, provider *domain.AIProvider, req *updateAIProviderRequest) bool {
	if req.APIKey == nil && req.LegacyAPIKeyRef == nil {
		return true
	}
	key := ""
	if req.APIKey != nil {
		key = strings.TrimSpace(*req.APIKey)
	} else if req.LegacyAPIKeyRef != nil {
		key = strings.TrimSpace(*req.LegacyAPIKeyRef)
	}
	switch {
	case key == "":
		s.clearProviderKey(r.Context(), provider)
	case s.providerKeys == nil:
		log.Printf("provider %s: no Kubernetes secret store configured — storing the API key literally (dev mode only)", provider.ID)
		provider.APIKeyRef = key
		provider.APIKeyMask = maskProviderKey(key)
	default:
		if err := s.setProviderKey(r.Context(), provider, key); err != nil {
			writeError(w, http.StatusBadGateway, "provider_key_store_failed", "could not store the provider key as a Kubernetes Secret")
			return false
		}
	}
	return true
}

func (s *Server) deprecateAIProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	providerID := chi.URLParam(r, "providerID")
	if err := s.store.DeprecateAIProvider(s.pendingUserAuditCtx(r, "registry.ai_provider.deprecate", string(domain.ResAIProvider), providerID, "", nil), providerID); err != nil {
		writeStorageError(w, err)
		return
	}
	// Best-effort: converge the virtual keys of every agent granted this
	// provider so deprecation does not leave models reachable. The
	// reconcile endpoint repairs anything this misses.
	s.syncAgentsWithAIProvider(r.Context(), providerID)
	w.WriteHeader(http.StatusNoContent)
}

// syncAgentsWithAIProvider re-converges gateway keys for all agents holding a
// permission on the given AI provider. Returns an action summary.
func (s *Server) syncAgentsWithAIProvider(ctx context.Context, providerID string) map[string]any {
	counts := map[string]int{"checked": 0, "errors": 0}
	agents, err := s.store.ListAllAgents(ctx)
	if err != nil {
		counts["errors"]++
		return toAnyMap(counts)
	}
	for _, agent := range agents {
		perms, err := s.store.ListAgentPermissions(ctx, agent.ID)
		if err != nil {
			counts["errors"]++
			continue
		}
		if !permissionsGrantProvider(perms, providerID) {
			continue
		}
		counts["checked"]++
		action, err := s.syncAgentGatewayKey(ctx, agent)
		if err != nil {
			counts["errors"]++
			continue
		}
		if action != "none" {
			counts[action]++
		}
	}
	return toAnyMap(counts)
}

// permissionsGrantProvider reports whether any permission grants the given
// AI provider resource.
func permissionsGrantProvider(perms []*domain.AgentPermission, providerID string) bool {
	for _, perm := range perms {
		if perm.ResourceType == domain.ResAIProvider && perm.ResourceID == providerID {
			return true
		}
	}
	return false
}

func toAnyMap(in map[string]int) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// reconcileGatewayKeys converges every agent's gateway virtual key with its
// current grants. Idempotent; safe to re-run after partial failures.
func (s *Server) reconcileGatewayKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	agents, err := s.store.ListAllAgents(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	summary := map[string]int{"checked": len(agents), "provisioned": 0, "updated": 0, "revoked": 0, "adopted": 0, "none": 0}
	var failures []map[string]string
	for _, agent := range agents {
		action, err := s.syncAgentGatewayKey(r.Context(), agent)
		if err != nil {
			failures = append(failures, map[string]string{"agent_id": agent.ID, "error": err.Error()})
			continue
		}
		summary[action]++
	}
	summary["errors"] = len(failures)
	out := toAnyMap(summary)
	if len(failures) > 0 {
		out["failures"] = failures
	}
	metadata, _ := json.Marshal(out)
	s.recordUserAudit(r, "gateway.keys.reconcile", "gateway", "", "", metadata)
	writeJSON(w, http.StatusOK, out)
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
	if typ == domain.ResProjectWorkspace {
		if msg, ok := validateGitWorkspace(req.Endpoint, req.AuthRef, req.Manifest); !ok {
			writeError(w, http.StatusBadRequest, "bad_request", msg)
			return
		}
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
	created, err := s.store.CreateResource(s.pendingUserAuditCtx(r, "registry.resource.create", string(typ), "", "", nil), resource)
	if err != nil {
		writeStorageError(w, err)
		return
	}
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
	updated, err := s.store.UpdateResource(s.pendingUserAuditCtx(r, "registry.resource.update", string(typ), resource.ID, "", nil), resource)
	if err != nil {
		writeStorageError(w, err)
		return
	}
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
	if err := s.store.DeprecateResource(s.pendingUserAuditCtx(r, "registry.resource.deprecate", string(typ), chi.URLParam(r, "resourceID"), "", nil), typ, chi.URLParam(r, "resourceID")); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteUsage describes one agent that holds a grant on a resource being
// deleted; surfaced to the UI so the operator sees the blast radius (S-103).
type deleteUsage struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	SquadID   string `json:"squad_id"`
}

// resourceUsage lists the agents currently granted the given resource.
func (s *Server) resourceUsage(ctx context.Context, typ domain.ResourceType, resourceID string) ([]deleteUsage, error) {
	perms, err := s.store.ListPermissionsByResource(ctx, typ, resourceID)
	if err != nil {
		return nil, err
	}
	usage := make([]deleteUsage, 0, len(perms))
	for _, perm := range perms {
		agent, err := s.store.GetAgent(ctx, perm.AgentID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return nil, err
		}
		usage = append(usage, deleteUsage{AgentID: agent.ID, AgentName: agent.Name, SquadID: agent.SquadID})
	}
	return usage, nil
}

// deleteAIProvider hard-deletes a provider (S-103). While any agent still
// holds a grant on it, the delete is refused with 409 + the usage list so
// the UI can warn; force=true deletes and revokes those grants.
func (s *Server) deleteAIProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	providerID := chi.URLParam(r, "providerID")
	force := r.URL.Query().Get("force") == "true"
	if !force {
		usage, err := s.resourceUsage(r.Context(), domain.ResAIProvider, providerID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if len(usage) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "in_use",
				"message": fmt.Sprintf("provider is granted to %d agent(s); retry with force to delete it and revoke those grants", len(usage)),
				"usage":   usage,
			})
			return
		}
	}
	if err := s.store.DeleteAIProvider(s.pendingUserAuditCtx(r, "registry.ai_provider.delete", string(domain.ResAIProvider), providerID, "", nil), providerID); err != nil {
		writeStorageError(w, err)
		return
	}
	// S-155: drop the managed key Secret with the provider (best-effort —
	// an orphan Secret is harmless and GC-able by label).
	if s.providerKeys != nil {
		if err := s.providerKeys.DeleteProviderKey(r.Context(), providerSecretName(providerID)); err != nil {
			log.Printf("provider %s: delete managed key secret: %v", providerID, err)
		}
	}
	// Best-effort convergence of gateway keys after the provider is gone.
	s.syncAgentsWithAIProvider(r.Context(), providerID)
	w.WriteHeader(http.StatusNoContent)
}

// deleteRegistryResource hard-deletes a registry resource (S-103) with the
// same in-use warning semantics as deleteAIProvider.
func (s *Server) deleteRegistryResource(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	typ, ok := registryTypeFromRequest(w, r)
	if !ok {
		return
	}
	resourceID := chi.URLParam(r, "resourceID")
	force := r.URL.Query().Get("force") == "true"
	if !force {
		usage, err := s.resourceUsage(r.Context(), typ, resourceID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if len(usage) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "in_use",
				"message": fmt.Sprintf("resource is granted to %d agent(s); retry with force to delete it and revoke those grants", len(usage)),
				"usage":   usage,
			})
			return
		}
	}
	if err := s.store.DeleteResource(s.pendingUserAuditCtx(r, "registry.resource.delete", string(typ), resourceID, "", nil), typ, resourceID); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createSquad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string          `json:"name"`
		Mission        string          `json:"mission"`
		Prompt         string          `json:"prompt"`
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
	// S-158: an optional squad prompt (optionally pre-populated from a
	// prompt template) is accepted at creation, with the same squad-tier
	// validation as the squad prompt editor.
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt != "" {
		if _, _, failure := checkPromptDraft(promptcompo.TierSquad, req.Prompt); failure != nil {
			writePromptFailure(w, failure)
			return
		}
	}
	// S-179: the mission reaches every agent's composed system prompt, so
	// it gets the same save-time validation as the Squad Context text.
	if _, _, failure := checkPromptDraft(promptcompo.TierSquad, req.Mission); failure != nil {
		writePromptFailure(w, failure)
		return
	}
	if len(req.OperatingModel) == 0 {
		req.OperatingModel = json.RawMessage(`{}`)
	}

	u := currentUser(r.Context())
	// S-156: a user may not create two squads with the same name (other
	// users may reuse it — the owner segment keeps the namespace distinct).
	if existing, err := s.store.GetSquadByName(r.Context(), u.ID, req.Name); err != nil && !errors.Is(err, storage.ErrNotFound) {
		writeStorageError(w, err)
		return
	} else if existing != nil {
		writeError(w, http.StatusConflict, "name_taken", "you already have a squad named "+existing.Name+"; squad names must be unique per user")
		return
	}
	// S-156: namespace is skquad-<owner>-<squad> and is fixed at creation
	// (K8s namespaces cannot be renamed).
	namespace, err := s.uniqueSquadNamespace(r.Context(), squadNamespaceFor(ownerSlug(u), req.Name))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to allocate squad namespace")
		return
	}
	squad := &domain.Squad{
		Name:           req.Name,
		Mission:        req.Mission,
		Prompt:         req.Prompt,
		OperatingModel: req.OperatingModel,
		OwnerID:        u.ID,
		Namespace:      namespace,
		Status:         domain.SquadActive,
	}
	createCtx := s.pendingUserAuditCtx(r, "squad.create", "squad", "", "", nil)
	if req.Prompt != "" {
		createCtx = storage.WithPromptRevision(createCtx, storage.PromptRevisionIntent{
			Scope:   domain.PromptScopeSquad,
			ScopeID: "", // store fills the new squad id into the revision
			SavedBy: u.ID,
		})
	}
	created, err := s.store.CreateSquad(createCtx, squad)
	if err != nil {
		writeStorageError(w, err)
		return
	}
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
	// S-133: platform admins may edit any squad, not only the owner.
	squad, ok := s.loadOwnedOrAdminSquad(w, r)
	if !ok {
		return
	}

	var req struct {
		Name           *string          `json:"name"`
		Mission        *string          `json:"mission"`
		Prompt         *string          `json:"prompt"`
		OperatingModel *json.RawMessage `json:"operating_model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.applySquadNameUpdate(w, squad, req.Name) {
		return
	}
	if !s.applySquadMissionUpdate(w, squad, req.Mission) {
		return
	}
	// S-PROMPT WP2: the squad tier gets sanitize + template-var + budget
	// validation, and the save carries a revision intent so the history row
	// commits with the squad update.
	promptIntent, ok := s.applySquadPromptUpdate(w, r, squad, req.Prompt)
	if !ok {
		return
	}
	if req.OperatingModel != nil {
		squad.OperatingModel = *req.OperatingModel
	}

	ctx := s.pendingUserAuditCtx(r, "squad.update", "squad", squad.ID, squad.ID, nil)
	if promptIntent != nil {
		ctx = storage.WithPromptRevision(ctx, *promptIntent)
	}
	updated, err := s.store.UpdateSquad(ctx, squad)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// applySquadNameUpdate validates an optional squad rename (S-156: names
// are immutable because the K8s namespace is derived from them).
// Returns false when an error response was written.
func (s *Server) applySquadNameUpdate(w http.ResponseWriter, squad *domain.Squad, raw *string) bool {
	if raw == nil {
		return true
	}
	name := strings.TrimSpace(*raw)
	if name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name must not be empty")
		return false
	}
	// S-156: the K8s namespace is derived from the squad name, so a
	// rename would desync the squad from its namespace. Names are
	// immutable; sending the same name back is a no-op.
	if !strings.EqualFold(name, squad.Name) {
		writeError(w, http.StatusBadRequest, "name_immutable", "squad name cannot be changed after creation (it is bound to the Kubernetes namespace "+squad.Namespace+")")
		return false
	}
	return true
}

// applySquadMissionUpdate validates and stages the optional mission
// change (S-179: same save-time prompt validation as the Squad Context
// text, since the mission is injected into the composed squad tier).
func (s *Server) applySquadMissionUpdate(w http.ResponseWriter, squad *domain.Squad, raw *string) bool {
	if raw == nil {
		return true
	}
	if _, _, failure := checkPromptDraft(promptcompo.TierSquad, *raw); failure != nil {
		writePromptFailure(w, failure)
		return false
	}
	squad.Mission = *raw
	return true
}

// applySquadPromptUpdate validates and stages the optional squad prompt
// change, returning the revision intent to commit alongside the update.
func (s *Server) applySquadPromptUpdate(w http.ResponseWriter, r *http.Request, squad *domain.Squad, raw *string) (*storage.PromptRevisionIntent, bool) {
	if raw == nil {
		return nil, true
	}
	prompt := strings.TrimSpace(*raw)
	if _, _, failure := checkPromptDraft(promptcompo.TierSquad, prompt); failure != nil {
		writePromptFailure(w, failure)
		return nil, false
	}
	squad.Prompt = prompt
	return &storage.PromptRevisionIntent{
		Scope:   domain.PromptScopeSquad,
		ScopeID: squad.ID,
		SavedBy: currentUser(r.Context()).ID,
	}, true
}

func (s *Server) deleteSquad(w http.ResponseWriter, r *http.Request) {
	// S-133: platform admins may delete any squad (orphan cleanup), not only the owner.
	squad, ok := s.loadOwnedOrAdminSquad(w, r)
	if !ok {
		return
	}
	agents, err := s.store.ListAgents(r.Context(), squad.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	identities, err := s.collectAgentIdentities(r.Context(), agents)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	// Revoke live gateway keys before the squad rows disappear so no
	// untracked key survives the delete. S-222: best-effort — a gateway
	// outage must not block squad deletion (see deleteAgent).
	s.revokeLiveGatewayKeys(r.Context(), identities)
	if err := s.store.DeleteSquad(s.pendingUserAuditCtx(r, "squad.delete", "squad", squad.ID, squad.ID, nil), squad.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	for _, identity := range identities {
		_ = s.crWriter.DeleteAgentCredential(r.Context(), identity.CredentialRef)
		_ = s.crWriter.DeleteAgentCredential(r.Context(), identity.VirtualKeyRef)
	}
	w.WriteHeader(http.StatusNoContent)
}

// collectAgentIdentities gathers the identity rows for the given agents,
// skipping agents that have no identity yet.
func (s *Server) collectAgentIdentities(ctx context.Context, agents []*domain.Agent) ([]*domain.AgentIdentity, error) {
	identities := make([]*domain.AgentIdentity, 0, len(agents))
	for _, agent := range agents {
		identity, err := s.store.GetAgentIdentity(ctx, agent.ID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, nil
}

// revokeLiveGatewayKeys revokes the active gateway virtual keys held by
// the given identities. S-222: revocation is best-effort — failures are
// logged (with the identity id for later reconciliation) and never abort
// the surrounding delete flow.
func (s *Server) revokeLiveGatewayKeys(ctx context.Context, identities []*domain.AgentIdentity) {
	for _, identity := range identities {
		if identity.GatewayKeyStatus == domain.GatewayKeyActive && identity.GatewayKeyToken != "" {
			if err := s.llmGateway.RevokeAgentKey(ctx, identity.GatewayKeyToken); err != nil {
				slog.Warn("squad delete: llm gateway key revoke failed (continuing with delete)",
					"agent_id", identity.AgentID, "identity_id", identity.ID, "error", err)
			}
		}
	}
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

// listSquadWakeLatency returns wake-path latency events (S-87) for a squad,
// newest-first, with a nearest-rank percentile summary over e2e_ms — the
// SLO surface (target p95 < 20s). Query: since=RFC3339 (default 7 days),
// limit (default 500, max 2000).
func (s *Server) listSquadWakeLatency(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "since must be RFC3339")
			return
		}
		since = parsed.UTC()
	}
	limit := 500
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		if parsed > 2000 {
			parsed = 2000
		}
		limit = parsed
	}
	events, err := s.store.ListWakeLatency(r.Context(), squad.ID, since, limit)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since":   since,
		"events":  events,
		"summary": summarizeWakeLatency(events),
	})
}

// wakeLatencySummary is the aggregated SLO view over a set of wake events.
type wakeLatencySummary struct {
	Count           int     `json:"count"`
	ColdStarts      int     `json:"cold_starts"`
	P50E2EMs        float64 `json:"p50_e2e_ms"`
	P95E2EMs        float64 `json:"p95_e2e_ms"`
	P99E2EMs        float64 `json:"p99_e2e_ms"`
	MaxE2EMs        float64 `json:"max_e2e_ms"`
	P95QueueMs      float64 `json:"p95_queue_ms"`
	P95ScaleupMs    float64 `json:"p95_scaleup_ms"`
	P95ClaimDelayMs float64 `json:"p95_claim_delay_ms"`
	SloTargetMs     float64 `json:"slo_target_ms"`
	SloMet          bool    `json:"slo_met"`
}

const wakeLatencySLOTargetMs = 20_000 // S-87: p95 e2e < 20s (validate/adjust after first data)

func summarizeWakeLatency(events []*domain.WakeLatencyEvent) wakeLatencySummary {
	sum := wakeLatencySummary{SloTargetMs: wakeLatencySLOTargetMs}
	if len(events) == 0 {
		return sum
	}
	e2e := make([]float64, 0, len(events))
	queue := make([]float64, 0, len(events))
	scaleup := make([]float64, 0, len(events))
	claimDelay := make([]float64, 0, len(events))
	for _, e := range events {
		e2e = append(e2e, e.E2EMs)
		queue = append(queue, e.QueueMs)
		scaleup = append(scaleup, e.ScaleupMs)
		claimDelay = append(claimDelay, e.ClaimDelayMs)
		if e.ColdStart {
			sum.ColdStarts++
		}
	}
	sum.Count = len(events)
	sum.P50E2EMs = nearestRankPercentile(e2e, 0.50)
	sum.P95E2EMs = nearestRankPercentile(e2e, 0.95)
	sum.P99E2EMs = nearestRankPercentile(e2e, 0.99)
	sum.MaxE2EMs = nearestRankPercentile(e2e, 1.0)
	sum.P95QueueMs = nearestRankPercentile(queue, 0.95)
	sum.P95ScaleupMs = nearestRankPercentile(scaleup, 0.95)
	sum.P95ClaimDelayMs = nearestRankPercentile(claimDelay, 0.95)
	sum.SloMet = sum.P95E2EMs < wakeLatencySLOTargetMs
	return sum
}

// nearestRankPercentile returns the nearest-rank percentile of values
// (ceil(p*n)-th smallest, 1-based). Empty input returns 0.
func nearestRankPercentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
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

// createAgentRequest is the POST body for createAgent.
type createAgentRequest struct {
	Name           string          `json:"name"`
	Role           string          `json:"role"`
	SystemPrompt   string          `json:"system_prompt"`
	Permissions    json.RawMessage `json:"permissions"`
	IdleTimeoutSec int             `json:"idle_timeout_sec"`
	// Storage (S-138): squad owners choose whether the agent gets a
	// durable workspace PVC and how large. storageClass is NOT accepted
	// here — platform-admin only (SKQUAD_STORAGE_CLASS).
	StorageEnabled *bool  `json:"storage_enabled"`
	StorageSize    string `json:"storage_size"`
	// S-170: optional primary model at creation. When set, the agent
	// is born bound AND with its runtime identity auto-provisioned —
	// no separate "Provision Identity" step. Omit it and the agent is
	// created unbound; identity can be provisioned later from the
	// agent screen once a model is bound.
	AIModelID string `json:"ai_model_id"`
	// WP8 (0014): legacy "default_provider_id"/"default_model" are
	// accepted-and-discarded (decodeJSON rejects unknown fields, so
	// they are declared as blank fields). Model selection is via the
	// ai_model_id binding (ADR-0010 D4).
	LegacyDefaultProviderID json.RawMessage `json:"default_provider_id,omitempty"`
	LegacyDefaultModel      json.RawMessage `json:"default_model,omitempty"`
}

// validateCreateAgentRequest validates and normalises the decoded
// createAgent body. On invalid input it writes the HTTP error and
// returns false.
func validateCreateAgentRequest(w http.ResponseWriter, req *createAgentRequest) bool {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return false
	}
	// S-PROMPT WP2: fail closed at the create door too — an agent must not
	// be born with a forged or over-cap prompt.
	req.SystemPrompt = strings.TrimSpace(req.SystemPrompt)
	if req.SystemPrompt != "" {
		if _, _, failure := checkPromptDraft(promptcompo.TierAgent, req.SystemPrompt); failure != nil {
			writePromptFailure(w, failure)
			return false
		}
	}
	if len(req.Permissions) == 0 {
		req.Permissions = json.RawMessage(`[]`)
	}
	if req.IdleTimeoutSec < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "idle_timeout_sec cannot be negative")
		return false
	}
	return true
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}
	var req createAgentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateCreateAgentRequest(w, &req) {
		return
	}
	// S-183: 0/unset means "follow the platform idle scale-to-zero
	// setting" — the deploy-time default is no longer baked into the row.
	// S-156: agent names are unique per user (across all their squads) and
	// immutable — the K8s Deployment name is derived from them.
	owner, err := s.store.GetUser(r.Context(), squad.OwnerID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if !s.ensureAgentNameFree(w, r, squad, req.Name) {
		return
	}
	storageEnabled, storageSize, ok := s.resolveCreateAgentStorage(w, req.StorageEnabled, req.StorageSize)
	if !ok {
		return
	}
	// S-170: validate the requested primary model BEFORE creating the
	// row — a bad model must not produce a half-born agent.
	requestedModel := strings.TrimSpace(req.AIModelID)
	if requestedModel != "" && !s.validateCreateAgentModel(w, r, squad.OwnerID, requestedModel) {
		return
	}

	agent := &domain.Agent{
		SquadID:        squad.ID,
		Name:           req.Name,
		Role:           req.Role,
		SystemPrompt:   strings.TrimSpace(req.SystemPrompt),
		Permissions:    req.Permissions,
		IdleTimeoutSec: req.IdleTimeoutSec,
		Status:         domain.AgentIdle,
		StorageEnabled: storageEnabled,
		StorageSize:    storageSize,
		AIModelID:      requestedModel,
		// S-156: deterministic K8s Deployment name, fixed at creation.
		DeploymentName: agentDeploymentNameFor(ownerSlug(owner), req.Name),
	}
	createCtx := s.pendingUserAuditCtx(r, "agent.create", "agent", "", squad.ID, nil)
	if agent.SystemPrompt != "" {
		createCtx = storage.WithPromptRevision(createCtx, storage.PromptRevisionIntent{
			Scope:   domain.PromptScopeAgent,
			ScopeID: "", // store fills the new agent id into the revision
			SavedBy: currentUser(r.Context()).ID,
		})
	}
	created, err := s.store.CreateAgent(createCtx, agent)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	// S-170: auto-provision the runtime identity when a primary model
	// was supplied. Creation is atomic from the caller's point of view:
	// if provisioning fails the agent row is rolled back so no agent
	// exists without the identity it can never use.
	if requestedModel != "" && !s.provisionIdentityOrRollback(w, r, created, squad) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// provisionIdentityOrRollback auto-provisions the runtime identity for
// a freshly created agent. On failure the agent row is rolled back and
// the HTTP error is written (returns false).
func (s *Server) provisionIdentityOrRollback(w http.ResponseWriter, r *http.Request, created *domain.Agent, squad *domain.Squad) bool {
	_, err := s.provisionAgentIdentity(s.pendingUserAuditCtx(r, "agent_identity.create", "agent_identity", "", squad.ID, nil), created, squad, currentUser(r.Context()).ID)
	if err == nil {
		return true
	}
	rollbackCtx := s.pendingUserAuditCtx(r, "agent.create_rollback", "agent", created.ID, squad.ID, json.RawMessage(fmt.Sprintf(`{"reason":%q}`, err.Error())))
	_ = s.store.DeleteAgent(rollbackCtx, created.ID)
	writeIdentityProvisionError(w, err)
	return false
}

// ensureAgentNameFree enforces S-156 name uniqueness (per user, across
// squads) for a new agent, writing the conflict response when taken.
func (s *Server) ensureAgentNameFree(w http.ResponseWriter, r *http.Request, squad *domain.Squad, name string) bool {
	existing, err := s.store.GetAgentByNameForOwner(r.Context(), squad.OwnerID, name)
	switch {
	case err != nil && !errors.Is(err, storage.ErrNotFound):
		writeStorageError(w, err)
		return false
	case existing == nil:
		return true
	}
	where := "another squad"
	if existing.SquadID == squad.ID {
		where = "this squad"
	}
	writeError(w, http.StatusConflict, "name_taken", "you already have an agent named "+existing.Name+" in "+where+"; agent names must be unique per user")
	return false
}

// resolveCreateAgentStorage validates the requested storage size and
// applies the platform default when storage is enabled without a size
// (S-189 split out of createAgent).
func (s *Server) resolveCreateAgentStorage(w http.ResponseWriter, enabledPtr *bool, rawSize string) (bool, string, bool) {
	storageEnabled := enabledPtr != nil && *enabledPtr
	storageSize := strings.TrimSpace(rawSize)
	if storageSize != "" {
		if err := s.validateStorageSize(storageSize); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return false, "", false
		}
	}
	if storageEnabled && storageSize == "" {
		storageSize = s.cfg.DefaultAgentStorageSize
	}
	return storageEnabled, storageSize, true
}

// validateCreateAgentModel checks the requested primary model against the
// owner's grants before the agent row exists (S-170: a bad model must
// not produce a half-born agent).
func (s *Server) validateCreateAgentModel(w http.ResponseWriter, r *http.Request, ownerID, requestedModel string) bool {
	granted, err := s.ownerGrantedModelIDs(r.Context(), ownerID)
	if err != nil {
		writeStorageError(w, err)
		return false
	}
	if _, err := s.resolveBindingModel(r.Context(), requestedModel, "ai_model_id", granted); err != nil {
		writeBindingError(w, err)
		return false
	}
	return true
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

// updateAgentRequest is the PATCH body for an agent (WP8 / 0014: legacy
// default_provider_id / default_model accepted-and-discarded, see agent
// create).
type updateAgentRequest struct {
	Name                    *string          `json:"name"`
	Role                    *string          `json:"role"`
	SystemPrompt            *string          `json:"system_prompt"`
	Permissions             *json.RawMessage `json:"permissions"`
	IdleTimeoutSec          *int             `json:"idle_timeout_sec"`
	LegacyDefaultProviderID json.RawMessage  `json:"default_provider_id,omitempty"`
	LegacyDefaultModel      json.RawMessage  `json:"default_model,omitempty"`
	// AI model binding (ADR-0010 D4). Pointer semantics: nil = leave
	// unchanged, "" = clear the slot, id = bind. A successful binding
	// change converges the agent's virtual key immediately (D5).
	AIModelID         *string `json:"ai_model_id"`
	FallbackAIModelID *string `json:"fallback_ai_model_id"`
	// Storage (S-138). Pointer semantics like the model bindings:
	// nil = leave unchanged. storageClass is not accepted (platform only).
	StorageEnabled *bool   `json:"storage_enabled"`
	StorageSize    *string `json:"storage_size"`
	// ThinkingLevel (S-178). Pointer semantics: nil = leave unchanged,
	// "" = unset (provider default), one of low|medium|high = set.
	ThinkingLevel *string `json:"thinking_level"`
}

// applyAgentScalarUpdates copies the non-binding scalar fields from the
// request onto the agent, returning a validation error (HTTP 400 message)
// when one is invalid. Extracted from updateAgent for cognitive
// complexity (S-126 / S3776).
func (s *Server) applyAgentScalarUpdates(agent *domain.Agent, req updateAgentRequest) error {
	// S-156: req.Name is deliberately NOT applied here — renames are
	// rejected up front in updateAgent (the Deployment name is derived
	// from the agent name).
	if req.Role != nil {
		agent.Role = *req.Role
	}
	if req.SystemPrompt != nil {
		agent.SystemPrompt = strings.TrimSpace(*req.SystemPrompt)
	}
	if req.Permissions != nil {
		agent.Permissions = *req.Permissions
	}
	if err := applyIdleTimeoutUpdate(agent, req.IdleTimeoutSec); err != nil {
		return err
	}
	if req.StorageEnabled != nil {
		agent.StorageEnabled = *req.StorageEnabled
	}
	if err := applyStorageSizeUpdate(agent, req.StorageSize, s.validateStorageSize); err != nil {
		return err
	}
	if err := applyThinkingLevelUpdate(agent, req.ThinkingLevel); err != nil {
		return err
	}
	if agent.StorageEnabled && agent.StorageSize == "" {
		agent.StorageSize = s.cfg.DefaultAgentStorageSize
	}
	return nil
}

// applyIdleTimeoutUpdate stages the per-agent idle timeout (S-183: 0
// clears the override and follows the platform setting; negative is
// invalid). S-189 split out of applyAgentScalarUpdates.
func applyIdleTimeoutUpdate(agent *domain.Agent, v *int) error {
	if v == nil {
		return nil
	}
	if *v < 0 {
		return errors.New("idle_timeout_sec cannot be negative")
	}
	agent.IdleTimeoutSec = *v
	return nil
}

// applyStorageSizeUpdate stages the per-agent storage size, running the
// supplied validator for non-empty values (S-189 split).
func applyStorageSizeUpdate(agent *domain.Agent, v *string, validate func(string) error) error {
	if v == nil {
		return nil
	}
	size := strings.TrimSpace(*v)
	if size != "" {
		if err := validate(size); err != nil {
			return err
		}
	}
	agent.StorageSize = size
	return nil
}

// applyThinkingLevelUpdate stages the S-178 thinking level. Empty string
// unsets it (provider default). S-189 split.
func applyThinkingLevelUpdate(agent *domain.Agent, v *string) error {
	if v == nil {
		return nil
	}
	level := strings.TrimSpace(*v)
	switch level {
	case "", "low", "medium", "high":
		agent.ThinkingLevel = level
	default:
		return errors.New("thinking_level must be one of low, medium, high (or empty to unset)")
	}
	return nil
}

// validateStorageSize enforces the S-138 front-door rules: a valid
// Kubernetes quantity, positive, and within the platform maximum
// (SKQUAD_MAX_AGENT_STORAGE, default 10Gi).
func (s *Server) validateStorageSize(raw string) error {
	max := strings.TrimSpace(s.cfg.MaxAgentStorage)
	if max == "" {
		max = "10Gi"
	}
	return domain.ValidateStorageSizeWithin(raw, max)
}

// applyAgentModelBinding validates and applies the requested primary /
// fallback model change to agent, converging the gateway virtual key
// before returning true. On any failure it writes the HTTP error response
// and returns false. Extracted from updateAgent for cognitive complexity
// (S-126 / S3776).
func (s *Server) applyAgentModelBinding(w http.ResponseWriter, r *http.Request, agent *domain.Agent, req updateAgentRequest) bool {
	newPrimary := agent.AIModelID
	if req.AIModelID != nil {
		newPrimary = strings.TrimSpace(*req.AIModelID)
	}
	newFallback := agent.FallbackAIModelID
	if req.FallbackAIModelID != nil {
		newFallback = strings.TrimSpace(*req.FallbackAIModelID)
	}
	if newPrimary == "" && newFallback != "" {
		writeError(w, http.StatusBadRequest, "bad_request", "cannot keep a fallback model without a primary model")
		return false
	}
	if newFallback != "" && newFallback == newPrimary {
		// The store enforces this too (ErrConflict); surface it as a
		// clean 400 instead of leaking the constraint error.
		writeError(w, http.StatusBadRequest, "fallback_same_as_primary", "fallback_ai_model_id must differ from ai_model_id")
		return false
	}
	if !s.validateAgentBindingModels(w, r, agent.SquadID, newPrimary, newFallback) {
		return false
	}
	return s.convergeAgentBinding(w, r, agent, newPrimary, newFallback)
}

func (s *Server) validateAgentBindingModels(w http.ResponseWriter, r *http.Request, squadID, newPrimary, newFallback string) bool {
	squad, err := s.store.GetSquad(r.Context(), squadID)
	if err != nil {
		writeStorageError(w, err)
		return false
	}
	granted, err := s.ownerGrantedModelIDs(r.Context(), squad.OwnerID)
	if err != nil {
		writeStorageError(w, err)
		return false
	}
	type bindingSlot struct {
		id    string
		field string
	}
	for _, slot := range []bindingSlot{{newPrimary, "ai_model_id"}, {newFallback, "fallback_ai_model_id"}} {
		if slot.id == "" {
			continue
		}
		if _, err := s.resolveBindingModel(r.Context(), slot.id, slot.field, granted); err != nil {
			writeBindingError(w, err)
			return false
		}
	}
	return true
}

func (s *Server) convergeAgentBinding(w http.ResponseWriter, r *http.Request, agent *domain.Agent, newPrimary, newFallback string) bool {
	// Converge the virtual key BEFORE persisting so a gateway failure
	// aborts the mutation with the previous binding intact (same
	// ordering discipline the permission-set path used).
	prevPrimary, prevFallback := agent.AIModelID, agent.FallbackAIModelID
	agent.AIModelID, agent.FallbackAIModelID = newPrimary, newFallback
	if _, err := s.syncAgentGatewayKey(r.Context(), agent); err != nil {
		agent.AIModelID, agent.FallbackAIModelID = prevPrimary, prevFallback
		if writeBindingError(w, err) {
			return false
		}
		writeError(w, http.StatusBadGateway, "llm_gateway_unavailable", "failed to converge LLM gateway virtual key")
		return false
	}
	return true
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	// S-133: platform admins may edit any agent, not only the owner.
	agent, ok := s.loadOwnedOrAdminAgent(w, r)
	if !ok {
		return
	}
	var req updateAgentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateAgentNameImmutable(w, agent, req.Name) {
		return
	}
	// S-PROMPT WP2: the agent tier gets the same sanitize + template-var +
	// budget battery as the other editable tiers before anything is applied.
	if req.SystemPrompt != nil {
		if _, _, failure := checkPromptDraft(promptcompo.TierAgent, strings.TrimSpace(*req.SystemPrompt)); failure != nil {
			writePromptFailure(w, failure)
			return
		}
	}
	// Captured before any mutation so a post-gateway persist failure can
	// roll the virtual key back to the pre-request binding.
	prevPrimary, prevFallback := agent.AIModelID, agent.FallbackAIModelID
	if err := s.applyAgentScalarUpdates(agent, req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	bindingTouched := false
	if req.AIModelID != nil || req.FallbackAIModelID != nil {
		if !s.applyAgentModelBinding(w, r, agent, req) {
			return
		}
		bindingTouched = true
	}

	agentCtx := s.pendingUserAuditCtx(r, "agent.update", "agent", agent.ID, agent.SquadID, nil)
	if req.SystemPrompt != nil {
		agentCtx = storage.WithPromptRevision(agentCtx, storage.PromptRevisionIntent{
			Scope:   domain.PromptScopeAgent,
			ScopeID: agent.ID,
			SavedBy: currentUser(r.Context()).ID,
		})
	}
	updated, err := s.store.UpdateAgent(agentCtx, agent)
	if err != nil {
		// If the binding converged at the gateway but the row failed to
		// persist, roll the key back to the previous binding best-effort;
		// the reconcile endpoint is the final repair path either way.
		if bindingTouched {
			agent.AIModelID, agent.FallbackAIModelID = prevPrimary, prevFallback
			_, _ = s.syncAgentGatewayKey(r.Context(), agent)
		}
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// validateAgentNameImmutable enforces S-156: the K8s Deployment name is
// derived from the agent name, so renames are rejected. Sending the same
// name back is a no-op.
func validateAgentNameImmutable(w http.ResponseWriter, agent *domain.Agent, raw *string) bool {
	if raw == nil {
		return true
	}
	name := strings.TrimSpace(*raw)
	if name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name must not be empty")
		return false
	}
	if !strings.EqualFold(name, agent.Name) {
		writeError(w, http.StatusBadRequest, "name_immutable", "agent name cannot be changed after creation (it is bound to the Kubernetes deployment "+agent.DeploymentName+")")
		return false
	}
	return true
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	// S-133: platform admins may delete any agent (orphan cleanup), not only the owner.
	agent, ok := s.loadOwnedOrAdminAgent(w, r)
	if !ok {
		return
	}
	// Capture the credential refs before the row (and its cascaded identity
	// rows) disappears, so the Kubernetes secrets can be cleaned up after.
	identity, err := s.store.GetAgentIdentity(r.Context(), agent.ID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		writeStorageError(w, err)
		return
	}
	// Revoke the live gateway key before the identity row disappears so no
	// untracked key survives the delete. S-222: revocation is best-effort —
	// a gateway hiccup must not block agent deletion. The credential refs
	// are already captured above, so k8s cleanup proceeds; an orphaned
	// gateway key is a reconcile problem, not a reason to fail the user's
	// delete with 502.
	if identity != nil && identity.GatewayKeyStatus == domain.GatewayKeyActive && identity.GatewayKeyToken != "" {
		if err := s.llmGateway.RevokeAgentKey(r.Context(), identity.GatewayKeyToken); err != nil {
			slog.Warn("agent delete: llm gateway key revoke failed (continuing with delete)",
				"agent_id", agent.ID, "identity_id", identity.ID, "error", err)
		}
	}
	if err := s.store.DeleteAgent(s.pendingUserAuditCtx(r, "agent.delete", "agent", agent.ID, agent.SquadID, nil), agent.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	// Best-effort: an orphaned credential secret must not block the delete.
	if identity != nil {
		_ = s.crWriter.DeleteAgentCredential(r.Context(), identity.CredentialRef)
		_ = s.crWriter.DeleteAgentCredential(r.Context(), identity.VirtualKeyRef)
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxInboxMessageChars caps one inbox message body (S-229: raised from
// 2000 to 10000 so agents can deliver substantial reports to the owner).
// The runtime tool defaults (send_inbox, notify_owner, send_message) and
// the seeded builtin-tool policies mirror this value — change them together.
const maxInboxMessageChars = 10000

const (
	maxInboxSubjectChars = 300
	maxInboxBodyChars    = 4000
)

// maxTaskThreadTurnChars caps one streamed working turn (S-183). Working
// narration is longer than an inbox ping but must not flood the thread or
// the messages table with megabyte "turns".
const maxTaskThreadTurnChars = 4000

// appendTaskThreadEvent writes a thread-only message for a task lifecycle
// event (S-181): created/assigned by the user, agent completion, agent
// block. The row is addressed to the task's assignee with task_id in the
// payload so the task-page thread (ListAgentMessageHistory filtered by
// task_id) renders it, and it is stored as DELIVERED so it never enters
// the agent's pending inbox — the agent learns about the task through the
// task wake path, not by re-processing its own thread. Best-effort: a
// failure is logged, never surfaced to the caller.
func (s *Server) appendTaskThreadEvent(ctx context.Context, task *domain.Task, fromType, fromID string, messageType domain.MessageType, text string) {
	if task == nil || task.AssigneeAgentID == "" {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	payload, err := json.Marshal(map[string]string{"message": text, "task_id": task.ID})
	if err != nil {
		return
	}
	if _, err := s.store.CreateMessage(ctx, &domain.Message{
		FromType:  fromType,
		FromID:    fromID,
		ToAgentID: task.AssigneeAgentID,
		SquadID:   task.SquadID,
		Type:      messageType,
		Payload:   payload,
		Status:    domain.MessageDelivered,
	}); err != nil {
		slog.Warn("task thread event failed", "task_id", task.ID, "from_type", fromType, "error", err)
	}
}

// notifyDelegationResult closes the loop for a task that was materialized from
// a delegate/handoff message: the requesting agent receives a reply carrying
// the completion summary, and the requesting squad's owner receives an inbox
// notification. Best-effort: failures never roll back the completion itself.
func (s *Server) notifyDelegationResult(ctx context.Context, task *domain.Task, completingAgent *domain.Agent, status, summary string) {
	if task.OriginMessageID == "" || task.CreatedByType != "agent" || task.CreatedByID == "" {
		return
	}
	source, err := s.store.GetAgent(ctx, task.CreatedByID)
	if err != nil || source.ID == completingAgent.ID {
		return
	}
	text := fmt.Sprintf("Delegated task %q finished with status %s. Summary: %s", task.Title, status, strings.TrimSpace(summary))
	payload, err := json.Marshal(map[string]string{"message": text, "task_id": task.ID, "task_status": status})
	if err != nil {
		return
	}
	if _, err := s.store.CreateMessage(ctx, &domain.Message{
		FromType:      "agent",
		FromID:        completingAgent.ID,
		ToAgentID:     source.ID,
		SquadID:       source.SquadID,
		Type:          domain.MessageReply,
		Payload:       payload,
		Status:        domain.MessagePending,
		CorrelationID: task.OriginMessageID,
	}); err == nil {
		_ = s.syncAgentStatusFromPendingWork(ctx, source.ID)
	}
	delegSubject, delegBody := taskNotifySubjectBody(task, completingAgent.Name, status, summary)
	s.notifySquadOwnerRich(ctx, ownerNotification{
		SquadID:     source.SquadID,
		Kind:        domain.InboxTaskCompleted,
		FromAgentID: completingAgent.ID,
		TaskID:      task.ID,
		Message:     fmt.Sprintf("Task %q delegated from agent %s finished with status %s", task.Title, source.Name, status),
		Subject:     delegSubject,
		Body:        delegBody,
	})
}

// notifyDelegationBlocked informs the requesting squad's owner when a
// delegated task is blocked. Best-effort.
func (s *Server) notifyDelegationBlocked(ctx context.Context, task *domain.Task, blockingAgent *domain.Agent) {
	if task.OriginMessageID == "" || task.CreatedByType != "agent" || task.CreatedByID == "" {
		return
	}
	source, err := s.store.GetAgent(ctx, task.CreatedByID)
	if err != nil || source.ID == blockingAgent.ID {
		return
	}
	delegSubject, delegBody := taskNotifySubjectBody(task, blockingAgent.Name, string(domain.TaskBlocked), task.Result)
	s.notifySquadOwnerRich(ctx, ownerNotification{
		SquadID:     source.SquadID,
		Kind:        domain.InboxActionRequired,
		FromAgentID: blockingAgent.ID,
		TaskID:      task.ID,
		Message:     fmt.Sprintf("Task %q delegated from agent %s was blocked by %s", task.Title, source.Name, blockingAgent.Name),
		Subject:     delegSubject,
		Body:        delegBody,
	})
}

// notifySquadOwner files an owner-facing inbox notification. It is best-effort
// by design: a notification failure must never fail the underlying task flow.
func (s *Server) notifySquadOwner(ctx context.Context, squadID string, kind domain.InboxKind, fromAgentID string, taskID string, message string) {
	s.notifySquadOwnerRich(ctx, ownerNotification{SquadID: squadID, Kind: kind, FromAgentID: fromAgentID, TaskID: taskID, Message: message})
}

// notifySquadOwnerRich (S-181) files an owner-facing inbox notification with
// an optional email-style subject and body alongside the legacy one-line
// message. Consumers that predate the richer payload keep reading Message;
// newer ones prefer Subject/Body. Best-effort like notifySquadOwner.
// ownerNotification carries the fields for one owner-facing inbox
// notification (S-189: params struct replaces the 8-arg signature).
type ownerNotification struct {
	SquadID     string
	Kind        domain.InboxKind
	FromAgentID string
	TaskID      string
	Message     string
	Subject     string
	Body        string
}

func (s *Server) notifySquadOwnerRich(ctx context.Context, n ownerNotification) {
	squadID, kind, fromAgentID, taskID, message, subject, body := n.SquadID, n.Kind, n.FromAgentID, n.TaskID, n.Message, n.Subject, n.Body
	squad, err := s.store.GetSquad(ctx, squadID)
	if err != nil || squad.OwnerID == "" {
		return
	}
	_, _ = s.store.CreateInboxMessage(ctx, &domain.InboxMessage{
		SquadID:     squadID,
		UserID:      squad.OwnerID,
		FromAgentID: fromAgentID,
		TaskID:      taskID,
		Kind:        kind,
		Message:     trimRunes(strings.TrimSpace(message), maxInboxMessageChars),
		Subject:     trimRunes(strings.TrimSpace(subject), maxInboxSubjectChars),
		Body:        trimRunes(strings.TrimSpace(body), maxInboxBodyChars),
	})
}

// taskNotifySubjectBody (S-181) builds the email-style subject/body for
// owner inbox notifications about a task reaching a terminal outcome.
// The subject is a one-line summary ("Task \"X\" is blocked because of
// …"), the body carries the key context plus a link to the task screen.
func taskNotifySubjectBody(task *domain.Task, agentName, status, summary string) (subject, body string) {
	oneLine := firstLine(summary, 140)
	switch status {
	case string(domain.TaskBlocked):
		if oneLine == "" {
			oneLine = "no reason was given"
		}
		subject = fmt.Sprintf("Task %q is blocked because of %s", task.Title, oneLine)
	case string(domain.TaskDone):
		if oneLine == "" {
			subject = fmt.Sprintf("Task %q completed", task.Title)
		} else {
			subject = fmt.Sprintf("Task %q completed: %s", task.Title, oneLine)
		}
	default:
		if oneLine == "" {
			subject = fmt.Sprintf("Task %q is now %s", task.Title, status)
		} else {
			subject = fmt.Sprintf("Task %q is now %s: %s", task.Title, status, oneLine)
		}
	}
	outcome := strings.TrimSpace(summary)
	if outcome == "" {
		outcome = "(no summary provided)"
	}
	taskPath := fmt.Sprintf("/squads/%s/tasks/%s", task.SquadID, task.ID)
	body = fmt.Sprintf(
		"Task: %s\nStatus: %s\nAgent: %s\nWhen: %s\n\n%s\n\nOpen the task: %s",
		task.Title, status, agentName, time.Now().UTC().Format(time.RFC3339), outcome, taskPath,
	)
	return trimRunes(subject, maxInboxSubjectChars), trimRunes(body, maxInboxBodyChars)
}

// firstLine returns the first non-empty line of text, truncated to max runes.
func firstLine(text string, max int) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimRunes(trimmed, max)
		}
	}
	return ""
}

func (s *Server) markInboxRead(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	updated, err := s.store.MarkInboxMessageRead(r.Context(), u.ID, chi.URLParam(r, "messageID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// notifyOwnerFromAgent lets an agent ask the squad owner for an action (for
// example approval to proceed). task_completed is server-emitted only.
func (s *Server) notifyOwnerFromAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	message := trimRunes(strings.TrimSpace(req.Message), maxInboxMessageChars)
	if message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", msgMessageRequired)
		return
	}
	principal := currentAgent(r.Context())
	squad, err := s.store.GetSquad(r.Context(), principal.Agent.SquadID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if squad.OwnerID == "" {
		writeError(w, http.StatusNotFound, "not_found", "squad owner not found for notification")
		return
	}
	created, err := s.store.CreateInboxMessage(s.pendingAgentAuditCtx(r, principal.Agent.ID, "inbox.notify_owner", "inbox_message", "", principal.Agent.SquadID, nil), &domain.InboxMessage{
		SquadID:     principal.Agent.SquadID,
		UserID:      squad.OwnerID,
		FromAgentID: principal.Agent.ID,
		Kind:        domain.InboxActionRequired,
		Message:     message,
	})
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "squad owner not found for notification")
		return
	}
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
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
	// S-170: the heavy lifting lives in provisionAgentIdentity so the
	// create-agent path can auto-provision too. This handler is now the
	// manual/retry door (legacy agents created before auto-provisioning).
	created, err := s.provisionAgentIdentity(s.pendingUserAuditCtx(r, "agent_identity.create", "agent_identity", "", agent.SquadID, nil), agent, squad, u.ID)
	if err != nil {
		writeIdentityProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// identityProvisionError tags which step of identity provisioning failed
// so HTTP handlers can preserve the exact status/code each step has
// always returned (S-170 refactor: the steps moved into
// provisionAgentIdentity, but their error contract did not change).
type identityProvisionError struct {
	step    string // credential | gateway | credential_secret | virtualkey_secret | store
	err     error
	message string
}

func (e *identityProvisionError) Error() string { return e.message }
func (e *identityProvisionError) Unwrap() error { return e.err }

// writeIdentityProvisionError maps a provisioning failure to the HTTP
// response the pre-refactor inline code produced.
func writeIdentityProvisionError(w http.ResponseWriter, err error) {
	var pe *identityProvisionError
	if !errors.As(err, &pe) {
		writeError(w, http.StatusInternalServerError, "internal", "identity provisioning failed")
		return
	}
	switch pe.step {
	case "gateway":
		if writeBindingError(w, pe.err) {
			return
		}
		writeError(w, http.StatusBadGateway, "llm_gateway_unavailable", msgGatewayKeyFailed)
	case "store":
		writeStorageError(w, pe.err)
	default:
		writeError(w, http.StatusInternalServerError, "internal", pe.message)
	}
}

// provisionAgentIdentity creates an agent's runtime identity end to end:
// control-plane credential, LLM gateway virtual key (allow-list derived
// from the agent's current model binding), the two projected Secrets, and
// the identity row. Partial secrets are cleaned up on failure; the caller
// decides whether to roll back the agent itself (createAgent does —
// S-170 auto-provisioning; the manual retry handler does not).
//
// The passed ctx should carry the pending audit intent for the identity
// creation so the row and its audit commit together.
func (s *Server) provisionAgentIdentity(ctx context.Context, agent *domain.Agent, squad *domain.Squad, userID string) (*domain.AgentIdentity, error) {
	identity := &domain.AgentIdentity{
		AgentID:        agent.ID,
		CredentialRef:  generatedCredentialRef(squad.Namespace, agent.ID),
		CredentialHash: "",
		VirtualKeyRef:  generatedVirtualKeyRef(squad.Namespace, agent.ID),
		CreatedBy:      userID,
	}
	credential, err := generateCredential()
	if err != nil {
		return nil, &identityProvisionError{step: "credential", err: err, message: "failed to generate agent credential"}
	}
	virtualKey, keyToken, err := s.provisionAgentVirtualKey(ctx, agent)
	if err != nil {
		return nil, &identityProvisionError{step: "gateway", err: err, message: msgGatewayKeyFailed}
	}
	identity.CredentialHash = hashCredential(credential)
	identity.GatewayKeyToken = keyToken
	identity.GatewayKeyStatus = domain.GatewayKeyActive
	if err := s.crWriter.WriteAgentCredential(ctx, identity.CredentialRef, agent.ID, credential); err != nil {
		return nil, &identityProvisionError{step: "credential_secret", err: err, message: "failed to write agent credential secret"}
	}
	if err := s.crWriter.WriteAgentCredential(ctx, identity.VirtualKeyRef, agent.ID, virtualKey); err != nil {
		_ = s.crWriter.DeleteAgentCredential(ctx, identity.CredentialRef)
		return nil, &identityProvisionError{step: "virtualkey_secret", err: err, message: "failed to write agent virtual-key secret"}
	}
	created, err := s.store.CreateAgentIdentity(ctx, identity)
	if err != nil {
		_ = s.crWriter.DeleteAgentCredential(ctx, identity.CredentialRef)
		_ = s.crWriter.DeleteAgentCredential(ctx, identity.VirtualKeyRef)
		return nil, &identityProvisionError{step: "store", err: err, message: "failed to persist agent identity"}
	}
	return created, nil
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
	virtualKey, keyToken, err := s.provisionAgentVirtualKey(r.Context(), agent)
	if err != nil {
		if writeBindingError(w, err) {
			return
		}
		writeError(w, http.StatusBadGateway, "llm_gateway_unavailable", msgGatewayKeyFailed)
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
	identity, err := s.store.RotateAgentIdentity(s.pendingUserAuditCtx(r, "agent_identity.rotate", "agent_identity", "", agent.SquadID, nil), agent.ID, credentialRef, hashCredential(credential), virtualKeyRef)
	if err != nil {
		_ = s.crWriter.DeleteAgentCredential(r.Context(), credentialRef)
		_ = s.crWriter.DeleteAgentCredential(r.Context(), virtualKeyRef)
		writeStorageError(w, err)
		return
	}
	if updated, err := s.store.SetAgentIdentityGatewayKey(r.Context(), agent.ID, keyToken, domain.GatewayKeyActive); err != nil {
		s.recordUserAudit(r, "agent_identity.gateway_key_record_stale", "agent_identity", identity.ID, agent.SquadID, nil)
	} else {
		identity = updated
	}
	_ = s.crWriter.DeleteAgentCredential(r.Context(), existing.CredentialRef)
	_ = s.crWriter.DeleteAgentCredential(r.Context(), existing.VirtualKeyRef)
	writeJSON(w, http.StatusOK, identity)
}

func (s *Server) gatewayConfigured() bool {
	return s.cfg != nil && s.cfg.LiteLLMAdminURL != "" && s.cfg.LiteLLMMasterKey != ""
}

func (s *Server) provisionAgentVirtualKey(ctx context.Context, agent *domain.Agent) (string, string, error) {
	req := GatewayKeyRequest{AgentID: agent.ID, SquadID: agent.SquadID}
	if s.gatewayConfigured() {
		models, primary, fallback, err := s.boundModelAllowList(ctx, agent)
		if err != nil {
			return "", "", err
		}
		req.Models = models
		req.PrimaryModel = primary
		req.FallbackModel = fallback
	}
	return s.llmGateway.ProvisionAgentKey(ctx, req)
}

// ownerGrantedModelIDs returns the set of AI model IDs granted to a user
// (ADR-0010 D3: grants follow people; the agent's authorisation boundary
// is its squad owner's grant set).
func (s *Server) ownerGrantedModelIDs(ctx context.Context, userID string) (map[string]bool, error) {
	grants, err := s.store.ListUserModelGrants(ctx, userID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(grants))
	for _, g := range grants {
		set[g.AIModelID] = true
	}
	return set, nil
}

// resolveBindingModel validates one bound model id: it must exist, be
// granted to the agent's owner, and still be active. field names the
// offending agent column so the error message is actionable.
func (s *Server) resolveBindingModel(ctx context.Context, modelID, field string, granted map[string]bool) (*domain.AIModel, error) {
	model, err := s.store.GetAIModel(ctx, modelID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf(errWrapFormat, errModelNotFound, field)
	}
	if err != nil {
		return nil, err
	}
	if !granted[modelID] {
		return nil, fmt.Errorf(errWrapFormat, errModelNotGranted, field)
	}
	if model.Status != domain.ResourceActive {
		return nil, fmt.Errorf(errWrapFormat, errModelDeprecated, field)
	}
	return model, nil
}

// boundModelAllowList resolves the agent's primary+fallback binding into the
// LiteLLM virtual-key allow-list (ADR-0010 D5: the grant compiles into the
// key; the runtime never re-checks grants).
//
// ⚠️ CRITICAL INVARIANT: whenever a fallback is bound, the returned allow-
// list MUST contain BOTH the primary and the fallback model names. If the
// key were provisioned with only [primary], the fallback would fail
// authorisation at the gateway at the exact moment it is needed — during a
// primary outage — turning an availability feature into an auth failure.
// Never filter the fallback out of this list (covered explicitly by
// TestBoundKeyContainsPrimaryAndFallback).
func (s *Server) boundModelAllowList(ctx context.Context, agent *domain.Agent) ([]string, string, string, error) {
	if strings.TrimSpace(agent.AIModelID) == "" {
		return nil, "", "", errModelNotBound
	}
	squad, err := s.store.GetSquad(ctx, agent.SquadID)
	if err != nil {
		return nil, "", "", err
	}
	granted, err := s.ownerGrantedModelIDs(ctx, squad.OwnerID)
	if err != nil {
		return nil, "", "", err
	}
	primary, err := s.resolveBindingModel(ctx, agent.AIModelID, "ai_model_id", granted)
	if err != nil {
		return nil, "", "", err
	}
	models := []string{primary.ModelName}
	fallbackName := ""
	if strings.TrimSpace(agent.FallbackAIModelID) != "" {
		fallback, err := s.resolveBindingModel(ctx, agent.FallbackAIModelID, "fallback_ai_model_id", granted)
		if err != nil {
			return nil, "", "", err
		}
		fallbackName = fallback.ModelName
		// Distinct IDs can still share a model name across providers; the
		// allow-list is a set of names, so only append a distinct one.
		if fallbackName != primary.ModelName {
			models = append(models, fallbackName)
		}
	}
	return models, primary.ModelName, fallbackName, nil
}

// syncAgentGatewayKey converges an agent's LiteLLM virtual key to its
// current model binding (ADR-0010 D5) in every direction: primary changed,
// fallback added, fallback removed, or fully unbound. It is idempotent and
// used by the agent-binding PATCH, the admin reconcile endpoint, and
// provider deprecation. Returns the action taken:
// "none"|"updated"|"revoked"|"provisioned".
func (s *Server) syncAgentGatewayKey(ctx context.Context, agent *domain.Agent) (string, error) {
	identity, err := s.store.GetAgentIdentity(ctx, agent.ID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "none", nil
		}
		return "", err
	}
	hasKey := identity.GatewayKeyStatus == domain.GatewayKeyActive && identity.GatewayKeyToken != ""

	models, primary, fallback, bindErr := s.boundModelAllowList(ctx, agent)
	if bindErr != nil {
		return s.convergeUnboundGatewayKey(ctx, agent, identity, hasKey, bindErr)
	}

	keyReq := GatewayKeyRequest{
		AgentID:       agent.ID,
		SquadID:       agent.SquadID,
		Models:        models,
		PrimaryModel:  primary,
		FallbackModel: fallback,
	}
	if hasKey {
		// Always rewrite the allow-list so primary changes, fallback adds
		// and fallback removals all converge (the gateway update is cheap
		// and idempotent).
		if err := s.llmGateway.UpdateAgentKey(ctx, identity.GatewayKeyToken, keyReq); err != nil {
			return "", err
		}
		return "updated", nil
	}
	squad, err := s.store.GetSquad(ctx, agent.SquadID)
	if err != nil {
		return "", err
	}
	return s.provisionOrAdoptGatewayKey(ctx, agent, squad, keyReq)
}

// provisionOrAdoptGatewayKey issues a fresh virtual key for an agent that
// has none recorded, or adopts the key that already exists at the gateway.
//
// LiteLLM enforces unique key aliases ("skquad-agent-<agentID>"). When the
// identity row shows no active token but the gateway still holds the key
// for that alias — the legacy/stuck state that triggered S-129 — a plain
// /key/generate fails with 400 "alias already exists", which previously
// aborted the binding PATCH with 502 and left the new binding
// unpersisted. Adopting the existing key (rewrite its allow-list, record
// its token) makes provisioning idempotent so the rebind succeeds.
func (s *Server) provisionOrAdoptGatewayKey(ctx context.Context, agent *domain.Agent, squad *domain.Squad, keyReq GatewayKeyRequest) (string, error) {
	alias := fmt.Sprintf("skquad-agent-%s", agent.ID)
	if existing, found, ferr := s.llmGateway.FindKeyByAlias(ctx, alias); ferr == nil && found {
		return s.adoptGatewayKey(ctx, agent, existing, keyReq)
	}
	key, token, err := s.llmGateway.ProvisionAgentKey(ctx, keyReq)
	if err != nil {
		// A concurrent provision may have created the key between the
		// pre-check and the generate; adopt it rather than failing.
		if existing, found, ferr := s.llmGateway.FindKeyByAlias(ctx, alias); ferr == nil && found {
			return s.adoptGatewayKey(ctx, agent, existing, keyReq)
		}
		return "", err
	}
	ref := generatedVirtualKeyRef(squad.Namespace, agent.ID)
	if err := s.crWriter.WriteAgentCredential(ctx, ref, agent.ID, key); err != nil {
		_ = s.llmGateway.RevokeAgentKey(ctx, token)
		return "", err
	}
	if _, err := s.store.SetAgentIdentityGatewayKey(ctx, agent.ID, token, domain.GatewayKeyActive); err != nil {
		_ = s.llmGateway.RevokeAgentKey(ctx, token)
		_ = s.crWriter.DeleteAgentCredential(ctx, ref)
		return "", err
	}
	return "provisioned", nil
}

// adoptGatewayKey converges an already-existing gateway key (identified by
// its token) onto the agent's current binding and records the token so the
// identity row stops claiming "none". The full secret is not recoverable
// from the gateway, so the k8s credential secret is left untouched — it
// already holds the runtime key from the original provisioning.
func (s *Server) adoptGatewayKey(ctx context.Context, agent *domain.Agent, token string, keyReq GatewayKeyRequest) (string, error) {
	if err := s.llmGateway.UpdateAgentKey(ctx, token, keyReq); err != nil {
		return "", err
	}
	if _, err := s.store.SetAgentIdentityGatewayKey(ctx, agent.ID, token, domain.GatewayKeyActive); err != nil {
		return "", err
	}
	return "adopted", nil
}

// convergeUnboundGatewayKey decides what to do with a gateway key when the
// agent's model binding failed. An unbound agent must not keep a live key:
// convergence here means revoke. Other binding failures (not found / not
// granted / deprecated) refuse convergence so the operator sees the precise
// error instead of a silently revoked or stale key; WP4's force cascade
// owns grant-revocation cleanup.
func (s *Server) convergeUnboundGatewayKey(ctx context.Context, agent *domain.Agent, identity *domain.AgentIdentity, hasKey bool, bindErr error) (string, error) {
	if !errors.Is(bindErr, errModelNotBound) {
		return "", bindErr
	}
	if !hasKey {
		return "none", nil
	}
	if err := s.llmGateway.RevokeAgentKey(ctx, identity.GatewayKeyToken); err != nil {
		return "", err
	}
	if _, err := s.store.SetAgentIdentityGatewayKey(ctx, agent.ID, identity.GatewayKeyToken, domain.GatewayKeyRevoked); err != nil {
		return "", err
	}
	return "revoked", nil
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

// agentPermissionRequest is one grant entry in the setAgentPermissions body.
type agentPermissionRequest struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

// buildAgentPermissions validates the requested grant entries and builds the
// deduplicated permission set for the agent. On the first invalid entry it
// writes the corresponding HTTP error and returns ok=false.
func (s *Server) buildAgentPermissions(ctx context.Context, w http.ResponseWriter, agent *domain.Agent, req []agentPermissionRequest, u *domain.User) ([]domain.AgentPermission, bool) {
	perms := make([]domain.AgentPermission, 0, len(req))
	seen := map[string]bool{}
	for _, item := range req {
		// ADR-0010 / S-107: the old door is closed. ai_provider grants are
		// replaced by user-level AI Model grants; give the operator an
		// actionable error instead of a generic invalid-type rejection.
		if strings.TrimSpace(item.ResourceType) == string(domain.ResAIProvider) {
			writeError(w, http.StatusBadRequest, "provider_not_grantable",
				"ai_provider is no longer grantable to agents; grant AI Models to the user instead (Settings \u2192 AI Models)")
			return nil, false
		}
		typ, ok := resourceTypeFromString(item.ResourceType)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "resource_type is invalid")
			return nil, false
		}
		resourceID := strings.TrimSpace(item.ResourceID)
		if resourceID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "resource_id is required")
			return nil, false
		}
		if err := s.ensureRegistryResourceExists(ctx, typ, resourceID); err != nil {
			writeStorageError(w, err)
			return nil, false
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
	return perms, true
}

func (s *Server) setAgentPermissions(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadOwnedAgent(w, r)
	if !ok {
		return
	}
	var req []agentPermissionRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	u := currentUser(r.Context())
	perms, ok := s.buildAgentPermissions(r.Context(), w, agent, req, u)
	if !ok {
		return
	}
	// ADR-0010 D5: agent permissions no longer decide the LLM allow-list —
	// the agent's model binding does. This endpoint must NOT touch the virtual
	// key; key convergence for binding changes happens in the agent PATCH
	// handler and the admin reconcile endpoint.
	metadata, _ := json.Marshal(map[string]any{"count": len(perms)})
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
		task.PromptSHA = exec.PromptSHA
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
	usage, err := s.store.SumMetering(r.Context(), squad.ID, "", time.Time{})
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
	// S-169: optional ?since=<RFC3339> restricts the sum (month-to-date
	// spend chip). Absent = lifetime, preserving the old contract.
	since := time.Time{}
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_since", "since must be an RFC3339 timestamp")
			return
		}
		since = parsed
	}
	usage, err := s.store.SumMetering(r.Context(), "", agent.ID, since)
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
	usage, err := s.store.SumMetering(r.Context(), "", "", time.Time{})
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
	// ModelUsed names the model that actually served the call when the
	// reporter knows it (WP5 / ADR-0010 Risk 3). The gateway callback
	// currently reports only the requested model, so this is optional and
	// falls back to Model at ingest.
	ModelUsed string `json:"model_used"`
	// Alert carries a gateway-raised alert signal, e.g.
	// "upstream_auth_failure" for upstream 401/403 (ADR-0010 D7: fall
	// back AND alert so a dead key never runs silently on fallback).
	Alert string `json:"alert"`
}

// validateGatewayMeteringRequest normalises and validates the required
// fields of a gateway metering callback. On failure it writes the HTTP
// error and returns false. Extracted from ingestGatewayMetering for
// cognitive complexity (S-126 / S3776).
func validateGatewayMeteringRequest(w http.ResponseWriter, req *gatewayMeteringRequest) bool {
	req.Status = strings.TrimSpace(req.Status)
	if req.Status == "" {
		req.Status = "success"
	}
	if req.Status != "success" && req.Status != "failure" {
		writeError(w, http.StatusBadRequest, "bad_request", "status must be success or failure")
		return false
	}
	if req.AgentID == "" || req.SquadID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent_id and squad_id are required")
		return false
	}
	return true
}

// recordGatewayFailureAudit records the failure audit event plus, when
// the gateway raised an alert, the dedicated upstream-auth alert event
// (ADR-0010 D7: fall back AND alert so a dead key never runs silently on
// fallback). Extracted from ingestGatewayMetering (S-126 / S3776).
func (s *Server) recordGatewayFailureAudit(ctx context.Context, req gatewayMeteringRequest, metadata []byte) {
	_ = s.recordSystemAudit(ctx, "llm.failure", "agent", req.AgentID, req.SquadID, metadata)
	if req.Alert == "" {
		return
	}
	// Loud alert per ADR-0010 D7: the gateway fell back past an
	// upstream auth failure. Dedicated audit action so it is
	// greppable independently of ordinary LLM failures.
	alertMeta, _ := json.Marshal(map[string]any{"alert": req.Alert, "error": trimRunes(req.Error, 512), "model": req.Model})
	_ = s.recordSystemAudit(ctx, "llm.upstream_auth_alert", "agent", req.AgentID, req.SquadID, alertMeta)
}

// meteringPricingSnapshot is the event-time pricing resolved for a
// metering event. When Snapshot is false the reporter-supplied cost is
// kept and no rates are recorded, so the gap stays visible rather than
// fabricated (WP5 / ADR-0010 D8 + Risk 3).
type meteringPricingSnapshot struct {
	Cost                 float64
	RateInputPer1M       *float64
	RateCachedInputPer1M *float64
	RateCacheWritePer1M  *float64
	RateOutputPer1M      *float64
	Snapshot             bool
	// ProviderID is the owning provider of the resolved AI model, used
	// to stamp metering events that arrive without provider linkage
	// (S-238). Empty when no model resolves.
	ProviderID string
}

// resolveMeteringPricing snapshots the pricing of the model that served
// the call. Extracted from ingestGatewayMetering for cognitive
// complexity (S-126 / S3776).
func (s *Server) resolveMeteringPricing(ctx context.Context, agent *domain.Agent, modelUsed string, req gatewayMeteringRequest) meteringPricingSnapshot {
	snap := meteringPricingSnapshot{Cost: req.Cost}
	if modelUsed == "" {
		return snap
	}
	model, ok := s.resolveMeteringModel(ctx, agent, modelUsed)
	if !ok {
		return snap
	}
	snap.ProviderID = model.ProviderID
	pricing, err := domain.ParseModelPricing(model.Pricing)
	if err != nil || pricing == nil {
		return snap
	}
	snap.Cost = pricing.CostFor(req.InputTokens, req.OutputTokens)
	snap.RateInputPer1M = &pricing.InputPer1M
	snap.RateCachedInputPer1M = &pricing.CachedInputPer1M
	snap.RateCacheWritePer1M = &pricing.CacheWritePer1M
	snap.RateOutputPer1M = &pricing.OutputPer1M
	snap.Snapshot = true
	return snap
}

func (s *Server) ingestGatewayMetering(w http.ResponseWriter, r *http.Request) {
	if !s.requireGatewayCallback(w, r) {
		return
	}
	var req gatewayMeteringRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateGatewayMeteringRequest(w, &req) {
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
		"model_used":    orString(strings.TrimSpace(req.ModelUsed), req.Model),
		"provider_id":   req.ProviderID,
		"task_id":       req.TaskID,
		"input_tokens":  req.InputTokens,
		"output_tokens": req.OutputTokens,
		"cost":          req.Cost,
		"currency":      defaultMeteringCurrency(req.Currency),
		"error":         trimRunes(req.Error, 512),
	})
	if req.Status == "failure" {
		s.recordGatewayFailureAudit(r.Context(), req, metadata)
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

	// WP5 / ADR-0010 D8 + Risk 3: resolve the model that served the call
	// and snapshot its pricing at event time. When a snapshot resolves, the
	// stored cost is computed FROM THE SNAPSHOT (never the reporter's
	// figure, and never re-derived from live pricing later). When no
	// snapshot resolves we keep the reporter-supplied cost and record no
	// rates, so the gap is visible rather than fabricated.
	modelUsed := strings.TrimSpace(req.ModelUsed)
	if modelUsed == "" {
		modelUsed = strings.TrimSpace(req.Model)
	}
	snap := s.resolveMeteringPricing(r.Context(), agent, modelUsed, req)

	// S-238: stamp the provider identity at WRITE time. The gateway
	// callback usually omits provider_id, which left every metering row
	// unlinked and made per-provider cost rollups depend entirely on
	// query-time registry joins. When the reporter omits it, use the
	// provider of the AI model that served the call; a reporter-supplied
	// provider_id always wins.
	providerID := strings.TrimSpace(req.ProviderID)
	if providerID == "" {
		providerID = snap.ProviderID
	}

	if err := s.store.RecordMetering(r.Context(), &domain.MeteringEvent{
		AgentID:              req.AgentID,
		SquadID:              req.SquadID,
		TaskID:               req.TaskID,
		ProviderID:           providerID,
		Model:                req.Model,
		ModelUsed:            modelUsed,
		InputTokens:          req.InputTokens,
		OutputTokens:         req.OutputTokens,
		Cost:                 snap.Cost,
		Currency:             req.Currency,
		Timestamp:            req.Timestamp,
		RateInputPer1M:       snap.RateInputPer1M,
		RateCachedInputPer1M: snap.RateCachedInputPer1M,
		RateCacheWritePer1M:  snap.RateCacheWritePer1M,
		RateOutputPer1M:      snap.RateOutputPer1M,
		RateSnapshot:         snap.Snapshot,
	}); err != nil {
		writeStorageError(w, err)
		return
	}
	_ = s.recordSystemAudit(r.Context(), "llm.metering.ingest", "agent", req.AgentID, req.SquadID, metadata)
	// S-203 WP3: every recorded cost is an enforcement evaluation point
	// for the squad owner's budget and the platform-wide limit.
	s.enforceBudgetsAfterMetering(r.Context(), req.SquadID)
	w.WriteHeader(http.StatusAccepted)
}

// resolveMeteringModel finds the AI Model row whose model_name matches the
// served model, preferring the agent's own binding (primary, then
// fallback) because the binding is the authoritative context the key was
// provisioned with. Returns ok=false when nothing matches so the caller
// records no rate snapshot instead of guessing.
func (s *Server) resolveMeteringModel(ctx context.Context, agent *domain.Agent, modelUsed string) (*domain.AIModel, bool) {
	if model, ok := s.meteringModelByBinding(ctx, agent.AIModelID, modelUsed); ok {
		return model, true
	}
	if model, ok := s.meteringModelByBinding(ctx, agent.FallbackAIModelID, modelUsed); ok {
		return model, true
	}
	models, err := s.store.ListAIModels(ctx, "")
	if err != nil {
		return nil, false
	}
	var fallbackMatch *domain.AIModel
	for _, model := range models {
		if model.ModelName != modelUsed {
			continue
		}
		if model.Status == domain.ResourceActive {
			return model, true
		}
		if fallbackMatch == nil {
			fallbackMatch = model
		}
	}
	// Prefer an active match; a deprecated model still prices honestly for
	// historical turns over recording nothing.
	return fallbackMatch, fallbackMatch != nil
}

// meteringModelByBinding resolves a bound model id (primary or fallback
// slot) to its AI Model row when the id is set and its model_name matches
// the served model. Extracted from resolveMeteringModel for cognitive
// complexity (S-126 / S3776).
func (s *Server) meteringModelByBinding(ctx context.Context, boundID, modelUsed string) (*domain.AIModel, bool) {
	id := strings.TrimSpace(boundID)
	if id == "" {
		return nil, false
	}
	model, err := s.store.GetAIModel(ctx, id)
	if err != nil || model.ModelName != modelUsed {
		return nil, false
	}
	return model, true
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
	// S-235: resolve actor display names so the timeline shows
	// "agent Bob" / "Ross" instead of GUIDs.
	decorateAuditEntries(entries, s.resolveActorDisplays(r.Context(), auditActorRefs(entries)))
	writeJSON(w, http.StatusOK, entries)
}

// auditActorRefs collects the user/agent actor references from audit
// entries (system actors are skipped — they have no name to resolve).
func auditActorRefs(entries []*domain.AuditEntry) []actorRef {
	refs := make([]actorRef, 0, len(entries))
	for _, entry := range entries {
		if entry.ActorType == "user" || entry.ActorType == "agent" {
			refs = append(refs, actorRef{kind: entry.ActorType, id: entry.ActorID})
		}
	}
	return refs
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
	var req createTaskRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "title is required")
		return
	}
	// S-213: allow creating directly into a non-default column (Backlog).
	// Without this the UI would create in the TO DO column and move afterwards, leaving
	// a race where an agent can claim the task before the move lands.
	initialStatus := req.Status
	if initialStatus == "" {
		initialStatus = domain.TaskTodo
	}
	if !initialStatus.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", msgStatusInvalid)
		return
	}
	if req.AssigneeAgentID != "" && !s.validateTaskAssignee(w, r, squad.ID, req.AssigneeAgentID) {
		return
	}
	u := currentUser(r.Context())
	task := &domain.Task{
		BoardID:         board.ID,
		SquadID:         squad.ID,
		Title:           req.Title,
		Description:     req.Description,
		Status:          initialStatus,
		AssigneeAgentID: req.AssigneeAgentID,
		CreatedByType:   "user",
		CreatedByID:     u.ID,
	}
	created, err := s.store.CreateTask(s.pendingUserAuditCtx(r, "task.create", "task", "", squad.ID, nil), task)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if created.AssigneeAgentID != "" && !s.seedTaskThreadOnCreate(w, r, created, u.ID) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// createTaskRequest is the JSON body for POST /tasks (S-189: named for
// the extracted validation helpers).
type createTaskRequest struct {
	Title           string            `json:"title"`
	Description     string            `json:"description"`
	AssigneeAgentID string            `json:"assignee_agent_id"`
	Status          domain.TaskStatus `json:"status"`
	Metadata        map[string]string `json:"metadata"`
}

// validateTaskAssignee checks the assignee exists and belongs to the
// squad, writing the error response when not (S-189 split).
func (s *Server) validateTaskAssignee(w http.ResponseWriter, r *http.Request, squadID, assigneeAgentID string) bool {
	agent, err := s.store.GetAgent(r.Context(), assigneeAgentID)
	if err != nil {
		writeStorageError(w, err)
		return false
	}
	if agent.SquadID != squadID {
		writeError(w, http.StatusBadRequest, "bad_request", "assignee_agent_id must belong to this squad")
		return false
	}
	return true
}

// seedTaskThreadOnCreate files the opening thread message for a newly
// created assigned task and syncs the assignee's pending-work status
// (S-181; S-189 split out of createTask).
func (s *Server) seedTaskThreadOnCreate(w http.ResponseWriter, r *http.Request, created *domain.Task, userID string) bool {
	// S-181: seed the task thread so the page is not empty for tasks
	// that actually ran — the creation itself is the opening message.
	threadText := created.Title
	if desc := strings.TrimSpace(created.Description); desc != "" {
		threadText = created.Title + "\n\n" + desc
	}
	s.appendTaskThreadEvent(r.Context(), created, "user", userID, domain.MessageConsult, threadText)
	if err := s.syncAgentStatusFromPendingWork(r.Context(), created.AssigneeAgentID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAssignedAgentState)
		return false
	}
	return true
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
		writeError(w, http.StatusForbidden, "forbidden", msgTaskNotAssigned)
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
	Title         string             `json:"title"`
	CorrelationID string             `json:"correlation_id"`
	MaxAttempts   int                `json:"max_attempts"`
	TTLSeconds    int                `json:"ttl_seconds"`
	// ConsultTimeoutSeconds (S-173) overrides the reply deadline for a
	// consult send. Only meaningful for type=consult; capped at 24h.
	ConsultTimeoutSeconds int `json:"consult_timeout_seconds"`
	// Attachments (S-194) are upload ids (POST /api/v1/uploads) to bind
	// to the message. Each must reference an image uploaded into the
	// message's squad; the handler normalizes them into
	// payload.attachments.
	Attachments []string `json:"attachments"`
}

type messageFailureRequest struct {
	Reason string `json:"reason"`
}

// maxCorrelationChainMessages (S-164) bounds how many messages may share one
// correlation_id. Consult/reply follow-ups between two agents keep the same
// correlation chain, so a runaway reply loop hits this ceiling instead of
// burning tokens forever. Generous for real conversations (6 full
// consult→reply round trips), loud on loops.
const maxCorrelationChainMessages = 12

// isUUID guards the correlation-chain count query so a malformed id is a
// clean 400 instead of a Postgres cast error surfacing as a 500.
func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	hex := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
	}
	for i, r := range []byte(value) {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !hex(r) {
				return false
			}
		}
	}
	return true
}

// listMySquadPeers (S-164) returns the other agents of the calling agent's
// squad — id/name/role/status only. It is the roster the runtime uses to
// resolve a send_message target by name; no prompts, credentials, or model
// configuration are exposed.
func (s *Server) listMySquadPeers(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	agents, err := s.store.ListAgents(r.Context(), principal.Agent.SquadID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	type peer struct {
		ID     string             `json:"id"`
		Name   string             `json:"name"`
		Role   string             `json:"role"`
		Status domain.AgentStatus `json:"status"`
	}
	out := make([]peer, 0, len(agents))
	for _, a := range agents {
		if a.ID == principal.Agent.ID {
			continue
		}
		out = append(out, peer{ID: a.ID, Name: a.Name, Role: a.Role, Status: a.Status})
	}
	writeJSON(w, http.StatusOK, out)
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
		writeError(w, http.StatusBadRequest, "bad_request", msgTypeInvalid)
		return
	}
	// S-PROMPT: agent-authored message content is NOT rejected for reserved
	// <skquad_ delimiters. Message content is DATA, never instructions: the
	// receiving runtime wraps it in <skquad_untrusted source="inbox"> (WP3)
	// before any LLM sees it, so forged blocks are inert there. Rejecting at
	// save broke legitimate agent communication — an agent truthfully
	// quoting its own prompt layers ("my context contains <skquad_...>")
	// got a 400 and the reply was lost. Tier-level forgery rejection stays
	// where composition actually consumes text (prompt save paths).
	target, err := s.store.GetAgent(r.Context(), targetID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if !s.agentMayMessageTarget(w, r, principal, target, messageType) {
		return
	}
	// S-164: bound the depth of a correlated conversation chain. Every
	// consult/reply/ping that continues a thread carries the same
	// correlation_id; once the chain hits the budget the send is rejected
	// and audited so a runaway agent loop fails loudly instead of looping.
	correlationID := strings.TrimSpace(req.CorrelationID)
	if correlationID != "" && !s.checkCorrelationChainBudget(w, r, principal, target, correlationID) {
		return
	}
	// Delegate and handoff messages materialize into a real task on the
	// target squad's board: the task is the durable unit of work, the
	// message becomes its delivered audit record (the target runtime is
	// woken by the task, never by the message itself).
	if messageType == domain.MessageDelegate || messageType == domain.MessageHandoff {
		s.deliverDelegatedMessage(w, r, principal, target, messageType, req)
		return
	}
	created, err := s.store.CreateMessage(s.pendingAgentAuditCtx(r, principal.Agent.ID, "message.send", "message", "", target.SquadID, nil), &domain.Message{
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
		TimeoutAt:     s.consultDeadline(messageType, req),
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), target.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateTargetAgentState)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// checkCorrelationChainBudget enforces the S-164 cap on correlated
// conversation chains: a chain at/over budget is rejected and audited so
// a runaway agent loop fails loudly instead of looping.
func (s *Server) checkCorrelationChainBudget(w http.ResponseWriter, r *http.Request, principal *agentPrincipal, target *domain.Agent, correlationID string) bool {
	if !isUUID(correlationID) {
		writeError(w, http.StatusBadRequest, "bad_request", "correlation_id must be a UUID")
		return false
	}
	chainLen, err := s.store.CountMessagesByCorrelation(r.Context(), correlationID)
	if err != nil {
		writeStorageError(w, err)
		return false
	}
	if chainLen >= maxCorrelationChainMessages {
		s.recordAgentAudit(r, principal.Agent.ID, "message.chain_exceeded", "agent", target.ID, target.SquadID, json.RawMessage(fmt.Sprintf(`{"correlation_id":%q,"chain_length":%d}`, correlationID, chainLen)))
		writeError(w, http.StatusConflict, "chain_exceeded", "correlation chain message budget exhausted")
		return false
	}
	return true
}

// materializeDelegatedTask creates the task a delegate/handoff message stands
// for: assigned to the target agent on the target squad's board, linked back
// to the originating message via origin_message_id.
func (s *Server) materializeDelegatedTask(ctx context.Context, source, target *domain.Agent, message *domain.Message) (*domain.Task, error) {
	board, err := s.store.GetBoard(ctx, target.SquadID)
	if err != nil {
		return nil, err
	}
	title := delegatedTaskTitle(message.Payload, source)
	description := delegatedTaskDescription(message.Payload, source, target, message.ID)
	return s.store.CreateTask(ctx, &domain.Task{
		BoardID:         board.ID,
		SquadID:         target.SquadID,
		Title:           title,
		Description:     description,
		Status:          domain.TaskTodo,
		AssigneeAgentID: target.ID,
		CreatedByType:   "agent",
		CreatedByID:     source.ID,
		OriginMessageID: message.ID,
	})
}

func delegatedTaskTitle(payload json.RawMessage, source *domain.Agent) string {
	var p struct {
		Title   string `json:"title"`
		Subject string `json:"subject"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &p)
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = strings.TrimSpace(p.Subject)
	}
	if title == "" {
		title = strings.TrimSpace(strings.SplitN(strings.TrimSpace(p.Message), "\n", 2)[0])
	}
	if title == "" {
		title = fmt.Sprintf("Delegated task from %s", source.Name)
	}
	return trimRunes(title, 200)
}

func delegatedTaskDescription(payload json.RawMessage, source, target *domain.Agent, messageID string) string {
	var p struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &p)
	body := strings.TrimSpace(p.Message)
	if body == "" {
		body = "(no description provided)"
	}
	origin := fmt.Sprintf("\n\n---\nDelegated by agent %s (%s) to %s via message %s",
		source.Name, source.ID, target.Name, messageID)
	return trimRunes(body+origin, maxAgentMemoryContentChars)
}

func withTaskID(payload json.RawMessage, taskID string) json.RawMessage {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		obj = map[string]any{}
	}
	obj["task_id"] = taskID
	out, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage(fmt.Sprintf(`{"task_id":%q}`, taskID))
	}
	return out
}

func (s *Server) ackCurrentAgentMessage(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	updated, err := s.store.AckMessage(s.pendingAgentAuditCtx(r, principal.Agent.ID, "message.ack", "message", "", principal.Agent.SquadID, nil), principal.Agent.ID, chi.URLParam(r, "messageID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
		return
	}
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
	updated, err := s.store.FailMessage(s.pendingAgentAuditCtx(r, principal.Agent.ID, "message.fail", "message", "", principal.Agent.SquadID, nil), principal.Agent.ID, chi.URLParam(r, "messageID"), req.Reason)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
		return
	}
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
		writeError(w, http.StatusBadRequest, "bad_request", msgTypeInvalid)
		return
	}
	u := currentUser(r.Context())
	attachments, err := s.resolveUploadAttachments(r, target.SquadID, req.Attachments)
	if err != nil {
		s.writeAttachmentError(w, err)
		return
	}
	created, err := s.store.CreateMessage(s.pendingUserAuditCtx(r, "message.create", "message", "", target.SquadID, nil), &domain.Message{
		FromType:      "user",
		FromID:        u.ID,
		ToAgentID:     target.ID,
		SquadID:       target.SquadID,
		Type:          messageType,
		Payload:       withAttachments(messagePayload(req), attachments),
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
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateTargetAgentState)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// wakeAgent pre-warms a scaled-to-zero agent (S-154). The chat UI calls it
// when the user starts typing so the runtime pod is already coming up by
// the time the message is actually sent. Idempotent and cheap: it only
// flips an *idle* agent to busy (which drives the Agent CR's
// desiredActive via the outbox); any other status is reported back
// untouched so we never clobber error/paused/busy state.
func (s *Server) wakeAgent(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadAgentForAction(w, r, "talk")
	if !ok {
		return
	}
	if target.Status != domain.AgentIdle {
		writeJSON(w, http.StatusOK, map[string]any{"waking": false, "status": string(target.Status)})
		return
	}
	// S-203 WP3: budget-blocked owners cannot pre-warm their agents.
	if s.agentBudgetBlocked(r.Context(), target.ID) {
		writeJSON(w, http.StatusOK, map[string]any{
			"waking": false,
			"status": string(target.Status),
			"reason": budgetStopNotice,
		})
		return
	}
	if err := s.setAgentStatusAndMirror(r.Context(), target.ID, domain.AgentBusy); err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"waking": true, "status": string(domain.AgentBusy)})
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
	fields := map[string]string{}
	if strings.TrimSpace(req.Message) != "" {
		fields["message"] = req.Message
	}
	if title := strings.TrimSpace(req.Title); title != "" {
		fields["title"] = req.Title
	}
	if len(fields) == 0 {
		return json.RawMessage(`{}`)
	}
	payload, err := json.Marshal(fields)
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

// consultDeadline (S-173) stamps agent consults with the deadline by
// which a correlated reply must land, else the sweeper notifies the
// asker. Per-send override wins; otherwise the platform default
// (SKQUAD_CONSULT_TIMEOUT_SECONDS, 15 min). Non-consult types never
// carry a deadline — delegate/handoff is the task loop, ping needs no
// answer, and replies close threads rather than opening them.
func (s *Server) consultDeadline(messageType domain.MessageType, req messageRequest) time.Time {
	if messageType != domain.MessageConsult {
		return time.Time{}
	}
	seconds := req.ConsultTimeoutSeconds
	if seconds <= 0 && s.cfg != nil {
		seconds = int(s.cfg.ConsultTimeout.Seconds())
	}
	if seconds <= 0 {
		return time.Time{}
	}
	if seconds > 86400 {
		seconds = 86400
	}
	return time.Now().UTC().Add(time.Duration(seconds) * time.Second)
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
	if perm.ResourceType == domain.ResAIProvider {
		provider, err := s.store.GetAIProvider(ctx, perm.ResourceID)
		if err != nil {
			return agentRuntimeResource{}, false, err
		}
		if provider.Status != domain.ResourceActive {
			return agentRuntimeResource{}, false, nil
		}
		manifest, err := json.Marshal(map[string]any{
			"kind": provider.Kind,
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

// agentMayMessageTarget enforces cross-squad messaging permissions. Same-squad
// messages are always allowed; cross-squad messages need the required action
// grant. On denial it records the audit event, writes the HTTP error and
// returns false.
func (s *Server) agentMayMessageTarget(w http.ResponseWriter, r *http.Request, principal *agentPrincipal, target *domain.Agent, messageType domain.MessageType) bool {
	if target.SquadID == principal.Agent.SquadID {
		return true
	}
	messageAction := requiredMessageAction(messageType)
	ok, err := s.store.AgentMayMessageSquad(r.Context(), principal.Agent.ID, target.SquadID, messageAction)
	if err != nil {
		writeStorageError(w, err)
		return false
	}
	if !ok {
		s.recordAgentAudit(r, principal.Agent.ID, "message.denied", "agent", target.ID, target.SquadID, nil)
		writeError(w, http.StatusForbidden, "forbidden", "agent cannot message the target squad")
		return false
	}
	return true
}

// deliverDelegatedMessage materializes a delegate/handoff message into a real
// task on the target squad's board and links the message to it as delivered.
func (s *Server) deliverDelegatedMessage(w http.ResponseWriter, r *http.Request, principal *agentPrincipal, target *domain.Agent, messageType domain.MessageType, req messageRequest) {
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
	task, err := s.materializeDelegatedTask(r.Context(), principal.Agent, target, created)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to materialize delegated task")
		return
	}
	delegateMeta, _ := json.Marshal(map[string]any{
		"message_id": created.ID,
		"task_id":    task.ID,
		"type":       string(messageType),
	})
	created, err = s.store.UpdateMessagePayload(s.pendingAgentAuditCtx(r, principal.Agent.ID, "message.delegate_materialized", "task", task.ID, target.SquadID, delegateMeta), created.ID, withTaskID(created.Payload, task.ID), domain.MessageDelivered)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to link delegated task to message")
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), target.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update target agent state")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) claimCurrentAgentTask(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	// Lenient decode: the claim body was historically ignored, so unknown
	// fields and even malformed bodies must not break claiming — they simply
	// mean "no started_at" (no wake-latency sample).
	startedAt := parseClaimStartedAt(r)
	task, err := s.store.ClaimNextTask(s.pendingAgentAuditCtx(r, principal.Agent.ID, "task.claim", "task", "", principal.Agent.SquadID, nil), principal.Agent.ID, workerIDFromRequest(r, principal.Agent.ID), defaultTaskExecutionLease)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeStorageError(w, err)
		return
	}
	if err := s.setAgentStatusAndMirror(r.Context(), principal.Agent.ID, domain.AgentBusy); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
		return
	}
	s.recordWakeLatency(r.Context(), principal.Agent, task.ID, startedAt)
	writeJSON(w, http.StatusOK, task)
}

// recordWakeLatency attributes a completed wake path (S-87): the most recent
// applied upsert_agent outbox event gives wake-requested (created) and
// CR-applied (updated) timestamps; the runtime reports its container start;
// now is the claim that delivered the task. Best-effort observability — a
// recording failure never fails the claim. Deduped per (agent, container
// start) in the store, so only the first task-delivering claim of a
// container start records.
func (s *Server) recordWakeLatency(ctx context.Context, agent *domain.Agent, taskID string, containerStarted time.Time) {
	if agent == nil || taskID == "" || containerStarted.IsZero() {
		return
	}
	upsert, err := s.store.LatestAppliedAgentUpsert(ctx, agent.ID)
	if err != nil {
		return // no attributable wake (no applied upsert yet)
	}
	claimed := time.Now().UTC()
	wakeRequested := upsert.CreatedAt.UTC()
	crApplied := upsert.UpdatedAt.UTC()
	containerStarted = containerStarted.UTC()
	if claimed.Before(wakeRequested) {
		return // clock skew beyond the wake window; not a trustworthy sample
	}
	scaleup := containerStarted.Sub(crApplied)
	if scaleup < 0 {
		scaleup = 0 // warm container: existed before the CR write
	}
	claimAnchor := crApplied
	if containerStarted.After(claimAnchor) {
		claimAnchor = containerStarted
	}
	claimDelay := claimed.Sub(claimAnchor)
	if claimDelay < 0 {
		claimDelay = 0
	}
	event := &domain.WakeLatencyEvent{
		AgentID:            agent.ID,
		SquadID:            agent.SquadID,
		TaskID:             taskID,
		WakeRequestedAt:    wakeRequested,
		CRAppliedAt:        crApplied,
		ContainerStartedAt: containerStarted,
		ClaimedAt:          claimed,
		QueueMs:            float64(crApplied.Sub(wakeRequested).Milliseconds()),
		ScaleupMs:          float64(scaleup.Milliseconds()),
		ClaimDelayMs:       float64(claimDelay.Milliseconds()),
		E2EMs:              float64(claimed.Sub(wakeRequested).Milliseconds()),
		ColdStart:          !containerStarted.Before(wakeRequested),
	}
	if _, err := s.store.RecordWakeLatency(ctx, event); err != nil {
		slog.Warn("wake latency recording failed", "agent_id", agent.ID, "task_id", taskID, "error", err)
	}
}

func (s *Server) startCurrentAgentTask(w http.ResponseWriter, r *http.Request) {
	// S-PROMPT WP5 run-audit: the runtime reports the composed-prompt
	// sha it is running under (or "env_legacy") in the start body so
	// "what did the agent see for run X" is one server-side query.
	// Old runtimes send no body — that stays valid (sha stays "").
	var req struct {
		PromptSHA string `json:"prompt_sha"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	promptSHA := strings.TrimSpace(req.PromptSHA)
	if promptSHA != "" && !validRunPromptSHA(promptSHA) {
		writeError(w, http.StatusBadRequest, "invalid_prompt_sha", "prompt_sha must be a 64-char hex sha256 or the literal env_legacy")
		return
	}
	// S-213/S-228: an agent may only start work from a pickup column —
	// TO DO, or resuming its own in-progress task. Backlog and every other
	// column are rejected here with a machine-readable reason before the
	// status update runs; pickup must go through the claim path, which
	// only serves the TO DO column. A human moving the card out of Backlog is the
	// instruction to start it. Missing/foreign tasks are still reported by
	// updateCurrentAgentTaskStatus below.
	if existing, err := s.store.GetTask(r.Context(), chi.URLParam(r, "taskID")); err == nil && !existing.Status.IsAgentPickupStatus() {
		if existing.Status == domain.TaskBacklog {
			writeError(w, http.StatusConflict, "task_in_backlog", "task is in the backlog and must be moved out by a human before it can be started")
			return
		}
		writeError(w, http.StatusConflict, "task_not_claimable", fmt.Sprintf("task is in %q and is not ready to start; only TO DO column tasks are claimable", existing.Status))
		return
	}
	updated, ok := s.updateCurrentAgentTaskStatus(w, r, domain.TaskInProgress, domain.AgentBusy, "task.start")
	if !ok {
		return
	}
	if promptSHA != "" {
		principal := currentAgent(r.Context())
		if _, err := s.store.SetTaskExecutionPromptSHA(r.Context(), principal.Agent.ID, updated.ID, promptSHA); err != nil {
			writeStorageError(w, err)
			return
		}
		updated.PromptSHA = promptSHA
	}
	writeJSON(w, http.StatusOK, updated)
}

// appendCurrentAgentTaskThread S-183: stream one assistant working turn
// of the current task into the task thread, so the task page shows the
// agent's work-in-progress rather than only the final summary. Only the
// assigned agent may append; turns are capped like inbox messages. The
// runtime treats failures as best-effort (logged, never task-fatal).
func (s *Server) appendCurrentAgentTaskThread(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
	}
	if r.Body == nil || r.ContentLength == 0 {
		writeError(w, http.StatusBadRequest, "missing_body", msgMessageRequired)
		return
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Message)
	if text == "" {
		writeError(w, http.StatusBadRequest, "empty_message", "message must not be empty")
		return
	}
	principal := currentAgent(r.Context())
	task, err := s.store.GetTask(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if task.AssigneeAgentID != principal.Agent.ID {
		writeError(w, http.StatusForbidden, "not_assignee", "only the assigned agent can append turns to this task thread")
		return
	}
	s.appendTaskThreadEvent(r.Context(), task, "agent", principal.Agent.ID, domain.MessageReply, trimRunes(text, maxTaskThreadTurnChars))
	w.WriteHeader(http.StatusNoContent)
}

// parseClaimStartedAt leniently extracts a started_at timestamp from the
// claim request body. Missing bodies, decode failures and unparseable values
// all yield the zero time (no wake-latency sample).
func parseClaimStartedAt(r *http.Request) time.Time {
	if r.Body == nil || r.ContentLength == 0 {
		return time.Time{}
	}
	var probe struct {
		StartedAt string `json:"started_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&probe); err != nil || probe.StartedAt == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, probe.StartedAt); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339Nano, probe.StartedAt); err == nil {
		return parsed
	}
	return time.Time{}
}

// taskCompletionRequest is the JSON body for completing the agent's current task.
type taskCompletionRequest struct {
	Status        domain.TaskStatus `json:"status"`
	Summary       string            `json:"summary"`
	PersistMemory bool              `json:"persist_memory"`
	ExecutionID   string            `json:"execution_id"`
	FencingToken  string            `json:"fencing_token"`
}

func (s *Server) completeCurrentAgentTask(w http.ResponseWriter, r *http.Request) {
	var req taskCompletionRequest
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
	updated, err := s.store.CompleteTaskExecution(s.pendingAgentAuditCtx(r, principal.Agent.ID, "task.complete", "task", taskID, principal.Agent.SquadID, nil), principal.Agent.ID, taskID, executionID, fencingToken, req.Status, summary)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
		return
	}
	subject, body := taskNotifySubjectBody(updated, principal.Agent.Name, string(req.Status), summary)
	s.notifySquadOwnerRich(r.Context(), ownerNotification{
		SquadID:     updated.SquadID,
		Kind:        domain.InboxTaskCompleted,
		FromAgentID: principal.Agent.ID,
		TaskID:      updated.ID,
		Message:     fmt.Sprintf("Agent %s moved task %q to %s", principal.Agent.Name, updated.Title, req.Status),
		Subject:     subject,
		Body:        body,
	})
	s.notifyDelegationResult(r.Context(), updated, principal.Agent, string(req.Status), summary)
	// S-181: the agent's turn itself must appear in the task thread, not
	// only in the owner's inbox. Empty summaries are surfaced explicitly
	// rather than silently vanishing (see S-182).
	threadText := strings.TrimSpace(summary)
	if threadText == "" {
		threadText = fmt.Sprintf("Moved this task to %s without a summary.", req.Status)
	}
	s.appendTaskThreadEvent(r.Context(), updated, "agent", principal.Agent.ID, domain.MessageReply, threadText)
	if req.PersistMemory && strings.TrimSpace(req.Summary) != "" {
		s.persistCompletionMemory(w, r, principal, updated, summary, strings.TrimSpace(req.Summary), req)
	}
	writeJSON(w, http.StatusOK, updated)
}

// persistCompletionMemory stores the runtime completion summary as raw-model
// agent memory pending review. A persistence failure is audited but never
// fails the completion itself.
func (s *Server) persistCompletionMemory(w http.ResponseWriter, r *http.Request, principal *agentPrincipal, updated *domain.Task, summary, rawSummary string, req taskCompletionRequest) {
	executionID := strings.TrimSpace(req.ExecutionID)
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
	embedding, embeddingModel := s.embedMemoryText(r.Context(), summary)
	if _, err := s.store.CreateAgentMemory(r.Context(), &domain.AgentMemory{
		AgentID:        principal.Agent.ID,
		SquadID:        updated.SquadID,
		SourceTaskID:   updated.ID,
		Content:        summary,
		RawContent:     rawSummary,
		TrustLevel:     "raw_model_output",
		Provenance:     "task_completion",
		ReviewStatus:   "pending_review",
		Embedding:      embedding,
		EmbeddingModel: embeddingModel,
		Metadata:       metadata,
	}); err != nil {
		auditMetadata, _ := json.Marshal(map[string]string{"error": err.Error(), "execution_id": executionID})
		s.recordAgentAudit(r, principal.Agent.ID, "task.memory_persist_failed", "task", updated.ID, updated.SquadID, auditMetadata)
	}
}

// embedMemoryText (S-212) produces the write-time embedding for memory
// content. Best-effort by contract: when the embedder is unconfigured
// or fails, the memory is still stored — without a vector — and the
// backfill job (cmd/embed-backfill) re-embeds it later. Returns
// (nil, "") in those cases; errors are logged, never surfaced to the
// completion path.
func (s *Server) embedMemoryText(ctx context.Context, content string) ([]float64, string) {
	if s.embeddings == nil || s.cfg == nil || s.cfg.MemoryEmbeddingModel == "" {
		return nil, ""
	}
	vec, err := s.embeddings.Embed(ctx, content)
	if err != nil {
		log.Printf("memory embedding generation failed (storing without vector): %v", err)
		return nil, ""
	}
	return vec, s.cfg.MemoryEmbeddingModel
}

func (s *Server) reportCurrentAgentTaskWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceResourceID string `json:"workspace_resource_id"`
		Branch              string `json:"branch"`
		CommitSHA           string `json:"commit_sha"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	principal := currentAgent(r.Context())
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(r.Context(), taskID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if task.AssigneeAgentID != principal.Agent.ID {
		writeError(w, http.StatusForbidden, "forbidden", msgTaskNotAssigned)
		return
	}
	resID := strings.TrimSpace(req.WorkspaceResourceID)
	branch := strings.TrimSpace(req.Branch)
	commitSHA := strings.TrimSpace(req.CommitSHA)
	if resID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "workspace_resource_id is required")
		return
	}
	// The agent may only link a git workspace it is actually granted and that
	// is active — prevents forging refs to workspaces the agent cannot access.
	perms, err := s.store.ListAgentPermissions(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	granted := false
	for _, p := range perms {
		if p.ResourceType == domain.ResProjectWorkspace && p.ResourceID == resID {
			res, err := s.store.GetResource(r.Context(), domain.ResProjectWorkspace, resID)
			if err == nil && res.Status == domain.ResourceActive {
				granted = true
			}
			break
		}
	}
	if !granted {
		s.recordAgentAudit(r, principal.Agent.ID, "task.workspace_link_denied", "task", taskID, task.SquadID, nil)
		writeError(w, http.StatusForbidden, "forbidden", "workspace is not granted to this agent")
		return
	}
	meta, _ := json.Marshal(map[string]string{"workspace_resource_id": resID, "branch": branch, "commit_sha": commitSHA})
	updated, err := s.store.SetTaskWorkspace(s.pendingAgentAuditCtx(r, principal.Agent.ID, "task.workspace_linked", "task", taskID, task.SquadID, meta), taskID, resID, branch, commitSHA)
	if err != nil {
		writeStorageError(w, err)
		return
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
		s.pendingAgentAuditCtx(r, principal.Agent.ID, "task.block", "task", chi.URLParam(r, "taskID"), principal.Agent.SquadID, nil),
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
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
		return
	}
	blockNote := strings.TrimSpace(req.Summary)
	if blockNote != "" {
		blockNote = ": " + blockNote
	}
	subject, body := taskNotifySubjectBody(updated, principal.Agent.Name, string(domain.TaskBlocked), strings.TrimSpace(req.Summary))
	s.notifySquadOwnerRich(r.Context(), ownerNotification{
		SquadID:     updated.SquadID,
		Kind:        domain.InboxActionRequired,
		FromAgentID: principal.Agent.ID,
		TaskID:      updated.ID,
		Message:     fmt.Sprintf("Agent %s blocked task %q%s", principal.Agent.Name, updated.Title, blockNote),
		Subject:     subject,
		Body:        body,
	})
	s.notifyDelegationBlocked(r.Context(), updated, principal.Agent)
	// S-193: a blocked task is also a bell notification — it awaits a
	// human decision/answer, distinct from the inbox copy.
	s.emitNotification(r.Context(), updated.SquadID, updated.ID, principal.Agent.ID,
		domain.NotificationTaskBlocked, domain.NotificationWarning,
		fmt.Sprintf("Task %s requires attention: blocked by agent %s%s", formatTaskRef(updated), principal.Agent.Name, blockNote))
	// S-181: the agent's block turn belongs in the thread too.
	blockedText := strings.TrimSpace(req.Summary)
	if blockedText == "" {
		blockedText = "Blocked this task without a reason."
	}
	s.appendTaskThreadEvent(r.Context(), updated, "agent", principal.Agent.ID, domain.MessageReply, blockedText)
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
		writeError(w, http.StatusBadRequest, "bad_request", msgStatusInvalid)
		return
	}
	if !s.heartbeatExecutionFence(w, r, principal, req.Status, req.ExecutionID, req.FencingToken) {
		return
	}
	status, ok := s.resolveHeartbeatStatus(r.Context(), w, principal.Agent.ID, req.Status)
	if !ok {
		return
	}
	previousStatus := principal.Agent.Status
	if err := s.setAgentStatusAndMirror(r.Context(), principal.Agent.ID, status); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
		return
	}
	// S-193: a transition into error while the agent holds in-progress
	// work is a task_failed bell alert (transition-gated so a sticky-error
	// agent beating every interval cannot spam the owner).
	if status == domain.AgentError {
		s.notifyTaskFailedOnAgentError(r.Context(), principal.Agent, previousStatus)
	}
	agent, err := s.store.GetAgent(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// heartbeatExecutionFence renews the task execution lease when a busy
// heartbeat carries an execution id. On failure it writes the HTTP error and
// returns false.
func (s *Server) heartbeatExecutionFence(w http.ResponseWriter, r *http.Request, principal *agentPrincipal, status domain.AgentStatus, executionID, fencingToken string) bool {
	if status != domain.AgentBusy || strings.TrimSpace(executionID) == "" {
		return true
	}
	if strings.TrimSpace(fencingToken) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "fencing_token is required with execution_id")
		return false
	}
	if _, err := s.store.HeartbeatTaskExecution(r.Context(), principal.Agent.ID, strings.TrimSpace(executionID), strings.TrimSpace(fencingToken), defaultTaskExecutionLease); err != nil {
		writeStorageError(w, err)
		return false
	}
	return true
}

// resolveHeartbeatStatus upgrades an idle heartbeat to busy when the agent
// still has pending work. On storage failure it writes the HTTP error and
// returns ok=false.
func (s *Server) resolveHeartbeatStatus(ctx context.Context, w http.ResponseWriter, agentID string, status domain.AgentStatus) (domain.AgentStatus, bool) {
	if status != domain.AgentIdle {
		return status, true
	}
	pending, err := s.agentHasPendingWork(ctx, agentID)
	if err != nil {
		writeStorageError(w, err)
		return status, false
	}
	if pending {
		return domain.AgentBusy, true
	}
	return status, true
}

func (s *Server) updateCurrentAgentTaskStatus(w http.ResponseWriter, r *http.Request, taskStatus domain.TaskStatus, agentStatus domain.AgentStatus, action string) (*domain.Task, bool) {
	principal := currentAgent(r.Context())
	task, err := s.store.GetTask(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if task.AssigneeAgentID != principal.Agent.ID {
		writeError(w, http.StatusForbidden, "forbidden", msgTaskNotAssigned)
		return nil, false
	}
	task.Status = taskStatus
	updated, err := s.store.UpdateTask(s.pendingAgentAuditCtx(r, principal.Agent.ID, action, "task", task.ID, task.SquadID, nil), task)
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if agentStatus == domain.AgentIdle {
		if err := s.syncAgentStatusFromPendingWork(r.Context(), principal.Agent.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
			return nil, false
		}
	} else if err := s.setAgentStatusAndMirror(r.Context(), principal.Agent.ID, agentStatus); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
		return nil, false
	}
	return updated, true
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadAccessibleTask(w, r)
	if !ok {
		return
	}
	// S-PROMPT WP5 run-audit: attach the latest execution attempt so a
	// single task-detail query answers "what prompt sha did this run
	// use". FencingToken stays private to the claiming worker.
	if exec, err := s.store.GetLatestTaskExecution(r.Context(), task.ID); err == nil {
		task.ExecutionID = exec.ID
		task.WorkerID = exec.WorkerID
		task.LeaseExpiresAt = exec.LeaseExpiresAt
		task.PromptSHA = exec.PromptSHA
	}
	writeJSON(w, http.StatusOK, task)
}

func taskIDFromPayload(payload json.RawMessage) string {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		return ""
	}
	if id, ok := obj["task_id"].(string); ok {
		return id
	}
	return ""
}

// listTaskMessages returns the task-scoped thread: every message on the
// task's assignee whose payload carries this task's id. Reuses the agent
// history store so no new persistence surface is needed.
func (s *Server) listTaskMessages(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadAccessibleTask(w, r)
	if !ok {
		return
	}
	filtered := []*domain.Message{}
	if task.AssigneeAgentID == "" {
		writeJSON(w, http.StatusOK, filtered)
		return
	}
	messages, err := s.store.ListAgentMessageHistory(r.Context(), task.AssigneeAgentID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	for _, msg := range messages {
		if taskIDFromPayload(msg.Payload) == task.ID {
			filtered = append(filtered, msg)
		}
	}
	// S-235: resolve sender display names (current agent name / user
	// first name) so the thread never renders raw GUIDs.
	refs := make([]actorRef, 0, len(filtered))
	for _, msg := range filtered {
		refs = append(refs, actorRef{kind: msg.FromType, id: msg.FromID})
	}
	decorateMessages(filtered, s.resolveActorDisplays(r.Context(), refs))
	writeJSON(w, http.StatusOK, filtered)
}

// createTaskMessage is the task-page composer: a user message routed to the
// task's assignee with task_id forced into the payload so the thread stays
// scoped even if the caller omits it.
func (s *Server) createTaskMessage(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadOwnedTask(w, r)
	if !ok {
		return
	}
	if task.AssigneeAgentID == "" {
		writeError(w, http.StatusBadRequest, "no_assignee", "task has no assigned agent to receive this message")
		return
	}
	var req messageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "empty_message", msgMessageRequired)
		return
	}
	messageType := req.Type
	if messageType == "" {
		messageType = domain.MessageConsult
	}
	if !messageType.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", msgTypeInvalid)
		return
	}
	u := currentUser(r.Context())
	attachments, err := s.resolveUploadAttachments(r, task.SquadID, req.Attachments)
	if err != nil {
		s.writeAttachmentError(w, err)
		return
	}
	created, err := s.store.CreateMessage(s.pendingUserAuditCtx(r, "task.message", "task", task.ID, task.SquadID, nil), &domain.Message{
		FromType:      "user",
		FromID:        u.ID,
		ToAgentID:     task.AssigneeAgentID,
		SquadID:       task.SquadID,
		Type:          messageType,
		Payload:       withAttachments(withTaskID(messagePayload(req), task.ID), attachments),
		Status:        domain.MessagePending,
		CorrelationID: strings.TrimSpace(req.CorrelationID),
		MaxAttempts:   req.MaxAttempts,
		ExpiresAt:     messageExpiresAt(req),
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), task.AssigneeAgentID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateTargetAgentState)
		return
	}
	writeJSON(w, http.StatusCreated, created)
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
	if !s.applyTaskAssigneeChange(r.Context(), w, task, req.AssigneeAgentID) {
		return
	}

	updated, err := s.store.UpdateTask(s.pendingUserAuditCtx(r, "task.update", "task", task.ID, task.SquadID, nil), task)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	// S-181: assigning a task later (board drag-drop / edit) is the moment
	// the agent joins — record it in the thread just like create-with-assignee.
	if updated.AssigneeAgentID != "" && updated.AssigneeAgentID != previousAssignee {
		if u := currentUser(r.Context()); u != nil {
			s.appendTaskThreadEvent(r.Context(), updated, "user", u.ID, domain.MessageConsult, "Task assigned to you: "+updated.Title)
		}
	}
	if err := s.syncAffectedAgentsFromTaskChange(r.Context(), previousAssignee, updated.AssigneeAgentID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAssignedAgentState)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// applyTaskAssigneeChange validates and applies an assignee change to the
// task. A non-empty assignee must exist and belong to the task's squad. On
// failure it writes the HTTP error and returns false.
func (s *Server) applyTaskAssigneeChange(ctx context.Context, w http.ResponseWriter, task *domain.Task, assigneeAgentID *string) bool {
	if assigneeAgentID == nil {
		return true
	}
	if *assigneeAgentID != "" {
		agent, err := s.store.GetAgent(ctx, *assigneeAgentID)
		if err != nil {
			writeStorageError(w, err)
			return false
		}
		if agent.SquadID != task.SquadID {
			writeError(w, http.StatusBadRequest, "bad_request", "assignee_agent_id must belong to this squad")
			return false
		}
	}
	task.AssigneeAgentID = *assigneeAgentID
	return true
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
		writeError(w, http.StatusBadRequest, "bad_request", msgStatusInvalid)
		return
	}
	previousAssignee := task.AssigneeAgentID
	task.Status = req.Status
	updated, err := s.store.UpdateTask(s.pendingUserAuditCtx(r, "task.move", "task", task.ID, task.SquadID, nil), task)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.syncAffectedAgentsFromTaskChange(r.Context(), previousAssignee, updated.AssigneeAgentID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAssignedAgentState)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadOwnedTask(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTask(s.pendingUserAuditCtx(r, "task.delete", "task", task.ID, task.SquadID, nil), task.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	if task.AssigneeAgentID != "" {
		if err := s.syncAgentStatusFromPendingWork(r.Context(), task.AssigneeAgentID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", msgUpdateAssignedAgentState)
			return
		}
	}
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
	writeError(w, http.StatusForbidden, "forbidden", msgNotSquadOwner)
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
		writeError(w, http.StatusForbidden, "forbidden", msgNotSquadOwner)
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
		s.recordUserAudit(r, auditAccessDenied, "squad", squad.ID, squad.ID, nil)
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
		s.recordUserAudit(r, auditAccessDenied, "squad", squad.ID, squad.ID, nil)
		writeError(w, http.StatusForbidden, "forbidden", msgNotSquadOwner)
	} else {
		s.recordUserAudit(r, auditAccessDenied, "squad", squad.ID, squad.ID, nil)
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
	s.recordUserAudit(r, auditAccessDenied, "squad", squad.ID, squad.ID, nil)
	writeError(w, http.StatusForbidden, "forbidden", msgNotSquadOwner)
	return nil, false
}

func (s *Server) recordUserAudit(r *http.Request, action, resourceType, resourceID, squadID string, metadata json.RawMessage) {
	_ = s.recordUserAuditRequired(r, action, resourceType, resourceID, squadID, metadata)
}

// pendingUserAuditCtx attaches a same-transaction audit entry to the
// request context (S-86). The next significant store mutation drains and
// writes it inside its transaction: a committed change always carries its
// audit record, and an audit-write failure rolls the mutation back.
func (s *Server) pendingUserAuditCtx(r *http.Request, action, resourceType, resourceID, squadID string, metadata json.RawMessage) context.Context {
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	actorType, actorID := "system", ""
	if u := currentUser(r.Context()); u != nil {
		actorType, actorID = "user", u.ID
	}
	return storage.WithPendingAudit(r.Context(), &domain.AuditEntry{
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		SquadID:      squadID,
		Metadata:     metadata,
	})
}

// pendingAgentAuditCtx is the agent-actor counterpart of pendingUserAuditCtx
// (S-86): the audit entry commits with the next store mutation that drains it.
func (s *Server) pendingAgentAuditCtx(r *http.Request, agentID, action, resourceType, resourceID, squadID string, metadata json.RawMessage) context.Context {
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	return storage.WithPendingAudit(r.Context(), &domain.AuditEntry{
		ActorType:    "agent",
		ActorID:      agentID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		SquadID:      squadID,
		Metadata:     metadata,
	})
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
	// S-203 WP3: a budget-blocked owner's agents never *start*. A busy
	// heartbeat from an already-running (busy) agent is allowed through —
	// the pod is mid-turn and must not be killed; it tears down at end of
	// turn via the idle mirror with the blocked idle-timeout override.
	if status == domain.AgentBusy && s.agentBudgetBlocked(ctx, agentID) {
		agent, err := s.store.GetAgent(ctx, agentID)
		if err == nil && agent.Status != domain.AgentBusy {
			log.Printf("budget-enforce: refused busy transition for agent %s (owner over budget)", agentID)
			return nil
		}
	}
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

// orString returns value when non-empty, otherwise the fallback.
func orString(value, fallback string) string {
	if value == "" {
		return fallback
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
		slog.Error("unexpected storage error", "error", err)
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

// defaultSearchProviders builds the production provider set from
// control-plane secrets (BT-2, ADR-0012 §3): duckduckgo is always
// available (keyless); brave/perplexity are registered only when their
// API key is present. A selected-but-absent provider maps to 502 at
// request time — keys never leave this process.
func defaultSearchProviders(cfg *config.Config) map[string]search.Provider {
	providers := map[string]search.Provider{
		"duckduckgo": search.NewDuckDuckGo(&http.Client{Timeout: 30 * time.Second}),
	}
	if cfg == nil {
		return providers
	}
	if cfg.SearchBraveAPIKey != "" {
		providers["brave"] = search.NewBrave(&http.Client{Timeout: 30 * time.Second}, cfg.SearchBraveAPIKey)
	}
	if cfg.SearchPerplexityAPIKey != "" {
		providers["perplexity"] = search.NewPerplexity(&http.Client{Timeout: 60 * time.Second}, cfg.SearchPerplexityAPIKey)
	}
	return providers
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
