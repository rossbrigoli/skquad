package controller

// S-319 / S-203 WP3-R1 (T1, operator half): the never-interrupt guarantee.
// The control-plane now fails CLOSED on budget-check errors — a blocked or
// unverifiable owner's CR is mirrored with idleTimeout=0 while the agent is
// still active. That is only safe because desiredReplicas() returns 1
// whenever Spec.DesiredActive is true, BEFORE any idle-timeout logic runs.
// These tests pin that ordering so a future refactor cannot silently turn a
// zero idle timeout into a mid-turn pod kill.

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

func int32ptr(v int32) *int32 { return &v }

func TestDesiredReplicasNeverInterruptsActiveAgent(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name          string
		desiredActive bool
		idleTimeout   string
		idleSince     *metav1.Time
		curReplicas   *int32
		want          int32
	}{
		{
			name:          "active agent with zero idle timeout stays up (fail-closed CR mid-turn)",
			desiredActive: true,
			idleTimeout:   "0s",
			curReplicas:   int32ptr(1),
			want:          1,
		},
		{
			name:          "active agent with empty idle timeout stays up",
			desiredActive: true,
			idleTimeout:   "",
			curReplicas:   int32ptr(1),
			want:          1,
		},
		{
			name:          "active agent with stale idleSince still stays up",
			desiredActive: true,
			idleTimeout:   "0s",
			idleSince:     &metav1.Time{Time: now.Add(-time.Hour)},
			curReplicas:   int32ptr(1),
			want:          1,
		},
		{
			name:          "inactive agent with zero idle timeout scales down immediately",
			desiredActive: false,
			idleTimeout:   "0s",
			curReplicas:   int32ptr(1),
			want:          0,
		},
		{
			name:          "inactive agent within idle timeout stays up",
			desiredActive: false,
			idleTimeout:   "10m",
			idleSince:     &metav1.Time{Time: now.Add(-time.Minute)},
			curReplicas:   int32ptr(1),
			want:          1,
		},
		{
			name:          "inactive agent past idle timeout scales down",
			desiredActive: false,
			idleTimeout:   "10m",
			idleSince:     &metav1.Time{Time: now.Add(-time.Hour)},
			curReplicas:   int32ptr(1),
			want:          0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := &skquadv1.Agent{
				Spec: skquadv1.AgentSpec{
					DesiredActive: tc.desiredActive,
					IdleTimeout:   tc.idleTimeout,
				},
			}
			if tc.idleSince != nil {
				agent.Status.IdleSince = *tc.idleSince
			}
			deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: tc.curReplicas}}
			got := desiredReplicas(agent, deployment, func() time.Time { return now })
			if got != tc.want {
				t.Fatalf("desiredReplicas = %d, want %d", got, tc.want)
			}
		})
	}
}

// updateIdleSince must clear IdleSince while the agent is active — the
// second half of the guarantee that a re-blocked-then-reactivated agent
// never inherits a stale idle clock.
func TestUpdateIdleSinceClearedWhileActive(t *testing.T) {
	now := time.Now()
	agent := &skquadv1.Agent{
		Spec: skquadv1.AgentSpec{DesiredActive: true, IdleTimeout: "0s"},
		Status: skquadv1.AgentStatus{
			IdleSince: metav1.Time{Time: now.Add(-time.Hour)},
		},
	}
	updateIdleSince(agent, func() time.Time { return now })
	if !agent.Status.IdleSince.IsZero() {
		t.Fatalf("IdleSince must be cleared while DesiredActive=true, got %v", agent.Status.IdleSince)
	}
}
