// Package kanikojob renders the in-cluster kaniko build Job and maps
// Job status back to the build-service's build states.
//
// The kaniko pod is deliberately unprivileged in the governance sense:
//
//   - Its ServiceAccount has NO RBAC and token automount disabled —
//     kaniko builds from the git URL + registry secret only and can
//     never talk to the Kubernetes API.
//   - The registry push credential is mounted from a pre-provisioned
//     docker-config Secret (operator-managed, allowlist-scoped); the
//     agent never sees it.
//   - The git source is pinned to a FULL resolved SHA (never a moving
//     branch), so a scheduled Job cannot drift from what was approved
//     at request time.
//   - The digest is written to /dev/termination-log via --digest-file,
//     surfacing in job.status.terminalMessage without log scraping.
package kanikojob

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rossbrigoli/skquad/build-service/internal/config"
)

// Build states surfaced by GET /v1/builds/{id}.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Label keys stamped on every build Job.
const (
	LabelApp      = "app"
	LabelBuildID  = "skquad.io/build-id"
	LabelPrincipal = "skquad.io/build-principal"
	AppValue      = "skquad-build"
)

// DigestFile is where kaniko writes the pushed digest; wired to the
// container's termination message path.
const DigestFile = "/dev/termination-log"

// JobName derives the K8s Job name from a build id.
func JobName(buildID string) string { return "bld-" + strings.ToLower(buildID) }

// Render builds the kaniko Job object for one approved build.
func Render(cfg *config.Config, buildID, principalID, gitURL, fullSHA, dockerfile, destination string) (*batchv1.Job, error) {
	if buildID == "" || principalID == "" {
		return nil, fmt.Errorf("kanikojob: buildID and principalID required")
	}
	if fullSHA == "" || !IsSHA(fullSHA) {
		return nil, fmt.Errorf("kanikojob: full resolved SHA required (got %q)", fullSHA)
	}
	args := []string{
		"--git=repo=" + gitURL + ",revision=" + fullSHA,
		"--context=git://" + strings.TrimPrefix(gitURL, "https://"),
		"--dockerfile=" + dockerfile,
		"--destination=" + destination,
		"--digest-file=" + DigestFile,
		"--verbosity=info",
	}

	grace := int64(30)
	labels := map[string]string{
		LabelApp:       AppValue,
		LabelBuildID:   buildID,
		LabelPrincipal: sanitizeLabel(principalID),
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName(buildID),
			Namespace: cfg.Namespace,
			Labels:  labels,
			Annotations: map[string]string{
				"skquad.io/destination": destination,
				"skquad.io/revision":    fullSHA,
			},
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   ptr(cfg.Job.ActiveDeadlineSeconds),
			TTLSecondsAfterFinished: ptr(int32(cfg.Job.TTLSecondsAfterFinished)),
			BackoffLimit:            ptr(int32(0)), // one attempt; a retry would re-resolve mutable state
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            cfg.KanikoServiceAccount,
					AutomountServiceAccountToken:  boolPtr(false),
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: &grace,
					Containers: []corev1.Container{{
						Name:  "kaniko",
						Image: cfg.KanikoImage,
						Args:  args,
						SecurityContext: &corev1.SecurityContext{
							// kaniko needs root INSIDE its own container to
							// assemble layers; it needs no privileges, no
							// escalation, and drops all capabilities.
							RunAsUser:                ptr(int64(0)),
							RunAsNonRoot:             boolPtr(false),
							AllowPrivilegeEscalation: boolPtr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							ReadOnlyRootFilesystem:   boolPtr(false), // kaniko writes to /kaniko/fs
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    mustQuantity(cfg.Job.CPURequest, "100m"),
								corev1.ResourceMemory: mustQuantity(cfg.Job.MemoryRequest, "256Mi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "registry-config",
							MountPath: "/kaniko/.docker",
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "registry-config",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName:  cfg.RegistrySecret,
								DefaultMode: ptr(int32(0o400)),
							},
						},
					}},
				},
			},
		},
	}
	return job, nil
}

// MapStatus converts a Job's observed state into a build state.
func MapStatus(job *batchv1.Job) string {
	if job == nil {
		return StatusPending
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return StatusSucceeded
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return StatusFailed
		}
	}
	if job.Status.Active > 0 || job.Status.StartTime != nil {
		return StatusRunning
	}
	return StatusPending
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// IsSHA validates a full commit SHA.
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// ExtractDigestFromPod pulls the pushed image digest from the kaniko
// container's termination message (kaniko writes --digest-file to
// /dev/termination-log, which surfaces in pod.status, not job.status).
// Returns "" when absent or malformed.
func ExtractDigestFromPod(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "kaniko" || cs.State.Terminated == nil {
			continue
		}
		msg := strings.TrimSpace(cs.State.Terminated.Message)
		idx := strings.Index(msg, "sha256:")
		if idx < 0 {
			return ""
		}
		d := strings.TrimSpace(msg[idx:])
		if sp := strings.IndexAny(d, " \t\n"); sp >= 0 {
			d = d[:sp]
		}
		hexPart := strings.TrimPrefix(d, "sha256:")
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hexPart) {
			return ""
		}
		return d
	}
	return ""
}

// sanitizeLabel makes a principal id safe as a label value (alphanumerics,
// '-', '_', '.', max 63 chars).
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if len(out) > 63 {
		out = out[:63]
		out = strings.Trim(out, "-_.")
	}
	if out == "" {
		out = "principal"
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func boolPtr(v bool) *bool { return &v }

// mustQuantity parses a k8s quantity, falling back to def on parse
// error (config validation should have caught bad values; the fallback
// keeps a typo from crashing the render path).
func mustQuantity(s, def string) resource.Quantity {
	if q, err := resource.ParseQuantity(s); err == nil && !q.IsZero() {
		return q
	}
	q, err := resource.ParseQuantity(def)
	if err != nil {
		return resource.MustParse("100m")
	}
	return q
}
