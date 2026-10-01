// S-GWREG: automatic LiteLLM gateway model-deployment registration.
//
// Registering an AI Model in the UI must fully provision the gateway:
// the registry row alone is dead weight — agents bound to a model with
// no gateway deployment die with "no healthy deployments for this
// model". These helpers wrap the gateway model-deployment surface
// (litellm_client.go) with the fail-loud semantics the AI-model CRUD
// handlers need, plus the admin drift-reconcile endpoint.
//
// Fail-loud contract: the gateway deployment is created BEFORE the
// registry row; a gateway failure returns 502 gateway_provision_failed
// and no registry row is written, so a silently-unusable model can
// never exist. If the registry write then fails, the just-created
// deployment is deleted again (compensate) and the gateway is reloaded.
//
// Reload contract: the litellm router does not pick up DB-persisted
// model changes until the gateway pod restarts (docs/llm-gateway.md),
// so every successful deployment mutation triggers one rollout restart
// via the K8s API (kube.GatewayReloader). Reload failures are logged
// loudly but do not fail the request — the deployment is persisted and
// the next restart picks it up; reconcile can also be re-run.
//
// When the gateway is not configured (dev mode, no LiteLLMAdminURL/
// master key) provisioning is skipped with a warning log, exactly like
// the pre-S-GWREG behaviour.

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
)

// GatewayModelRegistry is the model-deployment half of the LiteLLM
// gateway admin surface. The production implementation is
// *liteLLMGatewayClient (same client + master key as the virtual-key
// surface); tests inject fakes.
type GatewayModelRegistry interface {
	// DeployModel registers a new deployment and returns its gateway id.
	DeployModel(ctx context.Context, spec GatewayModelSpec) (deploymentID string, err error)
	// UpdateModelDeployment rewrites an existing deployment's params.
	UpdateModelDeployment(ctx context.Context, deploymentID string, spec GatewayModelSpec) error
	// DeleteModelDeployment removes a deployment by gateway id.
	DeleteModelDeployment(ctx context.Context, deploymentID string) error
	// ListModelDeployments returns every deployment the gateway knows.
	ListModelDeployments(ctx context.Context) ([]GatewayModelDeployment, error)
}

// GatewayReloader triggers a rollout restart of the gateway deployment
// so the litellm router reloads DB-persisted model changes.
type GatewayReloader interface {
	ReloadGateway(ctx context.Context) error
}

// litellmModelPrefixes maps provider kind → litellm model prefix.
// Verified against the live gateway registration shape: the prefix is
// the provider kind itself for every supported kind.
var litellmModelPrefixes = map[string]string{
	"openai":      "openai",
	"anthropic":   "anthropic",
	"gemini":      "gemini",
	"ollama_chat": "ollama_chat",
	"ollama":      "ollama",
	"azure":       "azure",
}

// litellmModelPrefix resolves the litellm model prefix for a provider
// kind. Unknown kinds are rejected — registering with a guessed prefix
// would create a deployment that fails at call time, which is exactly
// the silent failure this feature exists to eliminate.
func litellmModelPrefix(kind string) (string, bool) {
	prefix, ok := litellmModelPrefixes[strings.TrimSpace(kind)]
	return prefix, ok
}

// gatewayModelsEnabled reports whether gateway model provisioning is
// wired (gateway configured). False in dev mode → handlers skip with a
// warning instead of failing.
func (s *Server) gatewayModelsEnabled() bool {
	return s.gwModels != nil
}

// newGatewayReloader builds the S-GWREG gateway rollout reloader from
// in-cluster K8s config (same guard as newPodRestarter). Absent config
// → nil, nil: dev mode, reload is skipped with a warning.
func newGatewayReloader(cfg *config.Config) (*kube.GatewayReloader, error) {
	if cfg == nil || !cfg.K8sEnabled || cfg.K8sAPIBase == "" || cfg.K8sTokenFile == "" {
		return nil, nil
	}
	return kube.NewGatewayReloader(cfg)
}

// buildGatewayModelSpec resolves the provider's kind, base_url and live
// API key into the litellm registration shape. The key is resolved
// through the S-155 Secret store and never logged.
func (s *Server) buildGatewayModelSpec(ctx context.Context, provider *domain.LLMProvider, modelName string) (GatewayModelSpec, error) {
	prefix, ok := litellmModelPrefix(provider.Kind)
	if !ok {
		return GatewayModelSpec{}, fmt.Errorf("provider kind %q has no LiteLLM model prefix mapping", provider.Kind)
	}
	apiKey, err := s.resolveProviderKey(ctx, provider)
	if err != nil {
		return GatewayModelSpec{}, fmt.Errorf("resolve provider %s key: %w", provider.ID, err)
	}
	return GatewayModelSpec{
		ModelName:    modelName,
		LitellmModel: prefix + "/" + modelName,
		APIBase:      strings.TrimSpace(provider.BaseURL),
		APIKey:       apiKey,
	}, nil
}

// findGatewayDeployment returns the gateway deployment id registered
// under the given litellm model_name, if any.
func (s *Server) findGatewayDeployment(ctx context.Context, modelName string) (string, bool, error) {
	deployments, err := s.gwModels.ListModelDeployments(ctx)
	if err != nil {
		return "", false, err
	}
	for _, d := range deployments {
		if d.ModelName == modelName {
			return d.DeploymentID, true, nil
		}
	}
	return "", false, nil
}

// provisionGatewayModel creates the gateway deployment for (provider,
// modelName) and returns its id. The caller triggers the reload only
// after the registry write succeeds, so a request causes at most one
// rollout restart on the happy path.
func (s *Server) provisionGatewayModel(ctx context.Context, provider *domain.LLMProvider, modelName string) (string, error) {
	spec, err := s.buildGatewayModelSpec(ctx, provider, modelName)
	if err != nil {
		return "", err
	}
	return s.gwModels.DeployModel(ctx, spec)
}

// updateGatewayModelDeployment converges the gateway deployment when a
// PATCH changes provider_id and/or model_name:
//
//   - same model_name (provider/credential change): /model/update in place.
//   - renamed: create the new deployment FIRST, then delete the old one
//     (avoids a window where the model has no deployment at all, and
//     avoids relying on litellm rename semantics on /model/update). If
//     the old deployment cannot be deleted, the new one is removed
//     again so no shadow deployment survives.
//   - old deployment missing (drift): plain create.
func (s *Server) updateGatewayModelDeployment(ctx context.Context, oldModelName string, provider *domain.LLMProvider, newModelName string) error {
	spec, err := s.buildGatewayModelSpec(ctx, provider, newModelName)
	if err != nil {
		return err
	}
	depID, found, err := s.findGatewayDeployment(ctx, oldModelName)
	if err != nil {
		return err
	}
	if !found {
		_, err = s.gwModels.DeployModel(ctx, spec)
		return err
	}
	if oldModelName == newModelName {
		return s.gwModels.UpdateModelDeployment(ctx, depID, spec)
	}
	newID, err := s.gwModels.DeployModel(ctx, spec)
	if err != nil {
		return err
	}
	if err := s.gwModels.DeleteModelDeployment(ctx, depID); err != nil {
		if newID != "" {
			if cleanupErr := s.gwModels.DeleteModelDeployment(ctx, newID); cleanupErr != nil {
				log.Printf("gateway rename rollback: model %q: delete new deployment %s after failed old-deployment removal: %v", newModelName, newID, cleanupErr)
			}
		}
		return fmt.Errorf("delete old deployment %s for renamed model: %w", depID, err)
	}
	return nil
}

// compensateGatewayDelete best-effort removes a gateway deployment
// after a registry write failure (fail-loud compensation), then
// reloads so the router never keeps serving a model skquad refused to
// register. Failures are logged loudly; the reconcile endpoint is the
// repair path.
func (s *Server) compensateGatewayDelete(ctx context.Context, deploymentID, modelName string) {
	if deploymentID == "" {
		return
	}
	if err := s.gwModels.DeleteModelDeployment(ctx, deploymentID); err != nil {
		log.Printf("gateway compensation: model %q: could not delete deployment %s after registry failure: %v (run POST /api/v1/admin/gateway/models/reconcile after fixing)", modelName, deploymentID, err)
		return
	}
	s.reloadGateway(ctx)
}

// reloadGateway triggers the rollout restart. A missing reloader (dev
// mode without K8s config) or a failed restart is logged loudly but
// does not fail the originating request: the deployment is persisted
// in the gateway DB and any restart will pick it up.
func (s *Server) reloadGateway(ctx context.Context) {
	if s.gwReloader == nil {
		log.Printf("gateway reload skipped: reloader not configured (dev mode) — restart %s manually to pick up model changes", "skquad-llm-gateway")
		return
	}
	if err := s.gwReloader.ReloadGateway(ctx); err != nil {
		log.Printf("gateway reload FAILED (model changes persisted but router is stale until next restart): %v", err)
	}
}

// gatewayModelFailure writes the fail-loud 502 used by create/update.
func writeGatewayProvisionFailure(w http.ResponseWriter, op string, err error) {
	writeError(w, http.StatusBadGateway, "gateway_provision_failed",
		fmt.Sprintf("model was NOT %s: LiteLLM gateway deployment failed (%v); fix the gateway and retry — no registry row was written", op, err))
}

// gatewayModelReconcileReport is the drift-reconcile response shape.
type gatewayModelReconcileReport struct {
	Registered     []string                `json:"registered"`
	AlreadyPresent []string                `json:"already_present"`
	Failed         []gatewayModelFailure   `json:"failed"`
	Extras         []gatewayModelExtra     `json:"extras"`
}

type gatewayModelFailure struct {
	Model string `json:"model"`
	Error string `json:"error"`
}

// gatewayModelExtra is a gateway deployment with no matching ACTIVE
// registry entry. Reported, never deleted (a follow-up card owns
// gateway-deployment deletion).
type gatewayModelExtra struct {
	ModelName    string `json:"model_name"`
	DeploymentID string `json:"deployment_id"`
}

// reconcileGatewayModels compares every ACTIVE ai_models row against
// the gateway's model deployments: missing ones are registered (same
// shape as create), extras are reported WITHOUT deletion. One reload is
// triggered if anything was registered. platform_admin only, same auth
// as the keys reconcile endpoint.
func (s *Server) reconcileGatewayModels(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	if !s.gatewayModelsEnabled() {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "LLM gateway is not configured; model reconcile is unavailable")
		return
	}
	models, err := s.store.ListAIModels(r.Context(), domain.ResourceActive)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	deployments, err := s.gwModels.ListModelDeployments(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "gateway_unavailable", fmt.Sprintf("could not list gateway model deployments: %v", err))
		return
	}
	byModel := make(map[string]GatewayModelDeployment, len(deployments))
	for _, d := range deployments {
		byModel[d.ModelName] = d
	}
	report := gatewayModelReconcileReport{
		Registered:     make([]string, 0),
		AlreadyPresent: make([]string, 0),
		Failed:         make([]gatewayModelFailure, 0),
		Extras:         make([]gatewayModelExtra, 0),
	}
	for _, model := range models {
		if _, ok := byModel[model.ModelName]; ok {
			report.AlreadyPresent = append(report.AlreadyPresent, model.ModelName)
			delete(byModel, model.ModelName)
			continue
		}
		if _, err := s.provisionGatewayModel(r.Context(), s.providerForAIModel(r, model), model.ModelName); err != nil {
			report.Failed = append(report.Failed, gatewayModelFailure{Model: model.ModelName, Error: err.Error()})
			continue
		}
		report.Registered = append(report.Registered, model.ModelName)
	}
	for _, d := range byModel {
		report.Extras = append(report.Extras, gatewayModelExtra{ModelName: d.ModelName, DeploymentID: d.DeploymentID})
	}
	if len(report.Registered) > 0 {
		s.reloadGateway(r.Context())
	}
	metadata, _ := json.Marshal(report)
	s.recordUserAudit(r, "gateway.models.reconcile", "gateway", "", "", metadata)
	writeJSON(w, http.StatusOK, report)
}

// providerForAIModel resolves the model's provider for registration.
// A missing provider surfaces as a per-model registration error — it
// must never abort the whole reconcile pass.
func (s *Server) providerForAIModel(r *http.Request, model *domain.AIModel) *domain.LLMProvider {
	provider, err := s.store.GetLLMProvider(r.Context(), model.ProviderID)
	if err != nil {
		return &domain.LLMProvider{ID: model.ProviderID, Kind: "__missing_provider__"}
	}
	return provider
}
