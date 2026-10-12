package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry holds every gateway metric (plus Go/process metrics).
var Registry = prometheus.NewRegistry()

var (
	// Change feed and cache invalidation.
	Events = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_events_total",
		Help: "Change events received, by source (rgw, syncthing), mount and kind (changed, removed).",
	}, []string{"source", "mount", "kind"})
	EventsUnrouted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_events_unrouted_total",
		Help: "RGW events for a bucket/prefix no mount exposes.",
	}, []string{"bucket"})
	EventsRejected = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "smbgw_events_rejected_total",
		Help: "Event pushes rejected for a bad token or method.",
	})
	EventsDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_events_dropped_total",
		Help: "Events dropped because the mount's queue was full (a full refresh is forced instead).",
	}, []string{"mount"})
	Batches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_invalidation_batches_total",
		Help: "Debounced batches of change events processed.",
	}, []string{"mount"})
	BatchDirs = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "smbgw_invalidation_batch_dirs",
		Help:    "Directories refreshed per batch.",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64},
	}, []string{"mount"})
	Refreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_refreshes_total",
		Help: "rclone directory refreshes, by result (ok, missing - fell back to the parent -, error).",
	}, []string{"mount", "result"})
	RefreshSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "smbgw_refresh_duration_seconds",
		Help:    "Time to re-list one directory from the backend.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
	}, []string{"mount"})
	Kicks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_kernel_kicks_total",
		Help: "NFS directory mtime bumps that make the kernel drop its cached listing, by result.",
	}, []string{"mount", "result"})
	Prefetched = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_prefetched_files_total",
		Help: "New files read into the local cache ahead of time.",
	}, []string{"mount"})
	LastEvent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "smbgw_last_event_timestamp_seconds",
		Help: "Unix time of the last change event per mount.",
	}, []string{"mount"})

	// Feeds.
	RGWConfigured = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "smbgw_rgw_notification_configured",
		Help: "1 when the bucket's RGW notification to this gateway is set up.",
	}, []string{"bucket"})
	SyncthingConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "smbgw_syncthing_feed_connected",
		Help: "1 while the Syncthing event feed is being polled successfully.",
	}, []string{"mount"})
	SyncthingErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_syncthing_feed_errors_total",
		Help: "Failed Syncthing event API polls.",
	}, []string{"mount"})

	// Mounts.
	MountLaunches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_mount_launches_total",
		Help: "Mount process starts, by result.",
	}, []string{"mount", "result"})

	// SMB.
	SMBAuth = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_smb_auth_total",
		Help: "SMB login attempts by account and result (ok, or the NT status).",
	}, []string{"user", "result"})
	SMBFileOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_smb_file_ops_total",
		Help: "SMB file operations (from Samba's audit log) by share, user, operation and result.",
	}, []string{"share", "user", "op", "result"})
	SMBDRestarts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "smbgw_smbd_exits_total",
		Help: "Times smbd exited.",
	})

	ConfigReloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "smbgw_config_reloads_total",
		Help: "Live config reloads (users/shares), by result.",
	}, []string{"result"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		Events, EventsUnrouted, EventsRejected, EventsDropped, Batches, BatchDirs, Refreshes, RefreshSeconds,
		Kicks, Prefetched, LastEvent, RGWConfigured, SyncthingConnected, SyncthingErrors, MountLaunches,
		SMBAuth, SMBFileOps, SMBDRestarts, ConfigReloads,
	)
}
