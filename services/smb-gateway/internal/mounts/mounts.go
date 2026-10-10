// Package mounts runs and supervises the processes behind each mount (rclone
// or restic) and attaches them to the gateway's directory tree.
package mounts

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/config"
	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/rclone"
)

const ResticBinary = "/usr/local/bin/restic"

type Instance struct {
	M          *config.Mount
	Transport  string
	MountPoint string
	Ports      rclone.Ports
	RC         *rclone.RC // nil for restic

	cfg       *config.Config
	confFile  string
	cacheRoot string
	log       *slog.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	restarts int
	stopping bool
	lastErr  string
}

func New(c *config.Config, m *config.Mount, treeRoot, confFile, cacheRoot string, idx int, log *slog.Logger) *Instance {
	in := &Instance{
		M:          m,
		Transport:  c.ResolveTransport(m),
		MountPoint: treeRoot + m.Path,
		cfg:        c,
		confFile:   confFile,
		cacheRoot:  cacheRoot,
		log:        log.With("mount", m.Path),
	}
	if m.Type != config.TypeRestic {
		// Fixed ports per position in the config; loopback only.
		in.Ports = rclone.Ports{NFS: 20490 + idx, RC: 5572 + idx}
		in.RC = rclone.NewRC(in.Ports.RC)
	}
	return in
}

func (in *Instance) command() *exec.Cmd {
	if in.M.Type == config.TypeRestic {
		args := []string{"mount", in.MountPoint,
			"--allow-other",
			// smbd accesses everything as one user; access control is per
			// share in Samba, not per file.
			"--no-default-permissions",
			// restic mount otherwise holds a repository lock for as long
			// as it is mounted, which makes `restic prune` fail. Snapshots
			// are immutable, and the mount reloads its index whenever the
			// snapshot list changes (e.g. after a forget/prune).
			"--no-lock",
			"--cache-dir", fmt.Sprintf("%s/restic/%s", in.cacheRoot, in.M.ID),
			// The default template has colons, which Windows can't show.
			"--time-template", "2006-01-02T15.04.05",
		}
		cmd := exec.Command(ResticBinary, args...)
		env := append(os.Environ(),
			"RESTIC_REPOSITORY="+in.M.Repository,
			"RESTIC_PASSWORD="+in.M.Password,
		)
		s := in.M.S3Resolved
		if s.AccessKeyID != "" {
			env = append(env, "AWS_ACCESS_KEY_ID="+s.AccessKeyID, "AWS_SECRET_ACCESS_KEY="+s.SecretAccessKey, "AWS_DEFAULT_REGION="+s.Region)
		}
		cmd.Env = env
		return cmd
	}
	return exec.Command(rclone.Binary, rclone.Args(in.cfg, in.M, in.Transport, in.confFile, in.cacheRoot, in.MountPoint, in.Ports)...)
}

func (in *Instance) startProcess() error {
	cmd := in.command()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return err
	}
	go pipeLog(in.log, stdout)
	go pipeLog(in.log, stderr)
	exited := make(chan struct{})
	in.mu.Lock()
	in.cmd, in.exited = cmd, exited
	in.mu.Unlock()
	go func() {
		err := cmd.Wait()
		in.mu.Lock()
		if err != nil {
			in.lastErr = err.Error()
		}
		in.mu.Unlock()
		close(exited)
	}()
	return nil
}

func pipeLog(log *slog.Logger, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		log.Info(sc.Text())
	}
}

// Start launches the process and attaches the mount, then supervises it in
// the background until ctx ends.
func (in *Instance) Start(ctx context.Context) error {
	for _, d := range []string{in.MountPoint,
		fmt.Sprintf("%s/rclone/%s", in.cacheRoot, in.M.ID),
		fmt.Sprintf("%s/nfs-handles/%s", in.cacheRoot, in.M.ID),
		fmt.Sprintf("%s/restic/%s", in.cacheRoot, in.M.ID)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := in.launch(ctx); err != nil {
		return err
	}
	go in.supervise(ctx)
	return nil
}

func (in *Instance) launch(ctx context.Context) error {
	if in.Transport == config.TransportFUSE {
		// A previous process' mount would still be listed (dead) after a
		// crash; FUSE needs a clean mountpoint.
		_ = unix.Unmount(in.MountPoint, unix.MNT_DETACH)
	}
	if err := in.startProcess(); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := in.ready(ctx); err == nil {
			break
		} else if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become ready: %v", in.M.Path, err)
		}
		select {
		case <-in.exited:
			return fmt.Errorf("%s: process exited during startup: %s", in.M.Path, in.lastErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if in.Transport == config.TransportNFS && !IsMounted(in.MountPoint) {
		if err := in.mountNFS(); err != nil {
			return err
		}
	}
	in.log.Info("mounted", "transport", in.Transport, "type", in.M.Type, "read_only", in.M.ReadOnly)
	return nil
}

func (in *Instance) ready(ctx context.Context) error {
	switch in.Transport {
	case config.TransportNFS:
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return in.RC.Ping(cctx)
	default:
		if !IsMounted(in.MountPoint) {
			return errors.New("not mounted yet")
		}
		return nil
	}
}

// mountNFS mounts rclone's loopback NFS server with mount(2) directly, so no
// mount.nfs helper, rpcbind or /etc/protocols is needed in the image.
func (in *Instance) mountNFS() error {
	p := in.Ports.NFS
	opts := strings.Join([]string{
		"vers=3", "proto=tcp", "mountproto=tcp", "mountvers=3",
		"addr=127.0.0.1", "mountaddr=127.0.0.1",
		fmt.Sprintf("port=%d", p), fmt.Sprintf("mountport=%d", p),
		// No NLM: Samba keeps its own lock database, and rclone has no
		// lock manager anyway.
		"nolock",
		// hard: if rclone restarts, I/O blocks until it is back (the
		// supervisor restarts it within seconds, and the on-disk handle
		// cache keeps handles valid) instead of failing writes, which on
		// a soft mount can silently lose data.
		"hard", "timeo=600", "retrans=2",
		"rsize=1048576", "wsize=1048576",
		// Attribute caching only needs to cover a burst of stats from one
		// Explorer refresh - loopback GETATTRs against rclone's in-memory
		// tree are cheap. Directory listings are invalidated explicitly
		// (see invalidate.NFSKicker).
		"actimeo=1",
		// Never cache "file does not exist": a file another client just
		// created must be visible on the very next lookup.
		"lookupcache=positive",
	}, ",")
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if in.M.ReadOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount("127.0.0.1:/", in.MountPoint, "nfs", flags, opts); err != nil {
		return fmt.Errorf("mount nfs at %s: %w", in.MountPoint, err)
	}
	return nil
}

func (in *Instance) supervise(ctx context.Context) {
	backoff := time.Second
	for {
		in.mu.Lock()
		exited := in.exited
		in.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-exited:
		}
		in.mu.Lock()
		stopping := in.stopping
		in.restarts++
		in.mu.Unlock()
		if stopping {
			return
		}
		in.log.Error("process exited, restarting", "err", in.lastErr, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err := in.launch(ctx); err != nil {
			in.log.Error("restart failed", "err", err)
			if backoff < time.Minute {
				backoff *= 2
			}
			// launch failed after starting the process: make sure the loop
			// sees an exit to retry on.
			in.mu.Lock()
			if in.cmd != nil && in.cmd.Process != nil {
				_ = syscall.Kill(-in.cmd.Process.Pid, syscall.SIGTERM)
			}
			in.mu.Unlock()
			continue
		}
		backoff = time.Second
	}
}

// Stop unmounts and terminates the process. For NFS a normal unmount
// flushes the kernel's dirty pages to rclone first; rclone then gets a
// grace period to finish (anything it can't upload in time stays in the
// persistent cache and is uploaded on next start).
func (in *Instance) Stop(grace time.Duration) {
	in.mu.Lock()
	in.stopping = true
	cmd, exited := in.cmd, in.exited
	in.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- unix.Unmount(in.MountPoint, 0) }()
	select {
	case err := <-done:
		if err != nil {
			_ = unix.Unmount(in.MountPoint, unix.MNT_DETACH)
		}
	case <-time.After(grace / 2):
		_ = unix.Unmount(in.MountPoint, unix.MNT_DETACH)
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(grace / 2):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Health checks that the mount answers within timeout.
func (in *Instance) Health(timeout time.Duration) error {
	in.mu.Lock()
	exited := in.exited
	in.mu.Unlock()
	select {
	case <-exited:
		return fmt.Errorf("process not running")
	default:
	}
	done := make(chan error, 1)
	go func() {
		_, err := os.ReadDir(in.MountPoint)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("listing %s timed out after %s", in.MountPoint, timeout)
	}
}

func (in *Instance) Restarts() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.restarts
}

// IsMounted reports whether p is a mount point.
func IsMounted(p string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && unescapeMountinfo(f[4]) == p {
			return true
		}
	}
	return false
}

func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var c int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &c); err == nil {
				b.WriteByte(byte(c))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
