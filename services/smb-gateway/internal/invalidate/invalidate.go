// Package invalidate turns change events (from Ceph RGW bucket notifications
// or Syncthing's event API) into targeted cache refreshes for one mount.
//
// For every changed object it decides what to refresh and how:
//
//  1. Events are coalesced for a short debounce window and reduced to the
//     set of parent directories (plus grandparents for deletions, since a
//     folder on S3 disappears with its last object).
//  2. Large bursts (a sync dropping thousands of files) are collapsed
//     upwards until at most maxDirs directories remain, trading a few wider
//     listings for not issuing thousands of narrow ones.
//  3. Each directory is re-listed in rclone (vfs/refresh, which keeps the
//     existing nodes and therefore inode numbers - forgetting them would
//     make the kernel NFS client see "fileid changed" and return ESTALE).
//     If the directory no longer exists, its parent is refreshed instead.
//  4. On NFS mounts, the refreshed directory's mtime is bumped through a
//     side-channel NFS connection to rclone. rclone reports a constant
//     mtime for S3/SFTP directories, so without this the kernel NFS client
//     never notices its cached directory listing is stale.
//  5. Optionally, small new files in folders that are being browsed are
//     read through the mount once, so they are already in the local cache
//     when someone opens them.
package invalidate

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/rclone"
)

type Kind int

const (
	Changed Kind = iota
	Removed
)

// Change is one changed object, path relative to the mount's remote root.
type Change struct {
	Path string
	Kind Kind
	Size int64
}

// Kicker forces the kernel to revalidate its cached view of a directory.
type Kicker interface {
	Kick(dir string) error
}

type Invalidator struct {
	Name       string
	RC         *rclone.RC
	Kicker     Kicker // nil for FUSE mounts (kernel caches expire on their own)
	Debounce   time.Duration
	MountPoint string // for prefetching through the mount
	// PrefetchMax: max size of new files to read ahead (0 disables).
	PrefetchMax int64
	Log         *slog.Logger

	ch        chan Change
	once      sync.Once
	prefetchQ chan string
	mu        sync.Mutex
	stats     Stats
}

type Stats struct {
	Events     int64     `json:"events"`
	Refreshes  int64     `json:"refreshes"`
	Kicks      int64     `json:"kicks"`
	Errors     int64     `json:"errors"`
	Prefetched int64     `json:"prefetched"`
	LastEvent  time.Time `json:"last_event"`
}

const maxDirs = 64

func (iv *Invalidator) init() {
	iv.once.Do(func() {
		iv.ch = make(chan Change, 4096)
		iv.prefetchQ = make(chan string, 256)
		if iv.Debounce <= 0 {
			iv.Debounce = time.Second
		}
	})
}

// Notify queues a change. Never blocks the event source for long: if the
// queue is full the change is dropped and a refresh of the root is forced.
func (iv *Invalidator) Notify(c Change) {
	iv.init()
	c.Path = strings.Trim(path.Clean("/"+c.Path), "/")
	select {
	case iv.ch <- c:
	default:
		select {
		case iv.ch <- Change{Path: "", Kind: Removed}:
		default:
		}
	}
}

func (iv *Invalidator) Stats() Stats {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	return iv.stats
}

func (iv *Invalidator) count(f func(*Stats)) {
	iv.mu.Lock()
	f(&iv.stats)
	iv.mu.Unlock()
}

// Run processes changes until ctx is done.
func (iv *Invalidator) Run(ctx context.Context) {
	iv.init()
	go iv.prefetcher(ctx)
	for {
		var batch []Change
		select {
		case <-ctx.Done():
			return
		case c := <-iv.ch:
			batch = append(batch, c)
		}
		timer := time.NewTimer(iv.Debounce)
	collect:
		for {
			select {
			case c := <-iv.ch:
				batch = append(batch, c)
			case <-timer.C:
				break collect
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		iv.process(ctx, batch)
	}
}

func parent(p string) string {
	if p == "" {
		return ""
	}
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// Dirs reduces a batch of changes to the directories that need refreshing.
func Dirs(batch []Change, limit int) []string {
	set := map[string]bool{}
	for _, c := range batch {
		d := parent(c.Path)
		if c.Path == "" {
			d = ""
		}
		set[d] = true
		if c.Kind == Removed {
			set[parent(d)] = true
		}
	}
	for len(set) > limit {
		next := map[string]bool{}
		for d := range set {
			next[parent(d)] = true
		}
		set = next
	}
	// A parent and its child are separate listings, so both stay.
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	// Deepest first, so a parent is refreshed after its children.
	sort.Slice(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "/"), strings.Count(out[j], "/")
		if out[i] == "" {
			di = -1
		}
		if out[j] == "" {
			dj = -1
		}
		if di != dj {
			return di > dj
		}
		return out[i] < out[j]
	})
	return out
}

func (iv *Invalidator) process(ctx context.Context, batch []Change) {
	iv.count(func(s *Stats) { s.Events += int64(len(batch)); s.LastEvent = time.Now() })
	done := map[string]bool{}
	for _, d := range Dirs(batch, maxDirs) {
		iv.refreshDir(ctx, d, done)
	}
	if iv.PrefetchMax > 0 && iv.MountPoint != "" {
		for _, c := range batch {
			if c.Kind == Changed && c.Size > 0 && c.Size <= iv.PrefetchMax && done[parent(c.Path)] {
				select {
				case iv.prefetchQ <- c.Path:
				default:
				}
			}
		}
	}
}

// refreshDir refreshes d, walking up to the nearest existing ancestor.
func (iv *Invalidator) refreshDir(ctx context.Context, d string, done map[string]bool) {
	for {
		if done[d] {
			return
		}
		err := iv.RC.Refresh(ctx, d)
		if err == nil {
			done[d] = true
			iv.count(func(s *Stats) { s.Refreshes++ })
			if iv.Kicker != nil {
				if kerr := iv.Kicker.Kick(d); kerr != nil {
					iv.count(func(s *Stats) { s.Errors++ })
					iv.Log.Warn("kernel cache kick failed", "mount", iv.Name, "dir", d, "err", kerr)
				} else {
					iv.count(func(s *Stats) { s.Kicks++ })
				}
			}
			iv.Log.Debug("refreshed", "mount", iv.Name, "dir", d)
			return
		}
		if d == "" {
			iv.count(func(s *Stats) { s.Errors++ })
			iv.Log.Warn("refreshing root failed", "mount", iv.Name, "err", err)
			return
		}
		// Most likely the directory itself is gone (or never was) - its
		// parent's listing is what's stale then.
		iv.Log.Debug("refresh failed, trying parent", "mount", iv.Name, "dir", d, "err", err)
		d = parent(d)
	}
}

func (iv *Invalidator) prefetcher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-iv.prefetchQ:
			f, err := os.Open(path.Join(iv.MountPoint, p))
			if err != nil {
				continue
			}
			_, err = io.Copy(io.Discard, f)
			f.Close()
			if err == nil {
				iv.count(func(s *Stats) { s.Prefetched++ })
			}
		}
	}
}
