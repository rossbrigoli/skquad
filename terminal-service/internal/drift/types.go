// Package drift implements the TG-11 slice D drift runner
// (docs/tool-gateway.md §6.7): a periodic, chart-gated batch job that
// runs `ansible-playbook --check` (check mode, NO mutation) for every
// artifact-enabled ssh resource × host_group at the pinned
// default-branch tip rev and reports the result to the control plane.
//
// Hosts whose check-mode run reports changed/failed/unreachable are OFF
// the approved state. The runner classifies per-host outcomes, posts one
// report per (resource, host_group) to the CP drift ingest endpoint,
// and lets the CP own the batched owner digest. The runner itself never
// notifies anyone and never mutates anything.
package drift

import "sort"

// HostGroup is one admin-declared inventory group from the resource
// ceiling (name + member hosts).
type HostGroup struct {
	Name  string   `json:"name"`
	Hosts []string `json:"hosts"`
}

// ArtifactResource is the runner-facing projection of an
// artifact-enabled ssh resource, as served by the CP
// GET /internal/v1/artifact-resources surface. Everything here comes
// from admin-managed registration — the runner has no way to inject a
// different repo, user or host set.
type ArtifactResource struct {
	ResourceID    string      `json:"resource_id"`
	Name          string      `json:"name"`
	GitURL        string      `json:"git_url"`
	DefaultBranch string      `json:"default_branch"`
	PlaybooksPath string      `json:"playbooks_path"`
	DriftPlaybook string      `json:"drift_playbook"`
	SSHUser       string      `json:"ssh_user"`
	KnownHosts    string      `json:"known_hosts"`
	HostGroups    []HostGroup `json:"host_groups"`
}

// Report is one drift-check outcome per (resource, host_group), POSTed
// to the CP ingest. DriftedHosts is sorted for deterministic payloads;
// InSync is the derived complement (len(DriftedHosts) == 0).
type Report struct {
	ResourceID   string   `json:"resource_id"`
	HostGroup    string   `json:"host_group"`
	Playbook     string   `json:"playbook"`
	GitRev       string   `json:"git_rev"`
	DriftedHosts []string `json:"drifted_hosts"`
	InSync       bool     `json:"in_sync"`
}

// NewReport builds a report with normalized (sorted, deduped) drifted
// hosts and the derived in_sync flag.
func NewReport(resourceID, hostGroup, playbook, gitRev string, drifted []string) Report {
	seen := map[string]bool{}
	out := make([]string, 0, len(drifted))
	for _, h := range drifted {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Strings(out)
	return Report{
		ResourceID:   resourceID,
		HostGroup:    hostGroup,
		Playbook:     playbook,
		GitRev:       gitRev,
		DriftedHosts: out,
		InSync:       len(out) == 0,
	}
}
