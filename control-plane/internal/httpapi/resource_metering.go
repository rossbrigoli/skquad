package httpapi

// S-269: per-resource metering API.
//
// Vertical slice exposing per-resource usage/cost rollups that were
// previously impossible on the control plane (metering aggregated only
// per squad / per agent / platform-wide). Design notes:
//
//   - Metering is OPTIONAL per resource (Ross's hard requirement).
//     A non-metered resource needs no metering config and EVERY call
//     to it costs 0 — the rollup reports that zero explicitly (the
//     `metered` flag distinguishes a billed zero from missing data).
//   - Usage (call counts) is recorded for ALL resources, metered or
//     not; cost is charged only when metered (cost = price_per_call
//     per call, fixed at event time so later price edits never rewrite
//     history — the same event-time snapshot philosophy as the WP5
//     rate snapshot). LLM-token attribution per resource is OUT of
//     scope for this slice.
//   - Aggregation mirrors the existing daily-rollup pattern
//     (storage.SumMeteringDaily): a read-time rollup over the retained
//     raw metering events. Retention therefore follows the existing
//     metering retention policy — no separate rollup table or new
//     retention stream (S-203 alignment).
//   - Usage reaches the CP via the gateway's resource-usage report on
//     the existing internal trust channel (same class as
//     /internal/v1/policy, /internal/v1/credentials and
//     /internal/v1/confirmation/*: NetworkPolicy-internal, no
//     app-layer auth). The CP resolves agent → squad and the
//     resource's metering config at ingest time.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// parseSinceParam parses the optional ?since=<RFC3339> query param.
// Empty/absent means all time (same contract as the agent metering
// endpoint, S-169). Returns ok=false with a 400 already written when
// the value is malformed.
func parseSinceParam(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("since"))
	if raw == "" {
		return time.Time{}, true
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_since", "since must be an RFC3339 timestamp")
		return time.Time{}, false
	}
	return parsed, true
}

// getResourceMetering serves GET /api/v1/resources/{resourceID}/metering
// ?since=<RFC3339>. Returns the per-resource rollup row for one
// registry resource: {resource_id, type, name, metered, calls, cost,
// currency, last_seen}. A resource with no recorded usage returns a
// zero row (calls=0, cost=0) rather than 404 — the resource exists, it
// just has no traffic in the window.
func (s *Server) getResourceMetering(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	resourceID := strings.TrimSpace(chi.URLParam(r, "resourceID"))
	resource, err := s.store.GetResourceByID(r.Context(), resourceID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	since, ok := parseSinceParam(w, r)
	if !ok {
		return
	}
	rows, err := s.store.SumMeteringByResource(r.Context(), since, resourceID, "")
	if err != nil {
		writeStorageError(w, err)
		return
	}
	row := zeroResourceMeteringRow(resource)
	for _, candidate := range rows {
		if candidate.ResourceID == resourceID {
			row = candidate
			break
		}
	}
	// Registry is authoritative for name/metered/currency on the
	// single-resource view (rollup rows carry the join-time snapshot).
	row.Name = resource.Name
	row.Metered = resource.Metered
	if resource.Currency != "" {
		row.Currency = resource.Currency
	}
	writeJSON(w, http.StatusOK, row)
}

// listResourceMetering serves GET /api/v1/metering/resources
// ?since=<RFC3339>&type=<resource type>. Returns per-resource rows
// for every resource with recorded usage in the window, optionally
// filtered by resource type.
func (s *Server) listResourceMetering(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	since, ok := parseSinceParam(w, r)
	if !ok {
		return
	}
	rows, err := s.store.SumMeteringByResource(r.Context(), since, "", strings.TrimSpace(r.URL.Query().Get("type")))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// zeroResourceMeteringRow synthesises the no-usage row for a registry
// resource so the single-resource endpoint always answers with the
// resource's identity + metering config.
func zeroResourceMeteringRow(resource *domain.RegistryResource) domain.ResourceMeteringRow {
	currency := resource.Currency
	if currency == "" {
		currency = "USD"
	}
	return domain.ResourceMeteringRow{
		ResourceID:   resource.ID,
		ResourceType: string(domain.CanonicalResourceType(resource.Type)),
		Name:         resource.Name,
		Metered:      resource.Metered,
		Currency:     currency,
	}
}

// resourceUsageReport is the gateway's per-call usage report posted to
// POST /internal/v1/resource-usage (internal trust class — see the
// package comment). Count defaults to 1.
type resourceUsageReport struct {
	ResourceID   string `json:"resource_id"`
	ResourceType string `json:"resource_type"`
	AgentID      string `json:"agent_id"`
	Count        int    `json:"count,omitempty"`
	Timestamp    string `json:"timestamp,omitempty"`
}

// ingestResourceUsage records one governed resource call (or a small
// batch of identical calls) as a metering event carrying the resource
// dimension. Cost is computed HERE, at event time, from the resource's
// metered flag and price: metered ⇒ price_per_call × count;
// non-metered ⇒ 0 (explicit billed zero, never missing data).
func (s *Server) ingestResourceUsage(w http.ResponseWriter, r *http.Request) {
	var report resourceUsageReport
	if !decodeJSON(w, r, &report) {
		return
	}
	report.ResourceID = strings.TrimSpace(report.ResourceID)
	report.AgentID = strings.TrimSpace(report.AgentID)
	if report.ResourceID == "" || report.AgentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "resource_id and agent_id are required")
		return
	}
	if report.Count == 0 {
		report.Count = 1
	}
	if report.Count < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "count must be positive")
		return
	}
	resource, err := s.store.GetResourceByID(r.Context(), report.ResourceID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	agent, err := s.store.GetAgent(r.Context(), report.AgentID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	ts := time.Now().UTC()
	if report.Timestamp != "" {
		parsed, err := time.Parse(time.RFC3339, report.Timestamp)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_timestamp", "timestamp must be an RFC3339 value")
			return
		}
		ts = parsed.UTC()
	}
	currency := resource.Currency
	if currency == "" {
		currency = "USD"
	}
	resourceType := strings.TrimSpace(report.ResourceType)
	if resourceType == "" {
		resourceType = string(domain.CanonicalResourceType(resource.Type))
	}
	// One event per call keeps the rollup's Calls == calls invariant
	// (the rollup counts events; gateway reports are per-call so this
	// loop is a no-op in the common case, count=1).
	for i := 0; i < report.Count; i++ {
		event := &domain.MeteringEvent{
			AgentID:      agent.ID,
			SquadID:      agent.SquadID,
			ProviderID:   "",
			Model:        "",
			Cost:         resourceCallCost(resource, 1),
			Currency:     currency,
			Timestamp:    ts.Add(time.Duration(i) * time.Millisecond),
			ResourceID:   resource.ID,
			ResourceType: resourceType,
		}
		if err := s.store.RecordMetering(r.Context(), event); err != nil {
			writeStorageError(w, err)
			return
		}
	}
	cost := resourceCallCost(resource, report.Count)
	_ = s.recordSystemAudit(r.Context(), "resource.usage.ingest", "resource", resource.ID, agent.SquadID,
		usageAuditJSON(map[string]any{"agent_id": agent.ID, "calls": report.Count, "metered": resource.Metered, "cost": cost}))
	writeJSON(w, http.StatusCreated, map[string]any{"recorded": true, "metered": resource.Metered, "cost": cost, "currency": currency})
}

// resourceCallCost applies the S-269 billing rule: a metered resource
// bills price_per_call per call; everything else is an explicit zero.
// A metered resource whose price is missing (defence in depth — the
// API boundary validates the pairing) bills zero rather than guessing.
func resourceCallCost(resource *domain.RegistryResource, count int) float64 {
	if !resource.Metered || resource.PricePerCall == nil {
		return 0
	}
	return *resource.PricePerCall * float64(count)
}

// usageAuditJSON marshals v, falling back to "{}" (audit metadata must
// never fail a request). Named distinctly from the package's mustJSON
// (map[string]string → string) helper.
func usageAuditJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// validateMeteringCreate validates the S-269 metering fields on a
// create: a metered resource must carry a price >= 0.
func validateMeteringCreate(w http.ResponseWriter, req *createRegistryResourceReq) bool {
	if req.PricePerCall != nil && *req.PricePerCall < 0 {
		writeError(w, http.StatusBadRequest, "negative_price", "price_per_call must be >= 0")
		return false
	}
	if req.Metered != nil && *req.Metered && req.PricePerCall == nil {
		writeError(w, http.StatusBadRequest, "metered_requires_price", "a metered resource requires price_per_call >= 0")
		return false
	}
	return true
}

// applyMeteringUpdate applies and validates the S-269 metering fields
// on an update. Effective values: provided fields override stored
// ones; the metered⇒price invariant is checked against the EFFECTIVE
// pair so a request cannot flip a resource to metered without a price
// (and un-metering keeps the stored price for a later re-enable).
func applyMeteringUpdate(w http.ResponseWriter, resource *domain.RegistryResource, req *updateRegistryResourceReq) bool {
	effMetered := resource.Metered
	if req.Metered != nil {
		effMetered = *req.Metered
	}
	effPrice := resource.PricePerCall
	if req.PricePerCall != nil {
		if *req.PricePerCall < 0 {
			writeError(w, http.StatusBadRequest, "negative_price", "price_per_call must be >= 0")
			return false
		}
		effPrice = req.PricePerCall
	}
	if effMetered && effPrice == nil {
		writeError(w, http.StatusBadRequest, "metered_requires_price", "a metered resource requires price_per_call >= 0")
		return false
	}
	resource.Metered = effMetered
	resource.PricePerCall = effPrice
	if req.Currency != nil {
		resource.Currency = strings.TrimSpace(*req.Currency)
	}
	return true
}
