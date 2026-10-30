package runner

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/rossbrigoli/skquad/build-service/internal/config"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func parseQuantity(s string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return q, fmt.Errorf("invalid quantity %q", s)
	}
	return q, nil
}

// K8sRunner executes builds as kaniko Jobs via the Jobs API. The
// service SA's RBAC is namespace-scoped to cfg.Namespace: jobs
// create/get/list/watch/delete + pods get/list/watch. Nothing more.
type K8sRunner struct {
	client kubernetes.Interface
	cfg    *config.Config
}

// NewK8sRunner wires the runner to a clientset.
func NewK8sRunner(client kubernetes.Interface, cfg *config.Config) *K8sRunner {
	return &K8sRunner{client: client, cfg: cfg}
}

// Create submits the kaniko Job for an accepted build.
func (r *K8sRunner) Create(ctx context.Context, spec Spec) error {
	job, err := BuildJob(spec, r.cfg)
	if err != nil {
		return err
	}
	_, err = r.client.BatchV1().Jobs(r.cfg.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create kaniko job: %w", err)
	}
	return nil
}

// Get observes one build by id (label lookup, so it survives Job renames
// and works across restarts).
func (r *K8sRunner) Get(ctx context.Context, id string) (*Status, error) {
	if !BuildIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid build id")
	}
	sel := LabelBuildID + "=" + id
	jobs, err := r.client.BatchV1().Jobs(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	if len(jobs.Items) == 0 {
		return nil, nil
	}
	job := jobs.Items[0]
	pods, err := r.podsFor(ctx, id)
	if err != nil {
		r.logf("pods for build %s: %v", id, err)
	}
	return StatusFromJob(&job, pods), nil
}

// List returns builds for one principal (newest first by creation ts).
func (r *K8sRunner) List(ctx context.Context, principal string) ([]Status, error) {
	sel := LabelPrincipal + "=" + sanitizeLabel(principal)
	jobs, err := r.client.BatchV1().Jobs(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	out := make([]Status, 0, len(jobs.Items))
	for i := range jobs.Items {
		out = append(out, *StatusFromJob(&jobs.Items[i], nil))
	}
	return out, nil
}

// CountActive counts queued+running jobs for a principal (authoritative
// concurrency quota via the Jobs API).
func (r *K8sRunner) CountActive(ctx context.Context, principal string) (int, error) {
	sel := LabelPrincipal + "=" + sanitizeLabel(principal)
	jobs, err := r.client.BatchV1().Jobs(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range jobs.Items {
		j := jobs.Items[i]
		if jobComplete(&j) || jobFailed(&j) {
			continue
		}
		n++
	}
	return n, nil
}

func (r *K8sRunner) podsFor(ctx context.Context, id string) ([]corev1.Pod, error) {
	pods, err := r.client.CoreV1().Pods(r.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelBuildID + "=" + id,
	})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (r *K8sRunner) logf(format string, args ...any) {
	// Best-effort diagnostics; never leak secret material (there is none here).
	fmt.Printf("k8srunner: "+format+"\n", args...)
}
