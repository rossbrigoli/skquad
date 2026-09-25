package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	defaultOutboxBatchSize = 20
	defaultOutboxLease     = 2 * time.Minute
	defaultOutboxRetry     = 30 * time.Second
	defaultOutboxInterval  = 5 * time.Second
)

type outboxWriter interface {
	UpsertSquad(ctx context.Context, squad *domain.Squad) error
	DeleteSquad(ctx context.Context, squad *domain.Squad) error
	UpsertAgent(ctx context.Context, agent *domain.Agent, identity *domain.AgentIdentity) error
	DeleteAgent(ctx context.Context, agent *domain.Agent) error
}

// RunOutboxWorker continuously applies durable Kubernetes intents until ctx is
// cancelled. It is intentionally small; the database lease gates concurrent API
// replicas so duplicate workers do not process the same event at once.
func RunOutboxWorker(ctx context.Context, store storage.KubernetesOutboxStore, writer outboxWriter) {
	ticker := time.NewTicker(defaultOutboxInterval)
	defer ticker.Stop()
	for {
		if _, err := ProcessOutboxOnce(ctx, store, writer); err != nil && ctx.Err() == nil {
			slog.Warn("process kubernetes outbox", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessOutboxOnce leases and applies one batch of pending/failed outbox
// events. It returns the number of leased events.
func ProcessOutboxOnce(ctx context.Context, store storage.KubernetesOutboxStore, writer outboxWriter) (int, error) {
	events, err := store.LeaseKubernetesOutbox(ctx, defaultOutboxBatchSize, defaultOutboxLease)
	if err != nil {
		return 0, err
	}
	for _, event := range events {
		if err := applyOutboxEvent(ctx, store, writer, event); err != nil {
			markErr := store.MarkKubernetesOutboxFailed(ctx, event.ID, err.Error(), retryDelay(event.Attempts))
			if markErr != nil {
				return len(events), fmt.Errorf("mark outbox event failed: %w", markErr)
			}
			continue
		}
		if err := store.MarkKubernetesOutboxApplied(ctx, event.ID); err != nil {
			return len(events), fmt.Errorf("mark outbox event applied: %w", err)
		}
	}
	return len(events), nil
}

// workspaceGrantReader provides the grant + registry lookups needed to derive
// an agent's workspace secrets at CR-apply time (ADR-0009). Both the memory
// and Postgres stores satisfy this.
type workspaceGrantReader interface {
	ListAgentPermissions(ctx context.Context, agentID string) ([]*domain.AgentPermission, error)
	GetResource(ctx context.Context, typ domain.ResourceType, id string) (*domain.RegistryResource, error)
}

// deriveWorkspaceSecrets maps an agent's active git workspace grants to
// Kubernetes Secret names. Stale grants (missing resource), non-git kinds,
// inactive resources, and unusable auth refs are skipped so one bad grant
// cannot block the whole CR sync.
func deriveWorkspaceSecrets(ctx context.Context, reader workspaceGrantReader, agent *domain.Agent) ([]domain.WorkspaceSecret, error) {
	perms, err := reader.ListAgentPermissions(ctx, agent.ID)
	if err != nil {
		return nil, fmt.Errorf("derive workspace secrets: list grants: %w", err)
	}
	secrets := []domain.WorkspaceSecret{}
	for _, perm := range perms {
		if perm.ResourceType != domain.ResProjectWorkspace {
			continue
		}
		res, err := reader.GetResource(ctx, domain.ResProjectWorkspace, perm.ResourceID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				slog.Warn("workspace grant points at missing resource; skipping", "agent", agent.ID, "resource", perm.ResourceID)
				continue
			}
			return nil, fmt.Errorf("derive workspace secrets: resource %s: %w", perm.ResourceID, err)
		}
		if res.Status != domain.ResourceActive {
			continue
		}
		var manifest struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(res.Manifest, &manifest); err != nil || manifest.Kind != "git" {
			continue
		}
		secretName := secretNameFromRef(res.AuthRef)
		if secretName == "" {
			slog.Warn("workspace resource has no usable auth_ref secret; skipping", "resource", res.ID)
			continue
		}
		secrets = append(secrets, domain.WorkspaceSecret{ResourceID: res.ID, SecretName: secretName})
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].ResourceID < secrets[j].ResourceID })
	return secrets, nil
}

// aiModelResolver resolves bound AI Model ids to their registry rows.
// Both the memory and Postgres stores satisfy this; the outbox worker uses
// it to derive the gateway-routable model names for the Agent CR (WP5).
type aiModelResolver interface {
	GetAIModel(ctx context.Context, id string) (*domain.AIModel, error)
}

// deriveBindingModelNames resolves the agent's primary/fallback AI Model
// ids into model names on the CR payload. Unresolvable ids are logged and
// left empty — since the WP8 step-4 cutover the CR writer no longer falls
// back to the legacy free-text default_model, so an unresolvable binding
// surfaces as an empty defaultModel (runtime: "SKQUAD_DEFAULT_MODEL is
// required") rather than silently serving stale config. One stale binding
// still must not block the whole CR sync (same posture as
// deriveWorkspaceSecrets).
func deriveBindingModelNames(ctx context.Context, resolver aiModelResolver, agent *domain.Agent) {
	if resolver == nil {
		return
	}
	if id := strings.TrimSpace(agent.AIModelID); id != "" {
		model, err := resolver.GetAIModel(ctx, id)
		if err != nil {
			slog.Warn("cannot resolve bound primary AI model; CR defaultModel left empty",
				"agent", agent.ID, "ai_model_id", id, "error", err)
		} else {
			agent.AIModelName = model.ModelName
		}
	}
	if id := strings.TrimSpace(agent.FallbackAIModelID); id != "" {
		model, err := resolver.GetAIModel(ctx, id)
		if err != nil {
			slog.Warn("cannot resolve bound fallback AI model; fallbackAiModelId carries no name",
				"agent", agent.ID, "fallback_ai_model_id", id, "error", err)
		} else {
			agent.FallbackAIModelName = model.ModelName
		}
	}
}

func applyOutboxEvent(ctx context.Context, store storage.KubernetesOutboxStore, writer outboxWriter, event *domain.KubernetesOutboxEvent) error {
	var payload domain.KubernetesOutboxPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode outbox payload: %w", err)
	}
	switch event.Operation {
	case domain.KubernetesOpUpsertSquad:
		if payload.Squad == nil {
			return fmt.Errorf("outbox event %s missing squad payload", event.ID)
		}
		return writer.UpsertSquad(ctx, payload.Squad)
	case domain.KubernetesOpDeleteSquad:
		if payload.Squad == nil {
			return fmt.Errorf("outbox event %s missing squad payload", event.ID)
		}
		return writer.DeleteSquad(ctx, payload.Squad)
	case domain.KubernetesOpUpsertAgent:
		if payload.Agent == nil {
			return fmt.Errorf("outbox event %s missing agent payload", event.ID)
		}
		return upsertAgentFromOutbox(ctx, store, writer, &payload)
	case domain.KubernetesOpDeleteAgent:
		if payload.Agent == nil {
			return fmt.Errorf("outbox event %s missing agent payload", event.ID)
		}
		return writer.DeleteAgent(ctx, payload.Agent)
	default:
		return fmt.Errorf("unknown kubernetes outbox operation %q", event.Operation)
	}
}

// upsertAgentFromOutbox enriches the agent payload with workspace grant
// secrets and bound model names before writing the CR. Extracted from
// applyOutboxEvent for cognitive complexity (S-126 / S3776).
func upsertAgentFromOutbox(ctx context.Context, store storage.KubernetesOutboxStore, writer outboxWriter, payload *domain.KubernetesOutboxPayload) error {
	if reader, ok := store.(workspaceGrantReader); ok {
		secrets, err := deriveWorkspaceSecrets(ctx, reader, payload.Agent)
		if err != nil {
			return err
		}
		payload.Agent.WorkspaceSecrets = secrets
	}
	if resolver, ok := store.(aiModelResolver); ok {
		deriveBindingModelNames(ctx, resolver, payload.Agent)
	}
	return writer.UpsertAgent(ctx, payload.Agent, payload.Identity)
}

func retryDelay(attempts int) time.Duration {
	delay := defaultOutboxRetry
	for i := 0; i < attempts; i++ {
		delay *= 2
		if delay >= 5*time.Minute {
			return 5 * time.Minute
		}
	}
	return delay
}
