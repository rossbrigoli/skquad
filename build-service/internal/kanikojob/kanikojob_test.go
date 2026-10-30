package kanikojob

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rossbrigoli/skquad/build-service/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{
		Listen:               ":8090",
		Namespace:            "skquad-builds",
		KanikoImage:          "gcr.io/kaniko-project/executor:v1.24.0",
		KanikoServiceAccount: "skquad-kaniko",
		RegistrySecret:       "skquad-build-registry",
		PrincipalsFile:       "/tmp/p.json",
		DestinationsAllow:    []string{"docker.io/brigss007/agent-builds/"},
		GitHostsAllow:        []string{"github.com"},
		Limits:               config.Limits{MaxConcurrentPerPrincipal: 1, MaxPerHourPerPrincipal: 4},
		Job:                  config.JobConfig{ActiveDeadlineSeconds: 1800, TTLSecondsAfterFinished: 3600, CPURequest: "250m", MemoryRequest: "512Mi"},
	}
}

func TestRenderPinsRevisionAndFlags(t *testing.T) {
	cfg := testCfg()
	sha := strings.Repeat("a", 40)
	job, err := Render(cfg, "abc123", "sherlock", "https://github.com/o/r.git", sha, "Dockerfile", "docker.io/brigss007/agent-builds/x:dev")
	if err != nil {
		t.Fatal(err)
	}
	if job.Namespace != "skquad-builds" || job.Name != "bld-abc123" {
		t.Fatalf("bad name/ns: %s %s", job.Name, job.Namespace)
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != "skquad-kaniko" {
		t.Fatal("kaniko SA not set")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("kaniko pod must not automount SA token")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("restart policy must be Never")
	}
	c := pod.Containers[0]
	joined := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--git=repo=https://github.com/o/r.git,revision=" + sha,
		"--dockerfile=Dockerfile",
		"--destination=docker.io/brigss007/agent-builds/x:dev",
		"--digest-file=/dev/termination-log",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q in %q", want, joined)
		}
	}
	if !strings.Contains(joined, "--context=git://github.com/o/r.git") {
		t.Fatalf("context arg wrong: %q", joined)
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation must be disallowed")
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) == 0 {
		t.Fatal("capabilities must be dropped")
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/kaniko/.docker" || !c.VolumeMounts[0].ReadOnly {
		t.Fatal("registry config mount wrong")
	}
	if pod.Volumes[0].Secret == nil || pod.Volumes[0].Secret.SecretName != "skquad-build-registry" {
		t.Fatal("registry secret volume wrong")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 1800 {
		t.Fatal("active deadline not set")
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Fatal("ttl not set")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatal("backoff must be 0")
	}
	if job.Labels[LabelBuildID] != "abc123" || job.Labels[LabelPrincipal] != "sherlock" || job.Labels[LabelApp] != AppValue {
		t.Fatalf("labels wrong: %+v", job.Labels)
	}
	if job.Annotations["skquad.io/revision"] != sha {
		t.Fatal("revision annotation missing")
	}
}

func TestRenderRejectsNonFullSHA(t *testing.T) {
	cfg := testCfg()
	if _, err := Render(cfg, "abc", "p", "https://x/y/z.git", "main", "D", "d:1"); err == nil {
		t.Fatal("must reject non-SHA revision")
	}
}

func TestMapStatus(t *testing.T) {
	job := &batchv1.Job{Status: batchv1.JobStatus{}}
	if got := MapStatus(job); got != StatusPending {
		t.Fatalf("fresh job: %s", got)
	}
	job.Status.StartTime = &metav1.Time{}
	if got := MapStatus(job); got != StatusRunning {
		t.Fatalf("started job: %s", got)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if got := MapStatus(job); got != StatusSucceeded {
		t.Fatalf("complete: %s", got)
	}
	fail := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
	if got := MapStatus(fail); got != StatusFailed {
		t.Fatalf("failed: %s", got)
	}
	if got := MapStatus(nil); got != StatusPending {
		t.Fatalf("nil: %s", got)
	}
}

func terminatedPod(msg string) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "kaniko",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}},
	}}}}
}

func TestExtractDigestFromPod(t *testing.T) {
	good := "sha256:" + strings.Repeat("c", 64)
	if got := ExtractDigestFromPod(terminatedPod("INFO[0042] " + good)); got != good {
		t.Fatalf("got %q", got)
	}
	if got := ExtractDigestFromPod(terminatedPod("no digest here")); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := ExtractDigestFromPod(terminatedPod("sha256:tooshort")); got != "" {
		t.Fatalf("expected empty for malformed, got %q", got)
	}
	if got := ExtractDigestFromPod(nil); got != "" {
		t.Fatal("nil pod → empty")
	}
	// Wrong container name must be ignored.
	p := terminatedPod("sha256:" + strings.Repeat("d", 64))
	p.Status.ContainerStatuses[0].Name = "other"
	if got := ExtractDigestFromPod(p); got != "" {
		t.Fatalf("wrong container must be ignored, got %q", got)
	}
}

func TestSanitizeLabel(t *testing.T) {
	if got := sanitizeLabel("Sherlock Holmes!"); got != "sherlock-holmes" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("a", 100)
	if got := sanitizeLabel(long); len(got) > 63 {
		t.Fatalf("label too long: %d", len(got))
	}
	if got := sanitizeLabel("!!!"); got != "principal" {
		t.Fatalf("got %q", got)
	}
}
