// TG-8 slice C2: context marker signalling that the confirmation gate
// (docs/tg8-grant-approvals-spec.md §C) has already been satisfied for
// this dispatch (auto via standing grant, approved_once, or
// approved_standing). Drivers that previously fail-closed on
// confirmation-required policies (the MCP driver's
// "confirmation_required" denial) may proceed when the marker is set.
//
// The marker is ONLY ever set by the gateway pipeline after a positive
// CP gate decision — never by agent input.
package drivers

import "context"

type confirmationSatisfiedKey struct{}

// WithConfirmationSatisfied returns ctx marked as confirmation-satisfied.
func WithConfirmationSatisfied(ctx context.Context) context.Context {
	return context.WithValue(ctx, confirmationSatisfiedKey{}, true)
}

// ConfirmationSatisfied reports whether ctx carries the satisfied marker.
func ConfirmationSatisfied(ctx context.Context) bool {
	v, _ := ctx.Value(confirmationSatisfiedKey{}).(bool)
	return v
}
