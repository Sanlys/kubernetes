package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/mounts"
)

// collector reads state that lives elsewhere (mount processes, rclone's rc
// stats, smbd's session table) at scrape time.
type collector struct {
	g *gateway
}

func desc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(name, help, labels, nil)
}

var (
	dMountUp       = desc("smbgw_mount_up", "1 when the mount is attached and its process running.", "mount", "type", "transport")
	dMountRestarts = desc("smbgw_mount_restarts_total", "Times the mount's process exited and was restarted.", "mount")
	dCacheBytes    = desc("smbgw_rclone_cache_bytes", "Bytes used by the mount's VFS cache.", "mount")
	dCacheFiles    = desc("smbgw_rclone_cache_files", "Files in the mount's VFS cache.", "mount")
	dCacheErrored  = desc("smbgw_rclone_cache_errored_files", "Cached files rclone failed to upload (they exist only in the cache).", "mount")
	dCacheOOS      = desc("smbgw_rclone_cache_out_of_space", "1 when the VFS cache ran out of space.", "mount")
	dUpQueued      = desc("smbgw_rclone_uploads_queued", "Files waiting to be uploaded.", "mount")
	dUpInProgress  = desc("smbgw_rclone_uploads_in_progress", "Files being uploaded.", "mount")
	dMetaDirs      = desc("smbgw_rclone_cached_dirs", "Directories in rclone's in-memory listing cache.", "mount")
	dMetaFiles     = desc("smbgw_rclone_cached_entries", "Files in rclone's in-memory listing cache.", "mount")
	dBytes         = desc("smbgw_rclone_transferred_bytes_total", "Bytes rclone transferred to/from the backend (since the process started).", "mount")
	dTransfers     = desc("smbgw_rclone_transfers_total", "Completed file transfers.", "mount")
	dErrors        = desc("smbgw_rclone_errors_total", "rclone errors.", "mount")
	dDeletes       = desc("smbgw_rclone_deletes_total", "Objects deleted on the backend.", "mount")
	dRenames       = desc("smbgw_rclone_renames_total", "Objects renamed on the backend.", "mount")
	dSmbdUp        = desc("smbgw_smbd_up", "1 while smbd is running.")
	dSessions      = desc("smbgw_smb_sessions", "Open SMB sessions by user and protocol dialect.", "user", "dialect")
	dTcons         = desc("smbgw_smb_share_connections", "Open share connections by share.", "share")
	dOpenFiles     = desc("smbgw_smb_open_files", "Files currently open over SMB.")
)

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{dMountUp, dMountRestarts, dCacheBytes, dCacheFiles, dCacheErrored, dCacheOOS,
		dUpQueued, dUpInProgress, dMetaDirs, dMetaFiles, dBytes, dTransfers, dErrors, dDeletes, dRenames,
		dSmbdUp, dSessions, dTcons, dOpenFiles} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gauge := func(d *prometheus.Desc, v float64, l ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, l...)
	}
	counter := func(d *prometheus.Desc, v float64, l ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, l...)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, in := range c.g.instances {
		wg.Add(1)
		go func(in *mounts.Instance) {
			defer wg.Done()
			m := in.M.Path
			up := 0.0
			if in.Up() {
				up = 1
			}
			mu.Lock()
			gauge(dMountUp, up, m, in.M.Type, in.Transport)
			counter(dMountRestarts, float64(in.Restarts()), m)
			mu.Unlock()
			if in.RC == nil || up == 0 {
				return
			}
			if v, err := in.RC.VFSStats(ctx); err == nil {
				d := v.DiskCache
				mu.Lock()
				gauge(dCacheBytes, d.BytesUsed, m)
				gauge(dCacheFiles, d.Files, m)
				gauge(dCacheErrored, d.ErroredFiles, m)
				gauge(dCacheOOS, b2f(d.OutOfSpace), m)
				gauge(dUpQueued, d.UploadsQueued, m)
				gauge(dUpInProgress, d.UploadsInProgress, m)
				gauge(dMetaDirs, v.MetadataCache.Dirs, m)
				gauge(dMetaFiles, v.MetadataCache.Files, m)
				mu.Unlock()
			}
			if s, err := in.RC.CoreStats(ctx); err == nil {
				mu.Lock()
				counter(dBytes, s.Bytes, m)
				counter(dTransfers, s.Transfers, m)
				counter(dErrors, s.Errors, m)
				counter(dDeletes, s.Deletes, m)
				counter(dRenames, s.Renames, m)
				mu.Unlock()
			}
		}(in)
	}

	smbdUp := c.g.smbdRunning()
	if smbdUp {
		c.collectSmbstatus(ctx, gauge)
	}
	wg.Wait()
	gauge(dSmbdUp, b2f(smbdUp))
}

func (c *collector) collectSmbstatus(ctx context.Context, gauge func(*prometheus.Desc, float64, ...string)) {
	out, err := exec.CommandContext(ctx, "smbstatus", "--json").Output()
	if err != nil {
		return
	}
	var st struct {
		Sessions map[string]struct {
			Username string `json:"username"`
			Dialect  string `json:"session_dialect"`
		} `json:"sessions"`
		Tcons map[string]struct {
			Service string `json:"service"`
		} `json:"tcons"`
		OpenFiles map[string]json.RawMessage `json:"open_files"`
	}
	if json.Unmarshal(out, &st) != nil {
		return
	}
	type key struct{ user, dialect string }
	sessions := map[key]float64{}
	for _, s := range st.Sessions {
		sessions[key{s.Username, s.Dialect}]++
	}
	for k, n := range sessions {
		gauge(dSessions, n, k.user, k.dialect)
	}
	tcons := map[string]float64{}
	for _, t := range st.Tcons {
		if t.Service != "IPC$" {
			tcons[t.Service]++
		}
	}
	for s, n := range tcons {
		gauge(dTcons, n, s)
	}
	gauge(dOpenFiles, float64(len(st.OpenFiles)))
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
