// S-155: provider API keys are pasted into the UI and stored as
// control-plane-managed Kubernetes Secrets. The public API is
// write-only for the key itself: create/update accept ``api_key``,
// every read returns only ``api_key_masked`` (last 5 chars) and
// ``has_api_key``. The providers table keeps the k8s:// ref plus the
// mask, so reads never touch the API server.

package httpapi

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
)

// ProviderKeyStore is the Secret backend for provider API keys. The
// production implementation is kube.SecretStore; tests inject fakes.
type ProviderKeyStore interface {
	EnsureProviderKey(ctx context.Context, name, key string) error
	GetProviderKey(ctx context.Context, name string) (string, error)
	DeleteProviderKey(ctx context.Context, name string) error
	RefFor(secretName string) string
}

// providerSecretName maps a provider id to its managed Secret name.
func providerSecretName(providerID string) string {
	return kube.ProviderSecretName(providerID)
}

// maskProviderKey renders the display-safe tail: bullets plus the last
// 5 characters. Keys of 5 chars or fewer are fully bulleted so a short
// key leaks nothing beyond its length.
func maskProviderKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) <= 5 {
		return strings.Repeat("•", len(r))
	}
	return "•••••" + string(r[len(r)-5:])
}

// providerJSON is the S-155 wire shape: api_key_ref and the key itself
// are never serialized. api_key_masked carries the stored tail.
func providerJSON(p *domain.AIProvider) map[string]any {
	return map[string]any{
		"id":             p.ID,
		"name":           p.Name,
		"kind":           p.Kind,
		"base_url":       p.BaseURL,
		"api_key_masked": p.APIKeyMask,
		"has_api_key":    strings.TrimSpace(p.APIKeyRef) != "",
		"status":         p.Status,
		"registered_by":  p.RegisteredBy,
		"created_at":     p.CreatedAt,
	}
}

func providerJSONList(providers []*domain.AIProvider) []map[string]any {
	out := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		out = append(out, providerJSON(p))
	}
	return out
}

// resolveProviderKey returns the live API key for a provider. Managed
// refs (k8s://ns/name) resolve through the Secret store; a non-managed
// non-empty ref is returned as-is for out-of-cluster dev only — the
// startup migration wraps every in-cluster literal, so production
// never hits that branch.
func (s *Server) resolveProviderKey(ctx context.Context, provider *domain.AIProvider) (string, error) {
	ref := strings.TrimSpace(provider.APIKeyRef)
	if ref == "" {
		return "", nil
	}
	if !kube.IsManagedRef(ref) {
		return ref, nil
	}
	if s.providerKeys == nil {
		return "", fmt.Errorf("provider key is stored in Kubernetes but the secret store is not configured")
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	return s.providerKeys.GetProviderKey(ctx, name)
}

// setProviderKey stores a pasted key in the managed Secret and updates
// the provider's ref+mask in place (caller persists the provider).
func (s *Server) setProviderKey(ctx context.Context, provider *domain.AIProvider, key string) error {
	if s.providerKeys == nil {
		return fmt.Errorf("kubernetes secret storage is not configured")
	}
	secretName := providerSecretName(provider.ID)
	if err := s.providerKeys.EnsureProviderKey(ctx, secretName, key); err != nil {
		return err
	}
	provider.APIKeyRef = s.providerKeys.RefFor(secretName)
	provider.APIKeyMask = maskProviderKey(key)
	return nil
}

// clearProviderKey removes the managed Secret and blanks ref+mask.
func (s *Server) clearProviderKey(ctx context.Context, provider *domain.AIProvider) {
	if s.providerKeys == nil || !kube.IsManagedRef(provider.APIKeyRef) {
		provider.APIKeyRef = ""
		provider.APIKeyMask = ""
		return
	}
	name := provider.APIKeyRef[strings.LastIndex(provider.APIKeyRef, "/")+1:]
	if err := s.providerKeys.DeleteProviderKey(ctx, name); err != nil {
		log.Printf("provider %s: delete managed key secret: %v", provider.ID, err)
	}
	provider.APIKeyRef = ""
	provider.APIKeyMask = ""
}

// MigrateLegacyProviderKeys wraps pre-S-155 literal api_key_ref values
// into managed Secrets and records the display mask. Idempotent: rows
// already behind a k8s:// ref (or keyless) are skipped. Returns the
// number of providers wrapped. Called once at startup from main; a
// failure on one row is logged and does not block the others (the row
// keeps working through the dev-fallback branch until the next attempt).
func MigrateLegacyProviderKeys(ctx context.Context, store Store, keys ProviderKeyStore) (int, error) {
	if keys == nil {
		return 0, nil
	}
	providers, err := store.ListAIProviders(ctx)
	if err != nil {
		return 0, err
	}
	wrapped := 0
	for _, p := range providers {
		ref := strings.TrimSpace(p.APIKeyRef)
		if ref == "" || kube.IsManagedRef(ref) {
			continue
		}
		if err := keys.EnsureProviderKey(ctx, providerSecretName(p.ID), ref); err != nil {
			log.Printf("provider key migration: provider %s: %v", p.ID, err)
			continue
		}
		p.APIKeyRef = keys.RefFor(providerSecretName(p.ID))
		p.APIKeyMask = maskProviderKey(ref)
		if _, err := store.UpdateAIProvider(ctx, p); err != nil {
			log.Printf("provider key migration: persist %s: %v", p.ID, err)
			continue
		}
		wrapped++
	}
	return wrapped, nil
}
