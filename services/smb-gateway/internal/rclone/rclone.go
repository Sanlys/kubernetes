// Package rclone builds rclone remote configs and command lines for the
// gateway's mounts, and talks to rclone's remote control API.
package rclone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/config"
)

const Binary = "/usr/local/bin/rclone"

// RemoteName is the rclone remote a mount is configured under.
func RemoteName(m *config.Mount) string {
	return "m_" + strings.ReplaceAll(m.ID, "-", "_")
}

// RemotePath is what rclone serves/mounts for a mount.
func RemotePath(m *config.Mount) string {
	switch m.Type {
	case config.TypeS3:
		p := m.Bucket
		if m.Prefix != "" {
			p += "/" + m.Prefix
		}
		return RemoteName(m) + ":" + p
	case config.TypeSyncthing:
		return RemoteName(m) + ":" + strings.Trim(m.SFTP.Path, "/")
	}
	return ""
}

// WriteConfig writes an rclone.conf with one remote per rclone-backed mount.
func WriteConfig(c *config.Config, file string) error {
	var b bytes.Buffer
	for _, m := range c.Mounts {
		switch m.Type {
		case config.TypeS3:
			s := m.S3Resolved
			fmt.Fprintf(&b, "[%s]\ntype = s3\nprovider = Ceph\nenv_auth = false\n", RemoteName(m))
			fmt.Fprintf(&b, "access_key_id = %s\nsecret_access_key = %s\n", s.AccessKeyID, s.SecretAccessKey)
			fmt.Fprintf(&b, "endpoint = %s\nregion = %s\n", s.Endpoint, s.Region)
			// Buckets are pre-created by their ObjectBucketClaims; checking
			// costs a request and needs permissions the user may lack.
			b.WriteString("no_check_bucket = true\nforce_path_style = true\n")
			b.WriteString("chunk_size = 16Mi\nupload_concurrency = 8\n\n")
		case config.TypeSyncthing:
			s := m.SFTP
			fmt.Fprintf(&b, "[%s]\ntype = sftp\nhost = %s\nport = %d\nuser = %s\n", RemoteName(m), s.Host, s.Port, s.User)
			if s.Password != "" {
				pass, err := obscure(s.Password)
				if err != nil {
					return err
				}
				fmt.Fprintf(&b, "pass = %s\n", pass)
			}
			if s.PrivateKey != "" {
				fmt.Fprintf(&b, "key_pem = %s\n", strings.ReplaceAll(strings.TrimSpace(s.PrivateKey), "\n", `\n`))
			}
			// Syncthing's sftp sidecar is an sftp-only chroot: no shell to
			// run md5sum in, so don't probe for one on every connection.
			b.WriteString("shell_type = none\ndisable_hashcheck = true\nset_modtime = true\n")
			// Fewer round trips per read/write; OpenSSH accepts this size.
			b.WriteString("chunk_size = 255Ki\nconcurrency = 64\n")
			b.WriteString("known_hosts_file =\n\n")
		}
	}
	return os.WriteFile(file, b.Bytes(), 0o600)
}

func obscure(s string) (string, error) {
	cmd := exec.Command(Binary, "obscure", "-")
	cmd.Stdin = strings.NewReader(s)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rclone obscure: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Ports is where an rclone instance listens.
type Ports struct {
	NFS int
	RC  int
}

// Args builds the rclone command line for a mount. For NFS it is a
// `serve nfs` on loopback; for FUSE it is `mount` onto mountpoint.
func Args(c *config.Config, m *config.Mount, transport, confFile, cacheRoot, mountpoint string, p Ports) []string {
	cache := c.Cache
	args := []string{}
	if transport == config.TransportNFS {
		args = append(args, "serve", "nfs", RemotePath(m),
			"--addr", fmt.Sprintf("127.0.0.1:%d", p.NFS),
			// On-disk handle cache: rclone can be restarted under a live
			// kernel mount without the clients getting stale handles.
			"--nfs-cache-type", "disk",
			"--nfs-cache-dir", fmt.Sprintf("%s/nfs-handles/%s", cacheRoot, m.ID),
		)
	} else {
		args = append(args, "mount", RemotePath(m), mountpoint,
			"--allow-other",
			"--attr-timeout", "1s",
			"--async-read=true",
		)
	}
	args = append(args,
		"--config", confFile,
		"--cache-dir", fmt.Sprintf("%s/rclone/%s", cacheRoot, m.ID),
		"--vfs-cache-mode", "full",
		"--vfs-cache-max-size", m.Cache.MaxSize,
		"--vfs-cache-max-age", m.Cache.MaxAge.D().String(),
		"--vfs-cache-min-free-space", cache.MinFreeSpace,
		"--vfs-write-back", cache.WriteBack.D().String(),
		"--vfs-read-ahead", cache.ReadAhead,
		"--vfs-read-chunk-size", "16M",
		"--vfs-read-chunk-size-limit", "512M",
		// Size+modtime is enough to notice a changed object; hashing on S3
		// would need a HEAD per file.
		"--vfs-fast-fingerprint",
		"--buffer-size", cache.BufferSize,
		"--dir-cache-time", c.DirCacheTime(m).String(),
		"--transfers", "8",
		"--vfs-read-chunk-streams", fmt.Sprint(cache.ReadChunkStreams),
		"--uid", "1000", "--gid", "1000", "--umask", "002",
		"--rc", "--rc-addr", fmt.Sprintf("127.0.0.1:%d", p.RC), "--rc-no-auth",
		// INFO logs every upload, delete and cache eviction; JSON so the
		// gateway can re-emit it with its fields intact.
		"--log-level", "INFO",
		"--use-json-log",
		"--stats", "0",
	)
	if m.Type == config.TypeS3 && !m.PreserveModtime {
		// rclone keeps client mtimes in object metadata, which costs one
		// HEAD request per file whenever a folder is listed - painful in
		// Explorer/Finder on big folders. Use the object's LastModified.
		args = append(args, "--use-server-modtime")
	}
	if m.ReadOnly {
		args = append(args, "--read-only")
	}
	return args
}

// RC is a client for one rclone instance's remote control API.
type RC struct {
	Addr   string
	Client *http.Client
}

func NewRC(port int) *RC {
	return &RC{Addr: fmt.Sprintf("http://127.0.0.1:%d", port), Client: &http.Client{
		Timeout: 5 * time.Minute,
		// Client spans for each rc call, with traceparent propagated
		// to rclone.
		Transport: otelhttp.NewTransport(http.DefaultTransport,
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return "rclone " + r.URL.Path }),
			// Only calls made as part of a trace (refreshes); the metrics
			// scrape's stats calls would otherwise be a root trace each.
			otelhttp.WithFilter(func(r *http.Request) bool {
				return trace.SpanContextFromContext(r.Context()).IsValid()
			})),
	}}
}

func (r *RC) Call(ctx context.Context, method string, params map[string]any, out any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Addr+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rc %s: %s: %s", method, resp.Status, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Ping returns nil once the rc server answers.
func (r *RC) Ping(ctx context.Context) error {
	return r.Call(ctx, "rc/noop", map[string]any{}, nil)
}

// Refresh re-lists dir from the remote right now (non-recursively).
func (r *RC) Refresh(ctx context.Context, dir string) error {
	params := map[string]any{}
	if dir != "" {
		params["dir"] = dir
	}
	var res struct {
		Result map[string]string `json:"result"`
	}
	if err := r.Call(ctx, "vfs/refresh", params, &res); err != nil {
		return err
	}
	for k, v := range res.Result {
		if v != "OK" {
			return fmt.Errorf("refresh %q: %s", k, v)
		}
	}
	return nil
}

// VFSStats is the subset of vfs/stats the gateway exports as metrics.
type VFSStats struct {
	DiskCache struct {
		BytesUsed         float64 `json:"bytesUsed"`
		ErroredFiles      float64 `json:"erroredFiles"`
		Files             float64 `json:"files"`
		OutOfSpace        bool    `json:"outOfSpace"`
		UploadsInProgress float64 `json:"uploadsInProgress"`
		UploadsQueued     float64 `json:"uploadsQueued"`
	} `json:"diskCache"`
	MetadataCache struct {
		Dirs  float64 `json:"dirs"`
		Files float64 `json:"files"`
	} `json:"metadataCache"`
}

func (r *RC) VFSStats(ctx context.Context) (*VFSStats, error) {
	var s VFSStats
	return &s, r.Call(ctx, "vfs/stats", map[string]any{}, &s)
}

// CoreStats is rclone's cumulative transfer accounting.
type CoreStats struct {
	Bytes     float64 `json:"bytes"`
	Errors    float64 `json:"errors"`
	Transfers float64 `json:"transfers"`
	Deletes   float64 `json:"deletes"`
	Renames   float64 `json:"renames"`
	Checks    float64 `json:"checks"`
}

func (r *RC) CoreStats(ctx context.Context) (*CoreStats, error) {
	var s CoreStats
	return &s, r.Call(ctx, "core/stats", map[string]any{}, &s)
}
