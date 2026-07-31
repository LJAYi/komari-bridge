package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/LJAYi/komari-bridge/internal/komari"
	"github.com/LJAYi/komari-bridge/internal/model"
	"github.com/LJAYi/komari-bridge/internal/provider"
	"github.com/LJAYi/komari-bridge/internal/store"
)

type Runner struct {
	store     *store.Store
	komari    *komari.Client
	providers []provider.Provider
	log       *slog.Logger

	basicMu       sync.Mutex
	basicUploaded map[string]bool
}

func NewRunner(s *store.Store, k *komari.Client, providers []provider.Provider, logger *slog.Logger) *Runner {
	return &Runner{
		store: s, komari: k, providers: providers, log: logger,
		basicUploaded: make(map[string]bool),
	}
}

func (r *Runner) Run(ctx context.Context, interval time.Duration) error {
	if err := r.Cycle(ctx); err != nil {
		r.log.Error("collection cycle completed with errors", "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.Cycle(ctx); err != nil {
				r.log.Error("collection cycle completed with errors", "error", err)
			}
		}
	}
}

func (r *Runner) Cycle(ctx context.Context) error {
	var cycleErrors []error
	merged := make(map[string]model.Snapshot)
	for _, p := range r.providers {
		snapshots, err := p.Collect(ctx)
		if err != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("%s %s: %w", p.SourceType(), p.ID(), err))
			continue
		}
		r.log.Info("provider collection complete", "type", p.SourceType(), "id", p.ID(), "resources", len(snapshots))
		for _, snapshot := range snapshots {
			key := ResourceKey(snapshot.Identity)
			if current, ok := merged[key]; ok {
				merged[key] = mergeSnapshots(current, snapshot)
			} else {
				merged[key] = snapshot
			}
		}
	}
	for _, snapshot := range merged {
		if err := r.process(ctx, snapshot); err != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("%s: %w", snapshot.Identity.ExternalID, err))
		}
	}
	return errors.Join(cycleErrors...)
}

func mergeSnapshots(a, b model.Snapshot) model.Snapshot {
	base, enrichment := a, b
	if a.Priority > b.Priority {
		base, enrichment = b, a
	}
	// Whole guest-side metric/basic-info payloads replace hypervisor
	// observations. Discovery metadata remains canonical unless enrichment
	// explicitly supplies a value.
	// Guest-side collectors must not invent a virtualization platform. Preserve
	// authoritative discovery metadata when enrichment leaves it unknown.
	basicInfo := enrichment.BasicInfo
	if basicInfo.Virtualization == "" {
		basicInfo.Virtualization = base.BasicInfo.Virtualization
	}
	base.BasicInfo = basicInfo
	base.Report = enrichment.Report
	base.Online = enrichment.Online
	base.CollectedAt = enrichment.CollectedAt
	base.Priority = enrichment.Priority
	if enrichment.Name != "" {
		base.Name = enrichment.Name
	}
	if enrichment.Group != "" {
		base.Group = enrichment.Group
	}
	if enrichment.ResourceType != "" {
		base.ResourceType = enrichment.ResourceType
	}
	if enrichment.ParentExternalID != "" {
		base.ParentExternalID = enrichment.ParentExternalID
	}
	if base.Tags == nil {
		base.Tags = map[string]string{}
	}
	for key, value := range enrichment.Tags {
		base.Tags[key] = value
	}
	return base
}

func (r *Runner) process(ctx context.Context, snapshot model.Snapshot) error {
	if err := r.store.Observe(ctx, snapshot); err != nil {
		return err
	}
	binding, err := r.store.Binding(ctx, snapshot.Identity)
	if errors.Is(err, store.ErrNotBound) {
		name := snapshot.Name
		if snapshot.Group != "" {
			name = snapshot.Group + " / " + name
		}
		uuid, token, registerErr := r.komari.Register(ctx, name)
		if registerErr != nil {
			return registerErr
		}
		binding = model.Binding{Identity: snapshot.Identity, KomariUUID: uuid, Token: token}
		if err := r.store.Bind(ctx, binding); err != nil {
			return err
		}
		r.log.Info("registered virtual Komari client", "resource", snapshot.Identity.ExternalID, "uuid", uuid)
	} else if err != nil {
		return err
	}

	// Registration is useful for inventory, but an offline source must not be
	// kept online by basic-info or metric requests.
	if !snapshot.Online {
		return nil
	}
	if !r.wasBasicUploaded(binding.KomariUUID) {
		if err := r.komari.UploadBasicInfo(ctx, binding.Token, snapshot.BasicInfo); err != nil {
			return err
		}
		r.markBasicUploaded(binding.KomariUUID)
	}
	if err := r.komari.Report(ctx, binding.Token, snapshot.Report); err != nil {
		return err
	}
	return r.store.MarkReported(ctx, snapshot.Identity, time.Now())
}

func (r *Runner) wasBasicUploaded(uuid string) bool {
	r.basicMu.Lock()
	defer r.basicMu.Unlock()
	return r.basicUploaded[uuid]
}

func (r *Runner) markBasicUploaded(uuid string) {
	r.basicMu.Lock()
	defer r.basicMu.Unlock()
	r.basicUploaded[uuid] = true
}

func ResourceKey(identity model.Identity) string {
	return strings.Join([]string{identity.SourceType, identity.SourceID, identity.ExternalID}, "/")
}
