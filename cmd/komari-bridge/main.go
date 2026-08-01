package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/LJAYi/komari-bridge/internal/bridge"
	"github.com/LJAYi/komari-bridge/internal/buildinfo"
	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/httpapi"
	"github.com/LJAYi/komari-bridge/internal/komari"
	"github.com/LJAYi/komari-bridge/internal/provider"
	"github.com/LJAYi/komari-bridge/internal/slurm"
	"github.com/LJAYi/komari-bridge/internal/store"
	"github.com/LJAYi/komari-bridge/providers/linuxssh"
	"github.com/LJAYi/komari-bridge/providers/proxmox"
	"github.com/LJAYi/komari-bridge/providers/windowsssh"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	once := flag.Bool("once", false, "run one collection cycle and exit")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if *showVersion {
		buildinfo.Write(os.Stdout)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load configuration", "error", err)
		os.Exit(1)
	}
	db, err := store.Open(cfg.Database.Path)
	if err != nil {
		logger.Error("open resource store", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	komariClient, err := komari.New(cfg.Komari.Endpoint, cfg.Komari.AutoDiscoveryKey, cfg.Komari.Timeout.Duration)
	if err != nil {
		logger.Error("create Komari client", "error", err)
		os.Exit(1)
	}
	slurmStore := slurm.NewStore()
	providers, err := buildProviders(cfg, slurmStore)
	if err != nil {
		logger.Error("create providers", "error", err)
		os.Exit(1)
	}
	runner := bridge.NewRunner(db, komariClient, providers, logger)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *once {
		if err := runner.Cycle(ctx); err != nil {
			logger.Error("collection failed", "error", err)
			os.Exit(1)
		}
		return
	}
	httpServer := &http.Server{Addr: cfg.HTTP.Listen, Handler: httpapi.New(slurmStore, cfg.HTTP.APIKey).Handler()}
	go func() {
		logger.Info("bridge HTTP API started", "listen", cfg.HTTP.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("bridge HTTP API stopped", "error", err)
			cancel()
		}
	}()
	defer httpServer.Shutdown(context.Background())
	logger.Info("komari-bridge started", "version", buildinfo.Version, "commit", buildinfo.Commit,
		"interval", cfg.Scheduler.Interval.Duration, "providers", len(providers))
	if err := runner.Run(ctx, cfg.Scheduler.Interval.Duration); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("bridge stopped", "error", err)
		os.Exit(1)
	}
}

func buildProviders(cfg config.Config, slurmStore *slurm.Store) ([]provider.Provider, error) {
	providerCount := len(cfg.Providers.Proxmox) + len(cfg.Providers.AgentlessSSH) + len(cfg.Providers.Slurm) +
		len(cfg.Providers.WindowsWSL) + len(cfg.Providers.LinuxSSH) + len(cfg.Providers.WindowsSSH)
	providers := make([]provider.Provider, 0, providerCount)
	for _, pveCfg := range cfg.Providers.Proxmox {
		p, err := proxmox.New(pveCfg, cfg.Komari.Timeout.Duration)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	for _, sshCfg := range cfg.Providers.AgentlessSSH {
		p, err := linuxssh.NewAgentless(sshCfg, cfg.Komari.Timeout.Duration)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	for _, sshCfg := range cfg.Providers.Slurm {
		p, err := linuxssh.NewSlurm(sshCfg, cfg.Komari.Timeout.Duration, slurmStore)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	for _, sshCfg := range cfg.Providers.WindowsWSL {
		p, err := windowsssh.NewWSL(sshCfg, cfg.Komari.Timeout.Duration)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	// Deprecated provider names retain their original source identities so an
	// upgrade cannot register duplicate Komari clients in existing deployments.
	for _, sshCfg := range cfg.Providers.LinuxSSH {
		p, err := linuxssh.New(sshCfg, cfg.Komari.Timeout.Duration, slurmStore)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	for _, sshCfg := range cfg.Providers.WindowsSSH {
		p, err := windowsssh.New(sshCfg, cfg.Komari.Timeout.Duration)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, nil
}
