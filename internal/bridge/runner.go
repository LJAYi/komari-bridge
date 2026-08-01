package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/LJAYi/komari-bridge/internal/buildinfo"
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

	basicMu         sync.Mutex
	basicInfoHashes map[string][sha256.Size]byte

	authoritativeTargets map[string]struct{}
	authorityCache       map[string]cachedSnapshot
	authorityGracePeriod time.Duration
	now                  func() time.Time
}

type cachedSnapshot struct {
	snapshot model.Snapshot
	savedAt  time.Time
}

func NewRunner(s *store.Store, k *komari.Client, providers []provider.Provider, logger *slog.Logger) *Runner {
	runner := &Runner{
		store: s, komari: k, providers: providers, log: logger,
		basicInfoHashes:      make(map[string][sha256.Size]byte),
		authoritativeTargets: make(map[string]struct{}),
		authorityCache:       make(map[string]cachedSnapshot),
		authorityGracePeriod: time.Minute,
		now:                  time.Now,
	}
	for _, p := range providers {
		authority, ok := p.(provider.MetricAuthority)
		if !ok {
			continue
		}
		for _, target := range authority.MetricTargets() {
			if target.Valid() {
				runner.authoritativeTargets[ResourceKey(target)] = struct{}{}
			}
		}
	}
	return runner
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
		providerTargets := make(map[string]struct{})
		if authority, ok := p.(provider.MetricAuthority); ok {
			for _, target := range authority.MetricTargets() {
				if target.Valid() {
					providerTargets[ResourceKey(target)] = struct{}{}
				}
			}
		}
		snapshots, err := p.Collect(ctx)
		if err != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("%s %s: %w", p.SourceType(), p.ID(), err))
			continue
		}
		r.log.Info("provider collection complete", "type", p.SourceType(), "id", p.ID(), "resources", len(snapshots))
		for _, snapshot := range snapshots {
			key := ResourceKey(snapshot.Identity)
			if _, ok := providerTargets[key]; ok {
				if err := validateSnapshot(snapshot); err != nil {
					cycleErrors = append(cycleErrors, fmt.Errorf("%s %s %s: invalid authoritative snapshot: %w", p.SourceType(), p.ID(), snapshot.Identity.ExternalID, err))
					continue
				}
				r.authorityCache[key] = cachedSnapshot{snapshot: snapshot, savedAt: r.now()}
			}
			if current, ok := merged[key]; ok {
				merged[key] = mergeSnapshots(current, snapshot)
			} else {
				merged[key] = snapshot
			}
		}
	}
	r.applyMetricAuthorities(merged)
	for _, snapshot := range merged {
		// Komari's Version column describes the reporting client, not the
		// observed guest or hypervisor. Set it centrally so every provider and
		// merged snapshot exposes the exact bridge build that submitted it.
		snapshot.BasicInfo.Version = buildinfo.ClientVersion(snapshotCollector(snapshot))
		if err := validateSnapshot(snapshot); err != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("%s: invalid merged snapshot: %w", snapshot.Identity.ExternalID, err))
			continue
		}
		if err := r.process(ctx, snapshot); err != nil {
			cycleErrors = append(cycleErrors, fmt.Errorf("%s: %w", snapshot.Identity.ExternalID, err))
		}
	}
	return errors.Join(cycleErrors...)
}

func snapshotCollector(snapshot model.Snapshot) string {
	if source := snapshot.Tags["metrics_source"]; source != "" {
		return source
	}
	if source := snapshot.Tags["source"]; source != "" {
		return source
	}
	return snapshot.Identity.SourceType
}

func validateSnapshot(snapshot model.Snapshot) error {
	if !snapshot.Identity.Valid() {
		return fmt.Errorf("invalid resource identity")
	}
	if !snapshot.Online {
		return nil
	}
	if snapshot.BasicInfo.CPUCores < 0 || snapshot.BasicInfo.CPUPhysicalCores < 0 {
		return fmt.Errorf("negative CPU core count")
	}
	if math.IsNaN(snapshot.Report.CPU.Usage) || math.IsInf(snapshot.Report.CPU.Usage, 0) || snapshot.Report.CPU.Usage < 0 || snapshot.Report.CPU.Usage > 100 {
		return fmt.Errorf("CPU usage %.2f is outside 0..100", snapshot.Report.CPU.Usage)
	}
	if snapshot.BasicInfo.MemoryTotal <= 0 || snapshot.Report.RAM.Total <= 0 {
		return fmt.Errorf("online resource has no memory total")
	}
	if snapshot.BasicInfo.MemoryTotal != snapshot.Report.RAM.Total {
		return fmt.Errorf("basic memory total %d differs from report total %d", snapshot.BasicInfo.MemoryTotal, snapshot.Report.RAM.Total)
	}
	if err := validateUsage("memory", snapshot.Report.RAM); err != nil {
		return err
	}
	if snapshot.BasicInfo.SwapTotal != snapshot.Report.Swap.Total {
		return fmt.Errorf("basic swap total %d differs from report total %d", snapshot.BasicInfo.SwapTotal, snapshot.Report.Swap.Total)
	}
	if err := validateUsage("swap", snapshot.Report.Swap); err != nil {
		return err
	}
	if snapshot.BasicInfo.DiskTotal != snapshot.Report.Disk.Total {
		return fmt.Errorf("basic disk total %d differs from report total %d", snapshot.BasicInfo.DiskTotal, snapshot.Report.Disk.Total)
	}
	if err := validateUsage("disk", snapshot.Report.Disk); err != nil {
		return err
	}
	if snapshot.Report.Uptime < 0 || snapshot.Report.Process < 0 || snapshot.Report.Connections.TCP < 0 || snapshot.Report.Connections.UDP < 0 {
		return fmt.Errorf("negative host counter")
	}
	if snapshot.Report.Network.Up < 0 || snapshot.Report.Network.Down < 0 || snapshot.Report.Network.TotalUp < 0 || snapshot.Report.Network.TotalDown < 0 {
		return fmt.Errorf("negative network counter")
	}
	if snapshot.Report.GPU != nil {
		gpu := snapshot.Report.GPU
		if gpu.Count <= 0 || gpu.Count != len(gpu.DetailedInfo) {
			return fmt.Errorf("GPU count %d differs from %d devices", gpu.Count, len(gpu.DetailedInfo))
		}
		if math.IsNaN(gpu.AverageUsage) || math.IsInf(gpu.AverageUsage, 0) || gpu.AverageUsage < 0 || gpu.AverageUsage > 100 {
			return fmt.Errorf("GPU average usage %.2f is outside 0..100", gpu.AverageUsage)
		}
		for index, device := range gpu.DetailedInfo {
			if device.MemoryTotal < 0 || device.MemoryUsed < 0 || device.MemoryUsed > device.MemoryTotal {
				return fmt.Errorf("GPU %d has invalid memory usage %d/%d", index, device.MemoryUsed, device.MemoryTotal)
			}
			if math.IsNaN(device.Utilization) || math.IsInf(device.Utilization, 0) || device.Utilization < 0 || device.Utilization > 100 {
				return fmt.Errorf("GPU %d utilization %.2f is outside 0..100", index, device.Utilization)
			}
		}
	}
	return nil
}

func validateUsage(name string, usage model.Usage) error {
	if usage.Total < 0 || usage.Used < 0 || usage.Used > usage.Total {
		return fmt.Errorf("%s usage %d/%d is invalid", name, usage.Used, usage.Total)
	}
	return nil
}

func (r *Runner) applyMetricAuthorities(merged map[string]model.Snapshot) {
	now := r.now()
	for key := range r.authoritativeTargets {
		cached, ok := r.authorityCache[key]
		if !ok || now.Sub(cached.savedAt) > r.authorityGracePeriod {
			delete(merged, key)
			continue
		}
		if current, ok := merged[key]; ok {
			merged[key] = mergeSnapshots(current, cached.snapshot)
		} else {
			merged[key] = cached.snapshot
		}
	}
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
	basicHash, err := hashBasicInfo(snapshot.BasicInfo)
	if err != nil {
		return err
	}
	if !r.isCurrentBasicInfo(binding.KomariUUID, basicHash) {
		if err := r.komari.UploadBasicInfo(ctx, binding.Token, snapshot.BasicInfo); err != nil {
			return err
		}
		r.markBasicInfo(binding.KomariUUID, basicHash)
	}
	if err := r.komari.Report(ctx, binding.Token, snapshot.Report); err != nil {
		return err
	}
	return r.store.MarkReported(ctx, snapshot.Identity, r.now())
}

func hashBasicInfo(info model.BasicInfo) ([sha256.Size]byte, error) {
	payload, err := json.Marshal(info)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode basic info: %w", err)
	}
	return sha256.Sum256(payload), nil
}

func (r *Runner) isCurrentBasicInfo(uuid string, hash [sha256.Size]byte) bool {
	r.basicMu.Lock()
	defer r.basicMu.Unlock()
	current, ok := r.basicInfoHashes[uuid]
	return ok && current == hash
}

func (r *Runner) markBasicInfo(uuid string, hash [sha256.Size]byte) {
	r.basicMu.Lock()
	defer r.basicMu.Unlock()
	r.basicInfoHashes[uuid] = hash
}

func ResourceKey(identity model.Identity) string {
	return strings.Join([]string{identity.SourceType, identity.SourceID, identity.ExternalID}, "/")
}
