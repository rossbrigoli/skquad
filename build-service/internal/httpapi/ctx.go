package httpapi

import (
	"context"

	"github.com/rossbrigoli/skquad/build-service/internal/principal"
)

func contextWithPrincipal(ctx context.Context, p *principal.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}
