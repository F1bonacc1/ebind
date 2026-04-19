package worker

import (
	"context"
	"strings"
)

// ClaimProvider returns the logical targets currently owned by a worker.
//
// The returned claims may change over time. Workers refresh their claim set
// periodically and also re-check ownership before invoking a targeted task.
//
// Examples of claims include role-like values such as "primary" or
// application-scoped claims like "cluster/a/primary".
type ClaimProvider interface {
	Claims(ctx context.Context) ([]string, error)
}

// ClaimsFunc adapts a function to ClaimProvider.
type ClaimsFunc func(ctx context.Context) ([]string, error)

func (f ClaimsFunc) Claims(ctx context.Context) ([]string, error) { return f(ctx) }

// StaticClaims is a fixed claim set useful in tests and simple deployments.
type StaticClaims []string

func (s StaticClaims) Claims(context.Context) ([]string, error) {
	out := make([]string, 0, len(s))
	seen := map[string]struct{}{}
	for _, claim := range s {
		claim = strings.TrimSpace(claim)
		if claim == "" {
			continue
		}
		if _, ok := seen[claim]; ok {
			continue
		}
		seen[claim] = struct{}{}
		out = append(out, claim)
	}
	return out, nil
}

// ConcreteTarget names the stable, concrete placement claim for a specific
// worker instance. Colocated workflow steps resolve to this claim.
func ConcreteTarget(workerID string) string {
	return "worker:" + workerID
}
