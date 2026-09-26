package controller

// Workspace storage metrics (S-139). The operator already serves a
// Prometheus endpoint via controller-runtime's metrics server
// (--metrics-bind-address, wired in cmd/manager). These collectors are
// registered on the shared controller-runtime registry so they appear on
// the same /metrics endpoint — no new infra.

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// workspacePVCCreatedTotal counts workspace PVCs the operator created
	// (adoptions are not counted).
	workspacePVCCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "skquad_workspace_pvc_created_total",
		Help: "Number of per-agent workspace PVCs created by the operator.",
	})
	// workspacePVCPending is the number of labeled workspace PVCs observed
	// in Pending phase during the most recent orphan-GC sweep. A value
	// stuck > 0 for a long time means provisioning cannot satisfy a claim
	// (the alert hook for "Pending PVC").
	workspacePVCPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "skquad_workspace_pvc_pending",
		Help: "Per-agent workspace PVCs in Pending phase at the last sweep.",
	})
	// workspaceOrphansCleanedTotal counts orphaned workspace PVCs deleted
	// by the GC sweep (retain-annotated ones are reported, never counted
	// here).
	workspaceOrphansCleanedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "skquad_workspace_orphans_cleaned_total",
		Help: "Orphaned per-agent workspace PVCs deleted by the operator.",
	})
)

func init() {
	metrics.Registry.MustRegister(
		workspacePVCCreatedTotal,
		workspacePVCPending,
		workspaceOrphansCleanedTotal,
	)
}
