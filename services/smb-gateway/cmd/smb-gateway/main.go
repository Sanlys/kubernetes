// smb-gateway serves S3 buckets (via rclone), restic repositories (via restic
// mount) and Syncthing's data (via its SFTP sidecar) over SMB, with cache
// invalidation driven by Ceph RGW bucket notifications and Syncthing events.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/config"
	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/events"
	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/invalidate"
	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/mounts"
	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/rclone"
	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/samba"
)

const (
	treeRoot   = "/srv/smb"
	smbConf    = "/etc/samba/smb.conf"
	rcloneConf = "/run/smb-gateway/rclone.conf"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: smb-gateway run|validate|render|health [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	cfgFile := fs.String("config", "/etc/smb-gateway/config.yaml", "config file")
	cacheRoot := fs.String("cache", "/cache", "persistent cache directory")
	listen := fs.String("listen", ":8090", "address for the event receiver and health endpoint")
	debug := fs.Bool("debug", false, "debug logging")
	_ = fs.Parse(os.Args[2:])

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	switch os.Args[1] {
	case "validate":
		if _, _, err := config.Load(*cfgFile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("config OK")
	case "render":
		c, _, err := config.Load(*cfgFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(samba.Render(c, treeRoot, readOnlyFunc(c)))
		for _, m := range c.Mounts {
			fmt.Printf("# %s: type=%s transport=%s dir-cache-time=%s notified=%v\n",
				m.Path, m.Type, c.ResolveTransport(m), c.DirCacheTime(m), c.MountNotified(m))
		}
	case "health":
		os.Exit(health(*listen))
	case "run":
		if err := run(*cfgFile, *cacheRoot, *listen, log); err != nil {
			log.Error("fatal", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func health(listen string) int {
	_, port, _ := net.SplitHostPort(listen)
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, b.String())
		return 1
	}
	return 0
}

// readOnlyFunc reports whether a share path lies entirely on a read-only
// mount (so Samba should not advertise it as writable).
func readOnlyFunc(c *config.Config) func(string) bool {
	return func(p string) bool {
		for _, m := range c.Mounts {
			if p == m.Path || strings.HasPrefix(p, m.Path+"/") {
				return m.ReadOnly
			}
		}
		// The share is the tree root or an intermediate folder: writable if
		// any mount below it is.
		for _, m := range c.Mounts {
			if (p == "/" || strings.HasPrefix(m.Path, p+"/")) && !m.ReadOnly {
				return false
			}
		}
		return true
	}
}

type gateway struct {
	cfg       *config.Config
	raw       []byte
	log       *slog.Logger
	instances []*mounts.Instance
	invs      map[string]*invalidate.Invalidator

	rgw *events.RGWHandler

	mu       sync.Mutex
	smbd     *exec.Cmd
	smbdErr  chan error
	smbdDone chan struct{}
	rgwOK    map[string]string // bucket -> "" (ok) or last error
}

func run(cfgFile, cacheRoot, listen string, log *slog.Logger) error {
	c, raw, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	g := &gateway{cfg: c, raw: raw, log: log, invs: map[string]*invalidate.Invalidator{}, rgwOK: map[string]string{}}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	for _, d := range []string{path.Dir(rcloneConf), treeRoot, cacheRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := rclone.WriteConfig(c, rcloneConf); err != nil {
		return err
	}
	if err := g.buildTree(); err != nil {
		return err
	}

	for i, m := range c.Mounts {
		in := mounts.New(c, m, treeRoot, rcloneConf, cacheRoot, i, log)
		if err := in.Start(ctx); err != nil {
			g.stopMounts()
			return err
		}
		g.instances = append(g.instances, in)
	}
	g.ensureSharePaths()

	g.startInvalidation(ctx)
	srv := &http.Server{Addr: listen, Handler: g.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			cancel()
		}
	}()

	if err := samba.EnsureDataUser(); err != nil {
		return err
	}
	if err := g.applySamba(); err != nil {
		g.stopMounts()
		return err
	}
	if err := g.startSmbd(); err != nil {
		g.stopMounts()
		return err
	}
	log.Info("smb-gateway ready", "mounts", len(g.instances), "shares", len(c.Shares))

	restart := make(chan struct{}, 1)
	go g.watchConfig(ctx, cfgFile, restart)

	var exitErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case <-restart:
		log.Info("mount configuration changed - exiting so the container restarts with it")
	case err := <-g.smbdErr:
		exitErr = fmt.Errorf("smbd exited: %v", err)
	}
	g.stopSmbd()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(sctx)
	scancel()
	g.stopMounts()
	return exitErr
}

// buildTree creates the directory skeleton (mount points and the folders
// containing them) on a tmpfs.
func (g *gateway) buildTree() error {
	_ = unix.Unmount(treeRoot, unix.MNT_DETACH)
	if err := unix.Mount("tmpfs", treeRoot, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size=1m"); err != nil {
		return fmt.Errorf("mounting tree tmpfs: %w", err)
	}
	for _, m := range g.cfg.Mounts {
		if err := os.MkdirAll(treeRoot+m.Path, 0o755); err != nil {
			return err
		}
	}
	// Share paths in the skeleton itself (e.g. /backups) must exist too.
	for _, s := range g.cfg.Shares {
		onMount := false
		for _, m := range g.cfg.Mounts {
			if s.Path == m.Path || strings.HasPrefix(s.Path, m.Path+"/") {
				onMount = true
			}
		}
		if !onMount {
			if err := os.MkdirAll(treeRoot+s.Path, 0o755); err != nil {
				return err
			}
		}
	}
	// Root-owned and 0755: smbd does all file I/O as samba.DataUser, so
	// nothing can be created outside the mounts. (Not a read-only remount:
	// restic refuses to mount onto a directory on a read-only filesystem.)
	return nil
}

// ensureSharePaths creates share directories that live inside writable
// mounts (S3 has no real folders; the folder appears once it has content).
func (g *gateway) ensureSharePaths() {
	for _, s := range g.cfg.Shares {
		p := treeRoot + s.Path
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.MkdirAll(p, 0o775); err != nil {
			g.log.Error("share path does not exist and could not be created", "share", s.Name, "path", s.Path, "err", err)
		}
	}
}

func (g *gateway) startInvalidation(ctx context.Context) {
	var targets []events.Target
	for _, in := range g.instances {
		m := in.M
		if in.RC == nil || !g.cfg.MountNotified(m) {
			continue
		}
		var kicker invalidate.Kicker
		if in.Transport == config.TransportNFS {
			kicker = &invalidate.NFSKicker{Addr: fmt.Sprintf("127.0.0.1:%d", in.Ports.NFS)}
		}
		var prefetch int64
		if m.Cache.PrefetchMaxSize != "" {
			prefetch = parseSize(m.Cache.PrefetchMaxSize)
		}
		inv := &invalidate.Invalidator{
			Name: m.Path, RC: in.RC, Kicker: kicker, Debounce: g.cfg.Cache.Debounce.D(),
			MountPoint: in.MountPoint, PrefetchMax: prefetch, Log: g.log,
		}
		g.invs[m.Path] = inv
		go inv.Run(ctx)
		switch m.Type {
		case config.TypeS3:
			targets = append(targets, events.Target{Bucket: m.Bucket, Prefix: m.Prefix, Inv: inv})
		case config.TypeSyncthing:
			st := &events.Syncthing{API: m.Syncthing, Inv: inv, Log: g.log.With("mount", m.Path)}
			go st.Run(ctx)
		}
	}
	g.rgw = &events.RGWHandler{Token: g.cfg.Notifications.Token, Targets: targets, Log: g.log}

	if !*g.cfg.Notifications.Manage {
		return
	}
	seen := map[string]bool{}
	for _, in := range g.instances {
		m := in.M
		if m.Type != config.TypeS3 || !g.cfg.MountNotified(m) || seen[m.Bucket] {
			continue
		}
		seen[m.Bucket] = true
		b := events.BucketSetup{Bucket: m.Bucket, S3: m.S3Resolved}
		g.setRGW(m.Bucket, "pending")
		go events.EnsureRGWLoop(ctx, g.cfg, b, g.log, func(ok bool, err error) {
			if ok {
				g.setRGW(b.Bucket, "")
			} else {
				g.setRGW(b.Bucket, err.Error())
			}
		})
	}
}

func (g *gateway) setRGW(bucket, state string) {
	g.mu.Lock()
	g.rgwOK[bucket] = state
	g.mu.Unlock()
}

func parseSize(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if strings.HasSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I"), u.suf) {
			s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I"), u.suf)
			mult = u.m
			break
		}
	}
	var n float64
	if _, err := fmt.Sscanf(s, "%g", &n); err != nil {
		return 0
	}
	return int64(n * float64(mult))
}

func (g *gateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/rgw/", g.rgw)
	mux.HandleFunc("/healthz", g.healthz)
	return mux
}

type mountStatus struct {
	Path      string            `json:"path"`
	Type      string            `json:"type"`
	Transport string            `json:"transport"`
	Healthy   bool              `json:"healthy"`
	Error     string            `json:"error,omitempty"`
	Restarts  int               `json:"restarts"`
	Feed      *invalidate.Stats `json:"feed,omitempty"`
}

func (g *gateway) healthz(w http.ResponseWriter, r *http.Request) {
	// Only from inside the pod (the probe), it reveals the layout.
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ok := true
	var ms []mountStatus
	for _, in := range g.instances {
		st := mountStatus{Path: in.M.Path, Type: in.M.Type, Transport: in.Transport, Healthy: true, Restarts: in.Restarts()}
		if err := in.Health(10 * time.Second); err != nil {
			st.Healthy, st.Error, ok = false, err.Error(), false
		}
		if inv := g.invs[in.M.Path]; inv != nil {
			s := inv.Stats()
			st.Feed = &s
		}
		ms = append(ms, st)
	}
	g.mu.Lock()
	smbdUp := false
	if g.smbdDone != nil {
		select {
		case <-g.smbdDone:
		default:
			smbdUp = true
		}
	}
	rgw := map[string]string{}
	for k, v := range g.rgwOK {
		if v == "" {
			v = "ok"
		}
		rgw[k] = v
	}
	g.mu.Unlock()
	if !smbdUp {
		ok = false
	}
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "smbd": smbdUp, "mounts": ms, "rgw_notifications": rgw})
}

func (g *gateway) applySamba() error {
	conf := samba.Render(g.cfg, treeRoot, readOnlyFunc(g.cfg))
	if err := os.MkdirAll(path.Dir(smbConf), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(smbConf, []byte(conf), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("testparm", "-s", "--suppress-prompt", smbConf).CombinedOutput(); err != nil {
		return fmt.Errorf("testparm rejected generated smb.conf: %v: %s", err, out)
	}
	return samba.SyncUsers(g.cfg)
}

func (g *gateway) startSmbd() error {
	cmd := exec.Command("smbd", "--foreground", "--no-process-group", "--debug-stdout", "-s", smbConf)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	g.mu.Lock()
	g.smbd = cmd
	g.smbdErr = make(chan error, 1)
	g.smbdDone = make(chan struct{})
	errc, done := g.smbdErr, g.smbdDone
	g.mu.Unlock()
	go func() {
		err := cmd.Wait()
		close(done)
		errc <- err
	}()
	return nil
}

func (g *gateway) stopSmbd() {
	g.mu.Lock()
	cmd, done := g.smbd, g.smbdDone
	g.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
	}
	// smbd children serving client connections.
	_ = exec.Command("pkill", "-TERM", "smbd").Run()
}

func (g *gateway) stopMounts() {
	var wg sync.WaitGroup
	for _, in := range g.instances {
		wg.Add(1)
		go func(in *mounts.Instance) {
			defer wg.Done()
			in.Stop(60 * time.Second)
		}(in)
	}
	wg.Wait()
	_ = unix.Unmount(treeRoot, unix.MNT_DETACH)
}

// watchConfig picks up a changed Secret (kubelet updates the mounted file in
// place). Users, passwords and shares are applied live; anything affecting
// mounts makes the gateway exit so the container restarts with it.
func (g *gateway) watchConfig(ctx context.Context, file string, restart chan<- struct{}) {
	last := sha256.Sum256(g.raw)
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(raw)
		if sum == last {
			continue
		}
		last = sum
		nc, err := config.Parse(raw)
		if err != nil {
			g.log.Error("changed config is invalid, keeping the running one", "err", err)
			continue
		}
		if nc.MountsDigest() != g.cfg.MountsDigest() {
			restart <- struct{}{}
			return
		}
		old := g.cfg
		g.cfg = nc
		if err := g.applySamba(); err != nil {
			g.log.Error("applying new users/shares failed, reverting", "err", err)
			g.cfg = old
			_ = g.applySamba()
			continue
		}
		g.ensureSharePaths()
		if out, err := exec.Command("smbcontrol", "smbd", "reload-config").CombinedOutput(); err != nil {
			g.log.Error("smbcontrol reload-config", "err", err, "out", string(out))
		}
		g.log.Info("reloaded users and shares")
	}
}
