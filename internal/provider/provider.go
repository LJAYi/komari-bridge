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
