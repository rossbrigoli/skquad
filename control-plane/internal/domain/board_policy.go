package domain

import (
	"errors"
	"strings"
)

// BoardPolicy is the squad-level board-policy document (ADR-0016 D8).
// Every field is a pointer so an unset field is distinguishable from an
// explicit zero/false value: effective values resolve
// agent_override ?? squad_default ?? platform_default at transition time,
// so config changes take effect immediately with no agent restart.
//
// The stored document defaults to '{}' — an empty document means "no
// squad-level overrides", i.e. the platform defaults apply.
type BoardPolicy struct {
	// ReopenCapPerDay bounds agent-initiated board.reopen calls per task
	// per rolling 24h (ADR-0016 D3). 0 disables agent reopens.
	ReopenCapPerDay *int `json:"reopen_cap_per_day,omitempty"`
	// SelfApproveDefault is the squad-level default for agents'
	// self_approve_allowed (ADR-0016 D2); a per-agent override wins.
	SelfApproveDefault *bool `json:"self_approve_default,omitempty"`
	// LeaderApprovalAllowed permits leader-capability agents (ADR-0016 D5)
	// to move in-review → done on tasks assigned to other agents in the
	// squad. Default false: approval is strictly human.
	LeaderApprovalAllowed *bool `json:"leader_approval_allowed,omitempty"`
	// LeaderGrants grants leader capabilities per agent (ADR-0016 D5),
	// keyed by agent ID. Default: empty — no agent in the squad holds any
	// leader capability. Grants are squad-owner-managed via the
	// board-policy PUT; the map is replaced wholesale on every PUT.
	LeaderGrants map[string]LeaderGrant `json:"leader_grants,omitempty"`
}

// LeaderGrant is one agent's leader-capability bundle (ADR-0016 D5).
type LeaderGrant struct {
	// CanPrioritize permits board.create_task(ready=true): the task is
	// created directly in TO DO (claimable) instead of BACKLOG.
	CanPrioritize bool `json:"can_prioritize"`
	// CanAssignOthers permits board.assign of a task to another agent in
	// the squad (self-assign needs no grant).
	CanAssignOthers bool `json:"can_assign_others"`
}

// Any reports whether the grant carries at least one capability. An
// all-false entry is equivalent to no grant at all.
func (g LeaderGrant) Any() bool { return g.CanPrioritize || g.CanAssignOthers }

// Platform defaults for board policy (ADR-0016 D2/D3/D8). Helm env
// (e.g. SKQUAD_REOPEN_CAP) only seeds these platform defaults; squad and
// agent levels override them.
const (
	PlatformReopenCapPerDay       = 3
	PlatformSelfApproveDefault    = true
	PlatformLeaderApprovalAllowed = false
)

// EffectiveBoardPolicy is a BoardPolicy with every platform default
// resolved — no pointers, safe to hand to transition guards.
type EffectiveBoardPolicy struct {
	ReopenCapPerDay       int  `json:"reopen_cap_per_day"`
	SelfApproveDefault    bool `json:"self_approve_default"`
	LeaderApprovalAllowed bool `json:"leader_approval_allowed"`
}

// Effective resolves the squad document against the platform defaults.
func (p BoardPolicy) Effective() EffectiveBoardPolicy {
	eff := EffectiveBoardPolicy{
		ReopenCapPerDay:       PlatformReopenCapPerDay,
		SelfApproveDefault:    PlatformSelfApproveDefault,
		LeaderApprovalAllowed: PlatformLeaderApprovalAllowed,
	}
	if p.ReopenCapPerDay != nil {
		eff.ReopenCapPerDay = *p.ReopenCapPerDay
	}
	if p.SelfApproveDefault != nil {
		eff.SelfApproveDefault = *p.SelfApproveDefault
	}
	if p.LeaderApprovalAllowed != nil {
		eff.LeaderApprovalAllowed = *p.LeaderApprovalAllowed
	}
	return eff
}

// Validate rejects policy documents the API must not store. Currently:
// reopen_cap_per_day must be >= 0 (0 = reopens disabled for agents).
func (p BoardPolicy) Validate() error {
	if p.ReopenCapPerDay != nil && *p.ReopenCapPerDay < 0 {
		return errors.New("reopen_cap_per_day must be >= 0")
	}
	for agentID := range p.LeaderGrants {
		if strings.TrimSpace(agentID) == "" {
			return errors.New("leader_grants keys must be non-empty agent IDs")
		}
	}
	return nil
}

// GrantFor returns the leader grant for an agent — the zero grant when the
// agent holds none (ADR-0016 D5 default: none).
func (p BoardPolicy) GrantFor(agentID string) LeaderGrant {
	if p.LeaderGrants == nil {
		return LeaderGrant{}
	}
	return p.LeaderGrants[agentID]
}

// CanPrioritize reports whether the agent may create tasks directly in
// TO DO (board.create_task ready=true, ADR-0016 D5).
func (p BoardPolicy) CanPrioritize(agentID string) bool { return p.GrantFor(agentID).CanPrioritize }

// CanAssignOthers reports whether the agent may assign tasks to other
// squad members (board.assign, ADR-0016 D5).
func (p BoardPolicy) CanAssignOthers(agentID string) bool { return p.GrantFor(agentID).CanAssignOthers }

// IsLeader reports whether the agent holds ANY leader capability
// (ADR-0016 D5). An all-false grant is not a leader.
func (p BoardPolicy) IsLeader(agentID string) bool { return p.GrantFor(agentID).Any() }

// EffectiveSelfApprove resolves the self-approval knob for an agent in a
// squad: agent override ?? squad default ?? platform true (ADR-0016 D2).
// Nil agent or nil squad fields fall through to the next level.
func EffectiveSelfApprove(agent *Agent, squad *Squad) bool {
	if agent != nil && agent.SelfApproveAllowed != nil {
		return *agent.SelfApproveAllowed
	}
	if squad != nil && squad.BoardPolicy.SelfApproveDefault != nil {
		return *squad.BoardPolicy.SelfApproveDefault
	}
	return PlatformSelfApproveDefault
}

// EffectiveReopenCap resolves the agent reopen cap for a squad:
// squad override ?? platform 3 (ADR-0016 D3).
func EffectiveReopenCap(squad *Squad) int {
	if squad != nil && squad.BoardPolicy.ReopenCapPerDay != nil {
		return *squad.BoardPolicy.ReopenCapPerDay
	}
	return PlatformReopenCapPerDay
}

// EffectiveLeaderApproval resolves whether leader agents may approve
// other agents' in-review tasks: squad override ?? platform false
// (ADR-0016 D5/D8).
func EffectiveLeaderApproval(squad *Squad) bool {
	if squad != nil && squad.BoardPolicy.LeaderApprovalAllowed != nil {
		return *squad.BoardPolicy.LeaderApprovalAllowed
	}
	return PlatformLeaderApprovalAllowed
}

// LeaderMayApprove resolves whether a specific agent may leader-approve
// (in-review → done) OTHER agents' tasks: the agent must hold a leader
// grant (D5) AND the squad must have leader_approval_allowed enabled
// (D8). Both knobs are required — capabilities alone never unlock
// approval. Nil agent or squad → false.
func LeaderMayApprove(agent *Agent, squad *Squad) bool {
	if agent == nil || squad == nil {
		return false
	}
	return squad.BoardPolicy.IsLeader(agent.ID) && EffectiveLeaderApproval(squad)
}
