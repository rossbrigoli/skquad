// Package apply implements the TG-11 artifact executor (docs/tool-gateway.md
// §6.7): mutation via approved change artifacts, not keystrokes. A playbook
// is only runnable when its git revision is reachable from the resource's
// registered default branch (merge = human approval), it passes the lint
// gate, and every host it touches lives in an approved host_group.
package apply

import (
	"context"
	"time"
)

// Refusal codes are stable, client-safe strings recorded on refused applies.
const (
	RefusalRevNotFound = "rev_not_found"
	RefusalNotMerged   = "artifact_not_merged"
	RefusalNotTip      = "not_default_branch_tip"
	RefusalLintFailed  = "lint_failed"
	RefusalBadPlaybook = "playbook_not_found"
	RefusalEmptyHosts  = "empty_host_group"
	RefusalCertMint    = "cert_mint_failed"
)

// Status values for an apply job.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusRefused   = "refused"
)

// Request is one artifact apply. GitURL comes from the RESOURCE registration
// only — the tool call cannot point the executor at an arbitrary repo.
type Request struct {
	ApplyID    string
	ResourceID string
	AgentID    string
	Playbook   string // path relative to PlaybooksPath, no traversal
	GitRev     string // full 40-char lowercase SHA (validated upstream)
	HostGroup  string
	Hosts      []string // resolved from the ceiling's host_group
	CheckOnly  bool
	RequireTip bool

	// Repo
	GitURL        string
	DefaultBranch string
	PlaybooksPath string

	// SSH transport (mirrors the TG-10 exec substrate)
	SSHUser    string
	KnownHosts string
	CertTTL    time.Duration
	// CAMint mints a per-host short-lived cert; nil ⇒ StaticKeyPEM used.
	CAMint       CAMinter
	StaticKeyPEM string

	// Lint inputs
	MirrorsAllow []string
	DenyPatterns []string // ceiling command_deny, reused from TG-10 semantics

	Timeout time.Duration
}

// HostResult mirrors the ansible JSON callback stats block per host.
type HostResult struct {
	OK          int `json:"ok"`
	Changed     int `json:"changed"`
	Failed      int `json:"failed"`
	Unreachable int `json:"unreachable"`
	Skipped     int `json:"skipped"`
}

// LintFinding is one lint gate result. All findings are stored even when the
// apply proceeds (audit honesty over gate minimalism).
type LintFinding struct {
	Rule    string `json:"rule"`
	Task    string `json:"task"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// Result is the terminal outcome of an apply (or refusal).
type Result struct {
	Status        string                `json:"status"`
	RefusalReason string                `json:"refusal_reason,omitempty"`
	PerHost       map[string]HostResult `json:"per_host,omitempty"`
	LintFindings  []LintFinding         `json:"lint_findings,omitempty"`
	RecordingID   string                `json:"recording_id,omitempty"`
	ExitCode      int                   `json:"exit_code"`
	StartedAt     time.Time             `json:"started_at"`
	FinishedAt    time.Time             `json:"finished_at"`
}

// CAMinter matches sshexec.CAMinter's shape (per-host ephemeral certs).
type CAMinter interface {
	Mint(ctx context.Context, user, host string, ttl time.Duration) (certPEM []byte, err error)
}

// Engine runs applies. Bin paths are injectable for tests.
type Engine struct {
	GitBin     string // default "git"
	AnsibleBin string // default "ansible-playbook"
	// WorkRoot is where temp clone/work dirs are created (default: os.TempDir).
	WorkRoot string
	// NewRecorder returns a recorder for the run; nil disables recording.
	NewRecorder func(applyID string, meta map[string]any) (Recorder, error)
}

// Recorder is the subset of recorder.Recorder the engine uses.
type Recorder interface {
	AppendOut(data []byte) error
	Flush(ctx context.Context) error
	Close(ctx context.Context) error
	RecordingID() string
}
