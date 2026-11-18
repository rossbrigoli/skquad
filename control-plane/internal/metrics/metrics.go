// Package metrics holds the control-plane's Prometheus registry and the
// shared metric definitions (S-203 WP3-R3, plan WI-9).
//
// The control-plane previously exposed no metrics plumbing; this package
// is the minimal registry described in the remediation plan, served at
// GET /metrics on the main HTTP router (internal trust class — same
// reachability assumptions as /healthz; do not route it through the
// public ingress).
//
// Budget enforcement metrics are the observability half of the
// fail-closed posture: a money control that fails closed without
// counters fails mysteriously. See docs/adr/0017-budget-admission-
// control.md for what each series means operationally.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry is the control-plane's Prometheus registry. The HTTP layer
// exposes it at /metrics; instrumentation code registers collectors
// here via init() below.
var Registry = prometheus.NewRegistry()

// Budget enforcement series (S-203 WP3-R3 / plan WI-9).
//
// Label vocabularies:
//   - source: "user" | "platform" (the two block sources); "unknown"
//     is used only where the source cannot be resolved at low cost.
//   - site (rejections): "send" | "wake" | "chokepoint".
//   - site (check errors): "admission" (control-plane start paths) |
//     "crsync" (outbox worker CR apply / idle-timeout override).
var (
	// BudgetBlocksCreated counts newly created budget blocks
	// (transitions into blocked, per source). Each block is a hard
	// stop at turn granularity for every agent of the affected users.
	BudgetBlocksCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "budget_blocks_created_total",
		Help: "New budget blocks created, by source (user|platform). A block stops the affected users' agents at end of the current turn.",
	}, []string{"source"})

	// TurnsRejectedBudget counts confirmed-block refusals of a turn
	// start at each rejection site. Verification failures are NOT
	// counted here — they are counted by BudgetCheckErrors (a rejected
	// turn whose cause is an outage belongs to the error series).
	TurnsRejectedBudget = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "turns_rejected_budget_total",
		Help: "Turn starts refused for a confirmed budget block, by source (user|platform|unknown) and site (send|wake|chokepoint).",
	}, []string{"source", "site"})

	// BudgetCheckErrors counts budget-check failures at both
	// fail-closed sites. Sustained non-zero means users are being
	// blocked by a system outage, not by their budget — page-worthy.
	BudgetCheckErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "budget_check_errors_total",
		Help: "Budget check failures, by site (admission|crsync). Both sites fail closed; sustained non-zero = outage-driven blocking, not budget exhaustion.",
	}, []string{"site"})

	// BudgetOverrunDollars observes the accepted overrun (spend beyond
	// the cap at block creation) in USD. Overrun is a bounded trade —
	// cap + one turn's cost — reported, never prevented or clamped.
	BudgetOverrunDollars = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "budget_overrun_dollars",
		Help:    "Accepted overrun beyond the cap at block creation, USD, by source (user|platform). Reported, never prevented.",
		Buckets: []float64{0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 1000},
	}, []string{"source"})

	// BudgetFanoutRemirrorDuration times the block/resume fan-out
	// (remirrorUserAgents: re-enqueue CR upserts across every squad
	// the user owns). Long tails here mean enforcement latency after a
	// budget change.
	BudgetFanoutRemirrorDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "budget_fanout_remirror_duration_seconds",
		Help:    "Duration of the block/resume CR fan-out (remirrorUserAgents) across a user's squads.",
		Buckets: prometheus.DefBuckets,
	})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		BudgetBlocksCreated,
		TurnsRejectedBudget,
		BudgetCheckErrors,
		BudgetOverrunDollars,
		BudgetFanoutRemirrorDuration,
	)
}
