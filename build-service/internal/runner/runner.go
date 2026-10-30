// Package runner executes accepted builds as kaniko Jobs in the
// dedicated build namespace and maps Job/pod state back to build state.
//
// The kaniko Job is the durable record of an accepted build: labels and
// annotations carry the request metadata (principal, repo, revision,
// destination) into etcd, which is where audit can find it even if the
// service process dies. Terminal digest comes from the kaniko pod's
// termination message.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rossbrigoli/skquad/build-service/internal/config"
)

// Build states.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
)

// Label/annotation keys (all namespaced under skquad.io/).
const (
	LabelComponent = "skquad.io/component"
	LabelBuildID   = "skquad.io/build-id"
	LabelPrincipal = "skquad.io/principal"

	AnnotationRequest   = "skquad.io/build-request"
	AnnotationRevision  = "skquad.io/revision"
	AnnotationSubmitted = "skquad.io/submitted-at"
)

// Spec is an accepted build to execute.
type Spec struct {
	ID          string
	Principal   string
	Repo        string // canonical host/org/repo
	Revision    string
	Dockerfile  string
	Destination string
	BuildArgs   map[string]string
}

// Status is the observed state of a build.
type Status struct {
	ID          string    `json:"id"`
	Principal   string    `json:"principal"`
	Repo        string    `json:"repo"`
	Revision    string    `json:"revision"`
	Destination string    `json:"destination"`
	State       string    `json:"state"`
	Digest      string    `json:"digest,omitempty"`
	Message     string    `json:"message,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
}

// Runner executes and observes builds.
type Runner interface {
	Create(ctx context.Context, spec Spec) error
	Get(ctx context.Context, id string) (*Status, error)
	List(ctx context.Context, principal string) ([]Status, error)
	CountActive(ctx context.Context, principal string) (int, error)
}

// BuildIDPattern validates externally supplied build ids.
var BuildIDPattern = regexp.MustCompile(`^[a-z0-9]{8,32}$`)

// JobName maps a build id to its kaniko Job name.
func JobName(id string) string { return "build-" + id }

// BuildJob renders the kaniko Job for a spec. Pure function: fully
// unit-testable without a cluster.
//
// Hardening posture (mirrors the platform's least-privilege stance):
//   - privileged=false, allowPrivilegeEscalation=false (kaniko needs root
//     for filesystem snapshotting but never the privileged flag)
//   - automountServiceAccountToken=false on the kaniko pod: kaniko has
//     no business talking to the K8s API
//   - backoffLimit=0: no silent retries; every build is one auditable attempt
//   - activeDeadlineSeconds + ttlSecondsAfterFinished from config
//   - registry push credentials arrive only as the mounted docker-config
//     Secret; nothing agent-controlled touches them
func BuildJob(spec Spec, cfg *config.Config) (*batchv1.Job, error) {
	if !BuildIDPattern.MatchString(spec.ID) {
		return nil, fmt.Errorf("invalid build id %q", spec.ID)
	}
	args := []string{
		fmt.Sprintf("--git=repo=https://%s.git,revision=%s", spec.Repo, spec.Revision),
		fmt.Sprintf("--dockerfile=%s", spec.Dockerfile),
		fmt.Sprintf("--destination=%s", spec.Destination),
		// Write the pushed image digest to the termination message so the
		// service can return a content address to the agent.
		"--termination-log-path=/dev/termination-log",
	}
	for k, v := range spec.BuildArgs {
		args = append(args, fmt.Sprintf("--build-arg=%s=%s", k, v))
	}

	reqCPU := cfg.Job.CPURequest
	reqMem := cfg.Job.MemoryRequest
	resources := corev1.ResourceRequirements{}
	if reqCPU != "" || reqMem != "" {
		resources.Requests = corev1.ResourceList{}
		if reqCPU != "" {
			q, err := parseQuantity(reqCPU)
			if err != nil {
				return nil, fmt.Errorf("job.cpu_request: %w", err)
			}
			resources.Requests[corev1.ResourceCPU] = q
		}
		if reqMem != "" {
			q, err := parseQuantity(reqMem)
			if err != nil {
				return nil, fmt.Errorf("job.memory_request: %w", err)
			}
			resources.Requests[corev1.ResourceMemory] = q
		}
	}

	reqJSON, _ := json.Marshal(map[string]string{
		"repo":        spec.Repo,
		"revision":    spec.Revision,
		"dockerfile":  spec.Dockerfile,
		"destination": spec.Destination,
	})
	ttl32 := int32(cfg.Job.TTLSecondsAfterFinished)
	deadline := cfg.Job.ActiveDeadlineSeconds
	runAsNonRoot := false
	allowEsc := false

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName(spec.ID),
			Namespace: cfg.Namespace,
			Labels: map[string]string{
				LabelComponent: "build-service",
				LabelBuildID:   spec.ID,
				LabelPrincipal: sanitizeLabel(spec.Principal),
			},
			Annotations: map[string]string{
				AnnotationRequest:   string(reqJSON),
				AnnotationRevision:  spec.Revision,
				AnnotationSubmitted: time.Now().UTC().Format(time.RFC3339),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl32,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelComponent: "build-service",
						LabelBuildID:   spec.ID,
						LabelPrincipal: sanitizeLabel(spec.Principal),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            cfg.KanikoServiceAccount,
					AutomountServiceAccountToken:  boolPtr(false),
					TerminationGracePeriodSeconds: int64Ptr(5),
					Containers: []corev1.Container{{
						Name:  "kaniko",
						Image: cfg.KanikoImage,
						Args:  args,
						SecurityContext: &corev1.SecurityContext{
							Privileged:               boolPtr(false),
							AllowPrivilegeEscalation: &allowEsc,
							RunAsNonRoot:             &runAsNonRoot,
						},
						Resources:                resources,
						VolumeMounts:             []corev1.VolumeMount{kanikoDockerMount()},
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					}},
					Volumes: []corev1.Volume{{
						Name: "docker-config",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName:  cfg.RegistrySecret,
								DefaultMode: int32Ptr(0o400),
							},
						},
					}},
				},
			},
		},
	}
	return job, nil
}

func kanikoDockerMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      "docker-config",
		MountPath: "/kaniko/.docker",
		ReadOnly:  true,
	}
}

// StatusFromJob maps a Job (+ its pods) to a build Status. digest is
// extracted from the kaniko pod termination message.
func StatusFromJob(job *batchv1.Job, pods []corev1.Pod) *Status {
	st := &Status{State: StateQueued}
	if job == nil {
		return st
	}
	if v, ok := job.Labels[LabelBuildID]; ok {
		st.ID = v
	}
	if v, ok := job.Labels[LabelPrincipal]; ok {
		st.Principal = v
	}
	if v, ok := job.Annotations[AnnotationRevision]; ok {
		st.Revision = v
	}
	var req map[string]string
	if err := json.Unmarshal([]byte(job.Annotations[AnnotationRequest]), &req); err == nil {
		st.Repo = req["repo"]
		st.Destination = req["destination"]
	}
	if ts, err := time.Parse(time.RFC3339, job.Annotations[AnnotationSubmitted]); err == nil {
		st.SubmittedAt = ts
	} else {
		st.SubmittedAt = job.CreationTimestamp.Time
	}

	switch {
	case jobComplete(job):
		st.State = StateSucceeded
		st.FinishedAt = conditionTime(job, batchv1.JobComplete)
	case jobFailed(job):
		st.State = StateFailed
		st.FinishedAt = conditionTime(job, batchv1.JobFailed)
		st.Message = "build job failed (see kaniko pod logs)"
	case job.Status.Active > 0:
		st.State = StateRunning
	}

	for _, pod := range pods {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != "kaniko" || cs.State.Terminated == nil {
				continue
			}
			msg := cs.State.Terminated.Message
			if d := ExtractDigest(msg); d != "" {
				st.Digest = d
			}
			if cs.State.Terminated.ExitCode != 0 && st.State == StateFailed {
				st.Message = firstLine(msg)
			}
		}
	}
	return st
}

// ExtractDigest pulls the sha256 digest out of a kaniko termination
// message. Kaniko writes JSON like {"imageID":"reg/repo@sha256:..."};
// we also accept a bare "sha256:<hex>" or an embedded occurrence for
// robustness across kaniko versions.
func ExtractDigest(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	var tm struct {
		ImageID string `json:"imageID"`
	}
	if err := json.Unmarshal([]byte(msg), &tm); err == nil && tm.ImageID != "" {
		if d := findDigest(tm.ImageID); d != "" {
			return d
		}
	}
	return findDigest(msg)
}

// findDigest locates the first "sha256:" followed by 64 hex chars.
func findDigest(s string) string {
	const marker = "sha256:"
	for i := 0; ; {
		j := strings.Index(s[i:], marker)
		if j < 0 {
			return ""
		}
		j += i
		tail := s[j+len(marker):]
		if len(tail) >= 64 && isHex(tail[:64]) {
			return marker + tail[:64]
		}
		i = j + len(marker)
	}
}

func jobComplete(job *batchv1.Job) bool { return hasCondition(job, batchv1.JobComplete, "True") }
func jobFailed(job *batchv1.Job) bool   { return hasCondition(job, batchv1.JobFailed, "True") }

func hasCondition(job *batchv1.Job, t batchv1.JobConditionType, want string) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && string(c.Status) == want {
			return true
		}
	}
	return false
}

func conditionTime(job *batchv1.Job, t batchv1.JobConditionType) time.Time {
	for _, c := range job.Status.Conditions {
		if c.Type == t {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// sanitizeLabel coerces a principal id to a valid K8s label value.
func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > 63 {
		out = out[:63]
		out = strings.Trim(out, "-._")
	}
	if out == "" {
		out = "unknown"
	}
	return out
}

// SanitizeLabel is the exported wrapper used by the HTTP layer to match
// principal label values returned by the runner.
func SanitizeLabel(s string) string { return sanitizeLabel(s) }

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
