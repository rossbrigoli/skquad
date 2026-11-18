package runner

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/rossbrigoli/skquad/build-service/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{
		Listen:               ":8080",
		Namespace:            "skquad-builds",
		KanikoImage:          "gcr.io/kaniko-project/executor:v1.23.2",
		KanikoServiceAccount: "skquad-kaniko",
		RegistrySecret:       "agent-builds-registry-creds",
		PrincipalsFile:       "/tmp/p.json",
		DestinationsAllow:    []string{"ghcr.io/rossbrigoli/agent-builds/"},
		GitHostsAllow:        []string{"github.com"},
		Limits:               config.Limits{MaxConcurrentPerPrincipal: 2, MaxPerHourPerPrincipal: 10},
		Job: config.JobConfig{
			ActiveDeadlineSeconds:   3600,
			TTLSecondsAfterFinished: 604800,
			CPURequest:              "500m",
			MemoryRequest:           "2Gi",
		},
	}
}

func spec() Spec {
	return Spec{
		ID:          "a1b2c3d4e5f60718",
		Principal:   "sherlock",
		Repo:        "github.com/rossbrigoli/skquad",
		Revision:    "main",
		Ref:         "refs/heads/main",
		Dockerfile:  "Dockerfile",
		Destination: "ghcr.io/rossbrigoli/agent-builds/skquad:demo-1",
		BuildArgs:   map[string]string{"VERSION": "0.1.0"},
	}
}

func TestBuildJobContextRefForms(t *testing.T) {
	// Full SHA revision (probe skipped -> Ref empty): fragment is the SHA.
	sha := "6e9f14bcb48f7daf5a706d77ad2c491220d65698"
	s := spec()
	s.Revision = sha
	s.Ref = ""
	job, err := BuildJob(s, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	want := "--context=git://github.com/rossbrigoli/skquad.git#" + sha
	if !strings.Contains(strings.Join(job.Spec.Template.Spec.Containers[0].Args, " "), want) {
		t.Fatalf("sha fragment missing: %v", job.Spec.Template.Spec.Containers[0].Args)
	}
	// Tag ref passes through unchanged.
	s2 := spec()
	s2.Revision = "v1.2.3"
	s2.Ref = "refs/tags/v1.2.3"
	job2, err := BuildJob(s2, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	want2 := "--context=git://github.com/rossbrigoli/skquad.git#refs/tags/v1.2.3"
	if !strings.Contains(strings.Join(job2.Spec.Template.Spec.Containers[0].Args, " "), want2) {
		t.Fatalf("tag ref missing: %v", job2.Spec.Template.Spec.Containers[0].Args)
	}
	// Bare name falls back to refs/heads/.
	s3 := spec()
	s3.Ref = ""
	s3.Revision = "dev"
	job3, err := BuildJob(s3, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	want3 := "--context=git://github.com/rossbrigoli/skquad.git#refs/heads/dev"
	if !strings.Contains(strings.Join(job3.Spec.Template.Spec.Containers[0].Args, " "), want3) {
		t.Fatalf("bare-name fallback missing: %v", job3.Spec.Template.Spec.Containers[0].Args)
	}
}

func TestBuildJobShape(t *testing.T) {
	job, err := BuildJob(spec(), testCfg())
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "build-a1b2c3d4e5f60718" || job.Namespace != "skquad-builds" {
		t.Fatalf("name/ns wrong: %s/%s", job.Namespace, job.Name)
	}
	if job.Labels[LabelBuildID] != "a1b2c3d4e5f60718" || job.Labels[LabelPrincipal] != "sherlock" {
		t.Fatalf("labels wrong: %v", job.Labels)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Fatal("backoffLimit must be 0 (single auditable attempt)")
	}
	if *job.Spec.ActiveDeadlineSeconds != 3600 || *job.Spec.TTLSecondsAfterFinished != 604800 {
		t.Fatal("job budget not propagated")
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != "Never" {
		t.Fatal("restart policy must be Never")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("kaniko pod must not automount SA token")
	}
	if pod.ServiceAccountName != "skquad-kaniko" {
		t.Fatalf("kaniko SA wrong: %s", pod.ServiceAccountName)
	}
	c := pod.Containers[0]
	if c.Image != "gcr.io/kaniko-project/executor:v1.23.2" {
		t.Fatalf("image wrong: %s", c.Image)
	}
	if c.SecurityContext == nil || *c.SecurityContext.Privileged {
		t.Fatal("kaniko must not be privileged")
	}
	if *c.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation must be off")
	}
	joined := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--context=git://github.com/rossbrigoli/skquad.git#refs/heads/main",
		"--dockerfile=Dockerfile",
		"--destination=ghcr.io/rossbrigoli/agent-builds/skquad:demo-1",
		"--build-arg=VERSION=0.1.0",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q in %q", want, joined)
		}
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/kaniko/.docker" || !c.VolumeMounts[0].ReadOnly {
		t.Fatalf("docker config mount wrong: %+v", c.VolumeMounts)
	}
	if pod.Volumes[0].Secret == nil || pod.Volumes[0].Secret.SecretName != "agent-builds-registry-creds" {
		t.Fatal("registry secret volume wrong")
	}
	items := pod.Volumes[0].Secret.Items
	if len(items) != 1 || items[0].Key != ".dockerconfigjson" || items[0].Path != "config.json" {
		t.Fatalf("dockerconfigjson key must be mapped to config.json for kaniko, got %+v", items)
	}
	if c.Resources.Requests.Cpu().String() != "500m" {
		t.Fatalf("cpu request wrong: %v", c.Resources.Requests)
	}
}

func TestBuildJobRejectsBadID(t *testing.T) {
	s := spec()
	s.ID = "BAD ID!"
	if _, err := BuildJob(s, testCfg()); err == nil {
		t.Fatal("invalid build id must be rejected")
	}
}

func TestExtractDigest(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"imageID":"ghcr.io/rossbrigoli/agent-builds/skquad@sha256:aaaabbbbccccddddeeeeffff000011112222333344445555666677778888999a"}`,
			"sha256:aaaabbbbccccddddeeeeffff000011112222333344445555666677778888999a"},
		{"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"Published digest: sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef ok",
			"sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		{"", ""},
		{"no digest here", ""},
	}
	for _, tc := range cases {
		if got := ExtractDigest(tc.in); got != tc.want {
			t.Fatalf("ExtractDigest(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestStatusFromJobStates(t *testing.T) {
	job, _ := BuildJob(spec(), testCfg())
	st := StatusFromJob(job, nil)
	if st.State != StateQueued {
		t.Fatalf("fresh job should be queued, got %s", st.State)
	}
	addCond(job, "Complete", "True")
	st = StatusFromJob(job, nil)
	if st.State != StateSucceeded {
		t.Fatalf("Complete -> succeeded, got %s", st.State)
	}
	if st.Repo != "github.com/rossbrigoli/skquad" || st.Destination != "ghcr.io/rossbrigoli/agent-builds/skquad:demo-1" {
		t.Fatalf("request metadata lost: %+v", st)
	}

	job2, _ := BuildJob(spec(), testCfg())
	addCond(job2, "Failed", "True")
	st2 := StatusFromJob(job2, nil)
	if st2.State != StateFailed {
		t.Fatalf("Failed -> failed, got %s", st2.State)
	}
}

func addCond(job *batchv1.Job, condType, status string) {
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type:   batchv1.JobConditionType(condType),
		Status: corev1.ConditionStatus(status),
	})
}

func TestSanitizeLabel(t *testing.T) {
	if got := SanitizeLabel("Sherlock Holmes!"); got != "sherlock-holmes" {
		t.Fatalf("sanitize wrong: %q", got)
	}
	if got := SanitizeLabel(strings.Repeat("x", 100)); len(got) > 63 {
		t.Fatalf("label too long: %d", len(got))
	}
	if got := SanitizeLabel("!!!"); got != "unknown" {
		t.Fatalf("empty-after-sanitize must become unknown: %q", got)
	}
}

func TestBuildJobSubdirDockerfileUsesContextSubPath(t *testing.T) {
	s := spec()
	s.Dockerfile = "build-service/Dockerfile"
	j, err := BuildJob(s, testCfg())
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	joined := strings.Join(j.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{
		"--context=git://github.com/rossbrigoli/skquad.git#refs/heads/main",
		"--context-sub-path=build-service",
		"--dockerfile=Dockerfile",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "--dockerfile=build-service/Dockerfile") {
		t.Fatalf("dockerfile must be rebased onto the sub-path context: %q", joined)
	}
}
