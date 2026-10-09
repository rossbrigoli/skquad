package drift

import (
	"context"
	"log/slog"
	"time"

	"github.com/rossbrigoli/skquad/terminal-service/internal/apply"
)

// ResourceLister provides the artifact-enabled resources (CP-backed).
type ResourceLister interface {
	ListArtifactResources(ctx context.Context) ([]ArtifactResource, error)
}

// ReportPoster stores one drift report (CP-backed).
type ReportPoster interface {
	PostDriftReport(ctx context.Context, r Report) (string, error)
}

// Engine runs the check-mode applies. *apply.Engine satisfies it — the
// drift runner reuses the apply engine with CheckOnly=true and adds no
// engine logic of its own.
type Engine interface {
	Apply(ctx context.Context, req apply.Request) (*apply.Result, error)
}

// TipResolver resolves the current default-branch tip SHA for a repo
// (read-only, e.g. `git ls-remote`).
type TipResolver interface {
	TipRev(ctx context.Context, gitURL, branch string) (string, error)
}

// Runner executes one drift sweep: for every artifact-enabled resource,
// resolve the default-branch tip, then run a check-only apply per
// host_group and post the classified report.
//
// Failure policy (CronJob-friendly): a per-resource or per-group failure
// is logged and skipped — one broken repo or refused check never aborts
// the sweep. The run itself only errors when the resource listing fails
// (nothing to do and the CP is likely unhealthy; the CronJob should show
// a failed run in that case).
type Runner struct {
	Resources ResourceLister
	Poster    ReportPoster
	Engine    Engine
	Tips      TipResolver

	// DefaultPlaybook is used when a resource carries no
	// drift_playbook (SKQUAD_DRIFT_PLAYBOOK, default "site.yml").
	DefaultPlaybook string
	// CheckTimeout bounds one check-mode apply per group.
	CheckTimeout time.Duration
	// AgentID stamps the apply requests; drift runs are machine-driven.
	AgentID string

	// CAMint mints per-host short-lived SSH certs (same substrate as
	// the interactive/apply lanes). Nil ⇒ static-key transport.
	CAMint apply.CAMinter
	// SSHKeyPEM is the static identity key used with (or without) the
	// CA cert, mirroring apply.Request.StaticKeyPEM.
	SSHKeyPEM string
	// CertTTL bounds the minted drift certs.
	CertTTL time.Duration

	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
}

// Summary counts one sweep for logging/exit decisions.
type Summary struct {
	Resources int
	Checks    int   // reports posted
	Drifted   int   // reports with drifted hosts
	Skipped   int   // resource/group checks skipped on failure
}

func (r *Runner) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Run performs the full sweep. See Runner doc for the failure policy.
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	sum := Summary{}
	resources, err := r.Resources.ListArtifactResources(ctx)
	if err != nil {
		return sum, err
	}
	sum.Resources = len(resources)
	for _, res := range resources {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		tip, err := r.Tips.TipRev(ctx, res.GitURL, res.DefaultBranch)
		if err != nil {
			r.log().Warn("drift: tip resolution failed; skipping resource",
				"resource_id", res.ResourceID, "git_url", res.GitURL, "err", err)
			sum.Skipped++
			continue
		}
		playbook := res.DriftPlaybook
		if playbook == "" {
			playbook = r.DefaultPlaybook
		}
		for _, group := range res.HostGroups {
			if ctx.Err() != nil {
				return sum, ctx.Err()
			}
			report, ok := r.checkGroup(ctx, res, group, playbook, tip)
			if !ok {
				sum.Skipped++
				continue
			}
			if _, err := r.Poster.PostDriftReport(ctx, report); err != nil {
				r.log().Warn("drift: report post failed; skipping group",
					"resource_id", res.ResourceID, "host_group", group.Name, "err", err)
				sum.Skipped++
				continue
			}
			sum.Checks++
			if !report.InSync {
				sum.Drifted++
			}
		}
	}
	return sum, nil
}

// checkGroup runs one check-only apply and classifies the per-host
// outcomes. ok=false means no report was produced (engine error or a
// refusal — a refused check is not a drift signal, just a skipped
// check).
func (r *Runner) checkGroup(ctx context.Context, res ArtifactResource, group HostGroup, playbook, tip string) (Report, bool) {
	req := apply.Request{
		ResourceID:    res.ResourceID,
		AgentID:       r.agentID(),
		Playbook:      playbook,
		GitRev:        tip,
		HostGroup:     group.Name,
		Hosts:         group.Hosts,
		CheckOnly:     true,
		GitURL:        res.GitURL,
		DefaultBranch: res.DefaultBranch,
		PlaybooksPath: res.PlaybooksPath,
		SSHUser:       res.SSHUser,
		KnownHosts:    res.KnownHosts,
		CAMint:        r.CAMint,
		StaticKeyPEM:  r.SSHKeyPEM,
		CertTTL:       r.certTTL(),
		Timeout:       r.checkTimeout(),
	}
	result, err := r.Engine.Apply(ctx, req)
	if err != nil {
		r.log().Warn("drift: check apply failed; skipping group",
			"resource_id", res.ResourceID, "host_group", group.Name, "err", err)
		return Report{}, false
	}
	if result.Status != apply.StatusSucceeded && result.Status != apply.StatusFailed {
		// refused / anything non-terminal: no per-host truth, no report.
		r.log().Warn("drift: check did not run to completion; skipping group",
			"resource_id", res.ResourceID, "host_group", group.Name,
			"status", result.Status, "refusal_reason", result.RefusalReason)
		return Report{}, false
	}
	return NewReport(res.ResourceID, group.Name, playbook, tip, driftedHosts(result)), true
}

// driftedHosts classifies per-host check results: a host is drifted
// when its check-mode run reported changed tasks (off approved state)
// or failed/unreachable (cannot confirm conformance — fail closed).
// Skipped-only and ok-only hosts are in sync.
func driftedHosts(result *apply.Result) []string {
	var drifted []string
	for host, hr := range result.PerHost {
		if hr.Changed > 0 || hr.Failed > 0 || hr.Unreachable > 0 {
			drifted = append(drifted, host)
		}
	}
	return drifted
}

func (r *Runner) agentID() string {
	if r.AgentID != "" {
		return r.AgentID
	}
	return "drift-check"
}

func (r *Runner) certTTL() time.Duration {
	if r.CertTTL > 0 {
		return r.CertTTL
	}
	return 15 * time.Minute
}

func (r *Runner) checkTimeout() time.Duration {
	if r.CheckTimeout > 0 {
		return r.CheckTimeout
	}
	return 10 * time.Minute
}
