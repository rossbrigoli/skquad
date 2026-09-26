package controller

// Orphaned workspace PVC garbage collection (S-139).
//
// A workspace PVC is "orphaned" when it carries the platform marker
// label (skquad.io/workspace-pvc="true") inside a squad namespace that
// the platform manages (a Squad CR exists for it), but no Agent CR with
// the matching skquad.io/agent-id label exists any more — e.g. the Agent
// was force-deleted past its finalizer, or a restore left claims behind.
//
// Guards (deliberately narrow — never touch unrelated PVCs):
//  1. only PVCs labeled skquad.io/workspace-pvc="true",
//  2. only inside namespaces that host a Squad CR (SquadNamespace of a
//     listed Squad),
//  3. only when the PVC's agent-id label matches NO live Agent CR,
//  4. never while a Deployment in the namespace still references the
//     claim by name (a live pod may be mounting it),
//  5. PVCs annotated skquad.io/retain-pvc="true" are reported, never
//     deleted.
//
// The sweep runs once at operator startup and then periodically
// (interval: SKQUAD_ORPHAN_PVC_SWEEP_INTERVAL_MINUTES, default 15).
// It is a leader-elected manager Runnable, so only the leader collects.

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

// OrphanPVCGC sweeps orphaned agent workspace PVCs.
type OrphanPVCGC struct {
	client.Client
	// Recorder is optional (nil-safe) and emits events on the PVC for
	// deleted/retained orphans.
	Recorder record.EventRecorder
}

// OrphanSweepReport summarises one sweep pass.
type OrphanSweepReport struct {
	Scanned  int
	Cleaned  int
	Retained int
	// SkippedInUse: PVCs still referenced by a Deployment in the
	// namespace (mounted or pending pod) — left alone.
	SkippedInUse int
}

// SweepOrphanWorkspacePVCs runs one GC pass and returns the report.
func (g *OrphanPVCGC) SweepOrphanWorkspacePVCs(ctx context.Context) (OrphanSweepReport, error) {
	log := ctrl.LoggerFrom(ctx).WithName("workspace-pvc-gc")
	report := OrphanSweepReport{}

	// Guard 2: only namespaces the platform manages via Squad CRs.
	var squads skquadv1.SquadList
	if err := g.List(ctx, &squads); err != nil {
		return report, fmt.Errorf("list squads: %w", err)
	}
	managedNamespaces := make(map[string]struct{}, len(squads.Items))
	for i := range squads.Items {
		managedNamespaces[SquadNamespace(&squads.Items[i])] = struct{}{}
	}
	if len(managedNamespaces) == 0 {
		return report, nil
	}

	// Live agent identities: every Agent CR's spec.agentId. Agents live
	// in the operator namespace; the label on the PVC is the link.
	liveAgentIDs, err := g.liveAgentIDs(ctx)
	if err != nil {
		return report, err
	}

	pending := 0
	for ns := range managedNamespaces {
		// Guard 1: only labeled workspace PVCs.
		var pvcs corev1.PersistentVolumeClaimList
		if err := g.List(ctx, &pvcs, client.InNamespace(ns), client.MatchingLabels{
			LabelWorkspacePVC: "true",
		}); err != nil {
			return report, fmt.Errorf("list workspace pvc in %s: %w", ns, err)
		}
		// Guard 4: deployments still referencing claims by name.
		referenced, err := g.referencedClaims(ctx, ns)
		if err != nil {
			return report, err
		}
		for i := range pvcs.Items {
			pvc := &pvcs.Items[i]
			report.Scanned++
			if pvc.Status.Phase == corev1.ClaimPending {
				pending++
			}
			agentID := pvc.Labels[LabelAgentID]
			if agentID == "" {
				// Labeled workspace PVC without an agent-id: unknown
				// provenance, leave it alone.
				log.Info("workspace pvc has no agent-id label; skipping", "pvc", pvc.Namespace+"/"+pvc.Name)
				continue
			}
			if liveAgentIDs[agentID] {
				continue
			}
			if referenced[pvc.Name] {
				report.SkippedInUse++
				log.Info("orphan workspace pvc still referenced by a deployment; skipping",
					"pvc", pvc.Namespace+"/"+pvc.Name, "agentId", agentID)
				continue
			}
			// Guard 5: retain annotation is reported, never deleted.
			if pvc.GetAnnotations()[RetainPVCKeepAnnotation] == "true" {
				report.Retained++
				log.Info("orphan workspace pvc has retain annotation; NOT deleting",
					"pvc", pvc.Namespace+"/"+pvc.Name, "agentId", agentID)
				g.eventf(pvc, corev1.EventTypeNormal, "WorkspacePVCRetained",
					"orphaned workspace pvc %s is retained (annotation %s=true)", pvc.Name, RetainPVCKeepAnnotation)
				continue
			}
			if err := g.Delete(ctx, pvc); err != nil {
				return report, fmt.Errorf("delete orphan pvc %s/%s: %w", pvc.Namespace, pvc.Name, err)
			}
			report.Cleaned++
			workspaceOrphansCleanedTotal.Inc()
			log.Info("deleted orphaned workspace pvc",
				"pvc", pvc.Namespace+"/"+pvc.Name, "agentId", agentID)
			g.eventf(pvc, corev1.EventTypeNormal, "WorkspacePVCOrphanCleaned",
				"deleted orphaned workspace pvc %s (no Agent CR for agent-id %s)", pvc.Name, agentID)
		}
	}
	workspacePVCPending.Set(float64(pending))
	return report, nil
}

// liveAgentIDs collects spec.agentId of every Agent CR the operator sees.
func (g *OrphanPVCGC) liveAgentIDs(ctx context.Context) (map[string]bool, error) {
	var agents skquadv1.AgentList
	if err := g.List(ctx, &agents); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	live := make(map[string]bool, len(agents.Items))
	for i := range agents.Items {
		if agents.Items[i].Spec.AgentID != "" {
			live[agents.Items[i].Spec.AgentID] = true
		}
	}
	return live, nil
}

// referencedClaims returns claim names referenced by any Deployment pod
// template in the namespace (guard against deleting a PVC a live or
// pending pod is mounting).
func (g *OrphanPVCGC) referencedClaims(ctx context.Context, namespace string) (map[string]bool, error) {
	var deployments appsv1.DeploymentList
	if err := g.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", namespace, err)
	}
	refs := map[string]bool{}
	for i := range deployments.Items {
		for _, vol := range deployments.Items[i].Spec.Template.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				refs[vol.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	return refs, nil
}

// eventf emits an event if a Recorder is wired; events are best-effort.
func (g *OrphanPVCGC) eventf(obj runtime.Object, eventType, reason, messageFmt string, args ...interface{}) {
	if g.Recorder != nil {
		g.Recorder.Eventf(obj, eventType, reason, messageFmt, args...)
	}
}
