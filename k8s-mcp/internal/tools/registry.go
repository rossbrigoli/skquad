// Package tools implements the deliberately-scoped tool catalog exposed by
// the k8s-mcp server (MIG-7 / S-313).
//
// Scoping rules (all enforced here, at call time, deny-by-default):
//
//  1. Namespace allowlist — every Kubernetes read tool must name a
//     namespace from the configured set. No wildcards, no "all".
//  2. Resource deny list — secrets/configmaps/RBAC kinds can never be
//     fetched (also re-enforced inside the k8s client).
//  3. ArgoCD app allowlist — status/refresh/sync only for explicitly
//     permitted application names. An empty allowlist denies all gitops
//     operations.
//  4. No exec/attach/port-forward, no raw kubectl passthrough, no
//     arbitrary-verb or arbitrary-path tools exist at all.
//  5. Bounded output — log tails, event counts and message lengths are
//     capped so one tool result cannot flood the agent context.
//
// Policy denials are returned as tool results with IsError=true (visible
// to the agent as a clean "denied: ..." message), not as transport
// failures.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/rossbrigoli/skquad/k8s-mcp/internal/config"
	"github.com/rossbrigoli/skquad/k8s-mcp/internal/k8s"
)

// Result is a tool invocation outcome rendered into an MCP tool result.
type Result struct {
	Text    string
	IsError bool
}

// Handler executes a tool with decoded arguments.
type Handler func(ctx context.Context, args map[string]any) Result

// Tool is one catalog entry.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     Handler
}

// Registry holds the catalog in stable declaration order.
type Registry struct {
	tools  []Tool
	byName map[string]*Tool
	cfg    *config.Config
	k8s    *k8s.Client
}

// NewRegistry builds the catalog bound to config + k8s client.
func NewRegistry(cfg *config.Config, client *k8s.Client) *Registry {
	r := &Registry{cfg: cfg, k8s: client, byName: map[string]*Tool{}}
	for _, t := range []Tool{
		r.podsList(),
		r.servicesList(),
		r.eventsList(),
		r.podDescribe(),
		r.podLogs(),
		r.argoAppStatus(),
		r.argoAppRefresh(),
		r.argoAppSync(),
	} {
		r.tools = append(r.tools, t)
		cp := t
		r.byName[t.Name] = &cp
	}
	return r
}

// Tools returns the catalog in stable order (for tools/list).
func (r *Registry) Tools() []Tool { return r.tools }

// Call dispatches a tool invocation. The second return value is false
// when the tool does not exist (mapped to a JSON-RPC error by the MCP
// layer); denials are Result{IsError:true}, not "unknown tool".
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (Result, bool) {
	t, ok := r.byName[name]
	if !ok {
		return Result{}, false
	}
	return t.Handler(ctx, args), true
}

// ---------------------------------------------------------------------------
// argument helpers
// ---------------------------------------------------------------------------

var labelSelectorRe = regexp.MustCompile(`^[A-Za-z0-9._=/,\-()]+$`)
var revisionRe = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)

func strArg(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func boolArg(args map[string]any, key string) (bool, bool) {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return false, false
}

func intArg(args map[string]any, key string) (int, bool) {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case json.Number:
			i, err := n.Int64()
			if err == nil {
				return int(i), true
			}
		}
	}
	return 0, false
}

func denied(format string, a ...any) Result {
	return Result{Text: "denied: " + fmt.Sprintf(format, a...), IsError: true}
}

func failed(format string, a ...any) Result {
	return Result{Text: "error: " + fmt.Sprintf(format, a...), IsError: true}
}

func okJSON(v any) Result {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return failed("encoding result")
	}
	return Result{Text: string(b)}
}

func okText(s string) Result { return Result{Text: s} }

// checkNamespace enforces the namespace allowlist.
func (r *Registry) checkNamespace(ns string) (string, *Result) {
	if ns == "" {
		res := denied("namespace is required (no default namespace; fail-closed)")
		return "", &res
	}
	for _, allowed := range r.cfg.AllowedNamespaces {
		if ns == allowed {
			return ns, nil
		}
	}
	return "", ptrDenied("namespace %q is not in the allowed set %v", ns, r.cfg.AllowedNamespaces)
}

func ptrDenied(format string, a ...any) *Result {
	res := denied(format, a...)
	return &res
}

// checkArgoApp enforces the ArgoCD application allowlist.
func (r *Registry) checkArgoApp(app string) (string, *Result) {
	if app == "" {
		res := denied("app is required")
		return "", &res
	}
	if len(r.cfg.ArgoAllowedApps) == 0 {
		res := denied("no ArgoCD apps are allowlisted for this server (fail-closed)")
		return "", &res
	}
	for _, allowed := range r.cfg.ArgoAllowedApps {
		if app == allowed {
			return app, nil
		}
	}
	return "", ptrDenied("argocd app %q is not in the allowlist %v", app, r.cfg.ArgoAllowedApps)
}

func schema(props string, required ...string) json.RawMessage {
	req := "[]"
	if len(required) > 0 {
		parts := make([]string, 0, len(required))
		for _, s := range required {
			parts = append(parts, fmt.Sprintf("%q", s))
		}
		req = "[" + strings.Join(parts, ",") + "]"
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%s},"required":%s,"additionalProperties":false}`, props, req))
}

// ---------------------------------------------------------------------------
// read tools
// ---------------------------------------------------------------------------

func (r *Registry) podsList() Tool {
	return Tool{
		Name:        "k8s_pods_list",
		Description: "List pods (name, phase, ready, restarts, node) in an allowlisted namespace. Optional Kubernetes label selector.",
		InputSchema: schema(`"namespace":{"type":"string","description":"Target namespace (must be allowlisted)"},"label_selector":{"type":"string","description":"Optional Kubernetes label selector"}`, "namespace"),
		Handler: func(ctx context.Context, args map[string]any) Result {
			ns, deny := r.checkNamespace(strArg(args, "namespace"))
			if deny != nil {
				return *deny
			}
			sel := strArg(args, "label_selector")
			if sel != "" && !labelSelectorRe.MatchString(sel) {
				return denied("invalid label_selector characters")
			}
			raw, err := r.k8s.GetPods(ctx, ns, sel)
			if err != nil {
				return failed("listing pods: %v", err)
			}
			var list struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Spec struct {
						NodeName string `json:"nodeName"`
					} `json:"spec"`
					Status struct {
						Phase             string `json:"phase"`
						ContainerStatuses []struct {
							Ready        bool           `json:"ready"`
							RestartCount int            `json:"restartCount"`
							State        map[string]any `json:"state"`
						} `json:"containerStatuses"`
					} `json:"status"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return failed("parsing pod list")
			}
			rows := make([]map[string]any, 0, len(list.Items))
			for _, it := range list.Items {
				ready, restarts := 0, 0
				for _, cs := range it.Status.ContainerStatuses {
					if cs.Ready {
						ready++
					}
					restarts += cs.RestartCount
				}
				rows = append(rows, map[string]any{
					"name":     it.Metadata.Name,
					"phase":    it.Status.Phase,
					"ready":    fmt.Sprintf("%d/%d", ready, len(it.Status.ContainerStatuses)),
					"restarts": restarts,
					"node":     it.Spec.NodeName,
				})
			}
			return okJSON(map[string]any{"namespace": ns, "count": len(rows), "pods": rows})
		},
	}
}

func (r *Registry) servicesList() Tool {
	return Tool{
		Name:        "k8s_services_list",
		Description: "List services (name, type, clusterIP, ports) in an allowlisted namespace.",
		InputSchema: schema(`"namespace":{"type":"string"}`, "namespace"),
		Handler: func(ctx context.Context, args map[string]any) Result {
			ns, deny := r.checkNamespace(strArg(args, "namespace"))
			if deny != nil {
				return *deny
			}
			raw, err := r.k8s.GetServices(ctx, ns)
			if err != nil {
				return failed("listing services: %v", err)
			}
			var list struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Spec struct {
						Type      string `json:"type"`
						ClusterIP string `json:"clusterIP"`
						Ports     []struct {
							Name     string `json:"name"`
							Port     int    `json:"port"`
							Protocol string `json:"protocol"`
						} `json:"ports"`
					} `json:"spec"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return failed("parsing service list")
			}
			rows := make([]map[string]any, 0, len(list.Items))
			for _, it := range list.Items {
				rows = append(rows, map[string]any{
					"name":      it.Metadata.Name,
					"type":      it.Spec.Type,
					"clusterIP": it.Spec.ClusterIP,
					"ports":     it.Spec.Ports,
				})
			}
			return okJSON(map[string]any{"namespace": ns, "count": len(rows), "services": rows})
		},
	}
}

const (
	defaultEventLimit = 50
	maxEventLimit     = 200
	maxMessageChars   = 300
)

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}

func (r *Registry) eventsList() Tool {
	return Tool{
		Name:        "k8s_events_list",
		Description: fmt.Sprintf("List recent events in an allowlisted namespace (bounded; default %d, max %d).", defaultEventLimit, maxEventLimit),
		InputSchema: schema(`"namespace":{"type":"string"},"limit":{"type":"integer","description":"Max events (default 50, max 200)"}`, "namespace"),
		Handler: func(ctx context.Context, args map[string]any) Result {
			ns, deny := r.checkNamespace(strArg(args, "namespace"))
			if deny != nil {
				return *deny
			}
			limit := defaultEventLimit
			if v, ok := intArg(args, "limit"); ok && v > 0 {
				limit = v
			}
			if limit > maxEventLimit {
				limit = maxEventLimit
			}
			raw, err := r.k8s.GetEvents(ctx, ns, limit, "")
			if err != nil {
				return failed("listing events: %v", err)
			}
			return okJSON(map[string]any{"namespace": ns, "events": summarizeEvents(raw)})
		},
	}
}

func summarizeEvents(raw []byte) []map[string]any {
	var list struct {
		Items []struct {
			Type       string `json:"type"`
			Reason     string `json:"reason"`
			Message    string `json:"message"`
			Count      int    `json:"count"`
			LastTime   string `json:"lastTimestamp"`
			InvolvedOb struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	rows := make([]map[string]any, 0, len(list.Items))
	for _, e := range list.Items {
		rows = append(rows, map[string]any{
			"type":     e.Type,
			"reason":   e.Reason,
			"object":   e.InvolvedOb.Kind + "/" + e.InvolvedOb.Name,
			"message":  trunc(e.Message, maxMessageChars),
			"count":    e.Count,
			"lastTime": e.LastTime,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i]["lastTime"]) > fmt.Sprint(rows[j]["lastTime"])
	})
	return rows
}

func (r *Registry) podDescribe() Tool {
	return Tool{
		Name:        "k8s_pod_describe",
		Description: "Describe-equivalent summary of one pod: labels, containers/images, phase, conditions, container statuses, and recent events for that pod.",
		InputSchema: schema(`"namespace":{"type":"string"},"pod":{"type":"string"}`, "namespace", "pod"),
		Handler: func(ctx context.Context, args map[string]any) Result {
			ns, deny := r.checkNamespace(strArg(args, "namespace"))
			if deny != nil {
				return *deny
			}
			pod := strArg(args, "pod")
			if pod == "" {
				return denied("pod is required")
			}
			raw, err := r.k8s.GetPod(ctx, ns, pod)
			if err != nil {
				return failed("fetching pod: %v", err)
			}
			var p struct {
				Metadata struct {
					Name      string            `json:"name"`
					Namespace string            `json:"namespace"`
					Labels    map[string]string `json:"labels"`
					NodeName  string            `json:"node"`
				} `json:"metadata"`
				Spec struct {
					NodeName   string `json:"nodeName"`
					Containers []struct {
						Name      string `json:"name"`
						Image     string `json:"image"`
						Resources struct {
							Requests map[string]string `json:"requests"`
							Limits   map[string]string `json:"limits"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
				Status struct {
					Phase             string           `json:"phase"`
					PodIP             string           `json:"podIP"`
					Conditions        []map[string]any `json:"conditions"`
					ContainerStatuses []map[string]any `json:"containerStatuses"`
				} `json:"status"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return failed("parsing pod")
			}
			node := p.Spec.NodeName
			if node == "" {
				node = p.Metadata.NodeName
			}
			out := map[string]any{
				"name":      p.Metadata.Name,
				"namespace": p.Metadata.Namespace,
				"labels":    p.Metadata.Labels,
				"node":      node,
				"phase":     p.Status.Phase,
				"containers": map[string]any{
					"spec":   p.Spec.Containers,
					"status": p.Status.ContainerStatuses,
				},
				"conditions": p.Status.Conditions,
			}
			evRaw, err := r.k8s.GetEvents(ctx, ns, maxEventLimit, "involvedObject.name="+pod)
			if err == nil {
				out["events"] = summarizeEvents(evRaw)
			} else {
				out["events"] = []map[string]any{}
			}
			return okJSON(out)
		},
	}
}

func (r *Registry) podLogs() Tool {
	return Tool{
		Name:        "k8s_pod_logs",
		Description: fmt.Sprintf("Pod logs (plain text, bounded tail; default 100 lines, max %d). Optional container name.", r.cfg.MaxLogLines),
		InputSchema: schema(`"namespace":{"type":"string"},"pod":{"type":"string"},"container":{"type":"string"},"tail_lines":{"type":"integer","description":"Tail length (default 100, max from server config)"}`, "namespace", "pod"),
		Handler: func(ctx context.Context, args map[string]any) Result {
			ns, deny := r.checkNamespace(strArg(args, "namespace"))
			if deny != nil {
				return *deny
			}
			pod := strArg(args, "pod")
			if pod == "" {
				return denied("pod is required")
			}
			tail := 100
			if v, ok := intArg(args, "tail_lines"); ok && v > 0 {
				tail = v
			}
			if tail > r.cfg.MaxLogLines {
				tail = r.cfg.MaxLogLines
			}
			raw, err := r.k8s.GetPodLogs(ctx, ns, pod, strArg(args, "container"), tail)
			if err != nil {
				return failed("fetching logs: %v", err)
			}
			return okText(string(raw))
		},
	}
}

// ---------------------------------------------------------------------------
// gitops tools (ArgoCD, allowlisted apps only)
// ---------------------------------------------------------------------------

func (r *Registry) argoAppStatus() Tool {
	return Tool{
		Name:        "argocd_app_status",
		Description: "ArgoCD application status (sync status/revision, health, conditions). Only allowlisted apps; other apps are invisible to this server.",
		InputSchema: schema(`"app":{"type":"string","description":"ArgoCD application name (must be allowlisted)"}`, "app"),
		Handler: func(ctx context.Context, args map[string]any) Result {
			app, deny := r.checkArgoApp(strArg(args, "app"))
			if deny != nil {
				return *deny
			}
			raw, err := r.k8s.GetArgoApp(ctx, r.cfg.ArgoNamespace, app)
			if err != nil {
				return failed("fetching argocd app: %v", err)
			}
			var a struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					Sync struct {
						Status   string `json:"status"`
						Revision string `json:"revision"`
					} `json:"sync"`
					Health struct {
						Status string `json:"status"`
					} `json:"health"`
					Conditions     []map[string]any `json:"conditions"`
					OperationState map[string]any   `json:"operationState"`
					Resources      []map[string]any `json:"resources"`
				} `json:"status"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return failed("parsing argocd app")
			}
			opPhase := ""
			if a.Status.OperationState != nil {
				opPhase = fmt.Sprint(a.Status.OperationState["phase"])
			}
			return okJSON(map[string]any{
				"app":           a.Metadata.Name,
				"syncStatus":    a.Status.Sync.Status,
				"revision":      a.Status.Sync.Revision,
				"healthStatus":  a.Status.Health.Status,
				"lastOpPhase":   opPhase,
				"conditions":    a.Status.Conditions,
				"resourceCount": len(a.Status.Resources),
			})
		},
	}
}

func (r *Registry) argoAppRefresh() Tool {
	return Tool{
		Name:        "argocd_app_refresh",
		Description: "Trigger a hard refresh (re-read git state) of an allowlisted ArgoCD application. Does not sync.",
		InputSchema: schema(`"app":{"type":"string"}`, "app"),
		Handler: func(ctx context.Context, args map[string]any) Result {
			app, deny := r.checkArgoApp(strArg(args, "app"))
			if deny != nil {
				return *deny
			}
			patch := []byte(`{"operation":{"refresh":"hard"}}`)
			if _, err := r.k8s.PatchArgoApp(ctx, r.cfg.ArgoNamespace, app, patch); err != nil {
				return failed("refresh failed: %v", err)
			}
			return okText(fmt.Sprintf("refresh triggered for argocd app %q", app))
		},
	}
}

func (r *Registry) argoAppSync() Tool {
	return Tool{
		Name:        "argocd_app_sync",
		Description: "Sync an allowlisted ArgoCD application to its git state. Prune is NOT permitted (fail-closed); sync only applies/recreates, never deletes out-of-sync resources.",
		InputSchema: schema(`"app":{"type":"string"},"revision":{"type":"string","description":"Optional git revision (branch/sha); default HEAD"}`, "app"),
		Handler: func(ctx context.Context, args map[string]any) Result {
			app, deny := r.checkArgoApp(strArg(args, "app"))
			if deny != nil {
				return *deny
			}
			if prune, provided := boolArg(args, "prune"); provided && prune {
				return denied("prune=true is not permitted through this server")
			}
			rev := strArg(args, "revision")
			if rev != "" && (!revisionRe.MatchString(rev) || strings.Contains(rev, "..")) {
				return denied("invalid revision")
			}
			sync := map[string]any{"prune": false}
			if rev != "" {
				sync["revision"] = rev
			}
			patch, err := json.Marshal(map[string]any{"operation": map[string]any{"sync": sync}})
			if err != nil {
				return failed("encoding sync operation")
			}
			if _, err := r.k8s.PatchArgoApp(ctx, r.cfg.ArgoNamespace, app, patch); err != nil {
				return failed("sync failed: %v", err)
			}
			return okText(fmt.Sprintf("sync triggered for argocd app %q (prune=false)", app))
		},
	}
}
