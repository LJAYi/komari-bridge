package provider

import (
	"context"

	"github.com/LJAYi/komari-bridge/internal/model"
)

type Provider interface {
	ID() string
	SourceType() string
	Collect(context.Context) ([]model.Snapshot, error)
}

// MetricAuthority is implemented by enrichment providers that own the host
// metrics for resources discovered by another provider. The runner uses these
// declarations before collection starts, so a failed first collection cannot
// fall back to incompatible hypervisor metrics.
type MetricAuthority interface {
	MetricTargets() []model.Identity
}
