package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/ai-observable/ai-observable/internal/admin"
	"github.com/ai-observable/ai-observable/internal/collector"
	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/integration"
	"github.com/ai-observable/ai-observable/internal/proxy"
	// Registers every provider adapter (init side effect). Without it the
	// registry is empty and every validate/test/collector lane fails with
	// "unknown kind".
	_ "github.com/ai-observable/ai-observable/internal/provider/all"
	"github.com/ai-observable/ai-observable/internal/snapshot"
	"github.com/ai-observable/ai-observable/internal/store"
	"github.com/ai-observable/ai-observable/internal/vault"
	"github.com/spf13/cobra"
)

var version = "dev"
var dataDir string

func main() {
	root := &cobra.Command{Use: "llmtap", Short: "AI-Observable local LLM monitor"}
	root.PersistentFlags().StringVar(&dataDir, "data-dir", defaultDir(), "data directory")
	root.AddCommand(start(), status(), setup(), versionCmd())
	if e := root.Execute(); e != nil {
		os.Exit(1)
	}
}

func defaultDir() string {
	h, e := os.UserHomeDir()
	if e != nil {
		return ".llmtap"
	}
	return filepath.Join(h, ".llmtap")
}

// openVault opens the master key. mode=env without a key leaves the vault
// locked instead of refusing to start: the UI explains what is missing and
// managed providers answer 503 rather than the daemon dying.
func openVault(mode string) *vault.Vault {
	v, err := vault.Open(filepath.Join(dataDir, "keys"), mode)
	if err == nil {
		return v
	}
	fmt.Fprintln(os.Stderr, "vault:", err)
	if errors.Is(err, vault.ErrLocked) && v != nil {
		return v
	}
	return &vault.Vault{}
}

func resolveDataDir() (string, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	dataDir = abs
	return dataDir, nil
}

func vaultState(v *vault.Vault) string {
	if v == nil || !v.Available() {
		return "locked"
	}
	return "unlocked (" + v.KCV() + ")"
}

func start() *cobra.Command {
	var pp, ap string
	var forwardProxy, forwardAdmin string
	var remote bool
	cmd := &cobra.Command{Use: "start", RunE: func(c *cobra.Command, a []string) error {
		if _, e := resolveDataDir(); e != nil {
			return e
		}
		cfg, e := config.Load(dataDir)
		if e != nil {
			return e
		}
		if pp != "" {
			cfg.Server.ProxyAddr = config.WithPort(cfg.Server.ProxyAddr, pp)
		}
		if ap != "" {
			cfg.Server.AdminAddr = config.WithPort(cfg.Server.AdminAddr, ap)
		}
		if !remote && (!config.IsLoopback(cfg.Server.ProxyAddr) || !config.IsLoopback(cfg.Server.AdminAddr)) {
			return fmt.Errorf("refusing non-loopback bind; pass --allow-remote to override")
		}
		s, e := store.Open(dataDir)
		if e != nil {
			return e
		}
		defer s.Close()
		// The control DB is the source of truth for providers and keys; the
		// vault decrypts them. Both are optional for a minimal start.
		db, e := control.Open(filepath.Join(dataDir, "control.db"))
		if e != nil {
			return e
		}
		defer db.Close()
		v := openVault(cfg.Vault.Mode)
		// config.toml providers are imported additively so an existing setup
		// keeps working without the wizard; rows already in control.db win.
		if _, e := db.ImportFromConfig(context.Background(), cfg.Providers); e != nil {
			fmt.Fprintln(os.Stderr, "import config.toml:", e)
		}
		// Bind both ports before launching background work so a busy port is
		// reported without collectors, snapshots, or retention touching the DB.
		proxyLn, e := net.Listen("tcp", cfg.Server.ProxyAddr)
		if e != nil {
			return fmt.Errorf("proxy listen %s: %w", cfg.Server.ProxyAddr, e)
		}
		adminLn, e := net.Listen("tcp", cfg.Server.AdminAddr)
		if e != nil {
			proxyLn.Close()
			return fmt.Errorf("admin listen %s: %w", cfg.Server.AdminAddr, e)
		}
		forwarders := make([]*tcpForwarder, 0, 2)
		defer func() {
			for _, forwarder := range forwarders {
				forwarder.Close()
			}
		}()
		addForwarder := func(name, listen, target string) error {
			if listen == "" {
				return nil
			}
			forwarder, err := newTCPForwarder(name, listen, target)
			if err != nil {
				return err
			}
			forwarders = append(forwarders, forwarder)
			return nil
		}
		if e := addForwarder("proxy-forward", forwardProxy, cfg.Server.ProxyAddr); e != nil {
			proxyLn.Close()
			adminLn.Close()
			return e
		}
		if e := addForwarder("admin-forward", forwardAdmin, cfg.Server.AdminAddr); e != nil {
			proxyLn.Close()
			adminLn.Close()
			return e
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		snap := &snapshot.Snapshotter{DB: s.DB, ViewDir: filepath.Join(dataDir, "view")}
		if e := snap.Take(); e != nil {
			fmt.Fprintln(os.Stderr, "snapshot:", e)
		}
		go snapshotLoop(ctx, snap, cfg.Snapshot.IntervalSeconds)

		// The collector is declared here, not inside the if, so the admin API
		// can queue a run and report an honest "not running".
		var col *collector.Collector
		if cfg.Collector.Enabled {
			col = collector.New(db, s, v, cfg)
			col.Snapshot = func() {
				if e := snap.Take(); e != nil {
					fmt.Fprintln(os.Stderr, "snapshot:", e)
				}
			}
			go col.Run(ctx)
		}

		reg := proxy.NewControlRegistry(db, v, cfg)
		integrations := integration.New(db, s, v, cfg, dataDir)
		proxyHandler := proxy.NewWithResolver(cfg, s, reg)
		proxyHandler.Gateway = integrations
		mgr := admin.New(admin.Deps{
			Config: cfg, Store: s, Snap: snap, Control: db, Vault: v, Collector: col,
			Integrations: integrations, GatewayModels: proxyHandler.ModelsSeen,
			Invalidate: reg.Invalidate, DataDir: dataDir, Version: version,
		})

		go retentionLoop(ctx, s, snap, func() int { return cfg.Capture.RetentionDays }())

		proxySrv := &http.Server{
			Handler:           proxyHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		adminSrv := &http.Server{Handler: mgr, ReadHeaderTimeout: 10 * time.Second}
		errCh := make(chan error, 2+len(forwarders))
		go func() { errCh <- serveNamed(proxySrv, proxyLn, "proxy") }()
		go func() { errCh <- serveNamed(adminSrv, adminLn, "admin") }()
		for _, forwarder := range forwarders {
			f := forwarder
			go func() { errCh <- f.Serve(ctx) }()
		}

		proxyURL := "http://" + cfg.Server.ProxyAddr
		adminURL := "http://" + cfg.Server.AdminAddr
		fmt.Println("Proxy:  ", proxyURL)
		fmt.Println("Manager:", adminURL)
		fmt.Println("Grafana:", cfg.Server.GrafanaURL)
		fmt.Println("Vault:  ", vaultState(v))
		if cfg.Server.OpenOnStart {
			openBrowser(adminURL)
		}

		var runErr error
		select {
		case <-ctx.Done():
		case runErr = <-errCh:
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, forwarder := range forwarders {
			forwarder.Close()
		}
		proxySrv.Shutdown(shutCtx)
		adminSrv.Shutdown(shutCtx)
		if e := snap.Take(); e != nil {
			fmt.Fprintln(os.Stderr, "snapshot:", e)
		}
		return runErr
	}}
	cmd.Flags().StringVar(&pp, "proxy-port", "", "proxy port")
	cmd.Flags().StringVar(&ap, "admin-port", "", "admin port")
	cmd.Flags().StringVar(&forwardProxy, "forward-proxy", "", "listen address for forwarded proxy traffic")
	cmd.Flags().StringVar(&forwardAdmin, "forward-admin", "", "listen address for forwarded admin traffic")
	cmd.Flags().BoolVar(&remote, "allow-remote", false, "allow non-loopback binds")
	return cmd
}

// serveNamed turns a listener failure into an error the caller can return. A
// clean shutdown is not an error.
func serveNamed(srv *http.Server, ln net.Listener, name string) error {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	return nil
}

// snapshotLoop publishes the read-only view database on the configured interval.
func snapshotLoop(ctx context.Context, snap *snapshot.Snapshotter, seconds int) {
	if seconds < 1 {
		seconds = 5
	}
	t := time.NewTicker(time.Duration(seconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if e := snap.Take(); e != nil {
				fmt.Fprintln(os.Stderr, "snapshot:", e)
			}
		}
	}
}

// retentionLoop prunes old calls at most once a day, then refreshes the view so
// Grafana stops showing rows that no longer exist.
func retentionLoop(ctx context.Context, s *store.Store, snap *snapshot.Snapshotter, days int) {
	if days <= 0 {
		return
	}
	prune := func() {
		n, err := s.Retention(ctx, days)
		if err != nil {
			fmt.Fprintln(os.Stderr, "retention:", err)
			return
		}
		if n > 0 {
			fmt.Println("Retention:", n, "rows older than", days, "days deleted")
			if e := snap.Take(); e != nil {
				fmt.Fprintln(os.Stderr, "snapshot:", e)
			}
		}
	}
	if last, _, err := s.RetentionStatus(ctx); err == nil && (last.IsZero() || time.Since(last) >= 24*time.Hour) {
		prune()
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

// openBrowser is best-effort: a headless box must still start the daemon.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "open browser:", err)
	}
}

func status() *cobra.Command {
	return &cobra.Command{Use: "status", RunE: func(c *cobra.Command, a []string) error {
		if _, e := resolveDataDir(); e != nil {
			return e
		}
		s, e := store.Open(dataDir)
		if e != nil {
			return e
		}
		defer s.Close()
		n, e := s.Count(c.Context())
		fmt.Printf("calls: %d\ndatabase: %s\n", n, filepath.Join(dataDir, "llmtap.db"))
		return e
	}}
}

func setup() *cobra.Command {
	return &cobra.Command{Use: "setup [codex|openai-sdk|curl|generic]", Run: func(c *cobra.Command, a []string) {
		fmt.Println("Set your API base URL to http://127.0.0.1:7878/openai/v1 and preserve Authorization. Example: OPENAI_BASE_URL=http://127.0.0.1:7878/openai/v1")
	}}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(c *cobra.Command, a []string) {
		fmt.Println("AI-Observable", version)
	}}
}
