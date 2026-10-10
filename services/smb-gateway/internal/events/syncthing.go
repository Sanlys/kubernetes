package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/config"
	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/invalidate"
)

// Syncthing long-polls Syncthing's REST event API and forwards file changes
// in its folders - both changes Syncthing pulled in from other devices
// (ItemFinished) and changes made directly on disk (LocalChangeDetected) -
// to the invalidator of the mount exposing Syncthing's data directory.
type Syncthing struct {
	API *config.SyncthingAPI
	Inv *invalidate.Invalidator
	Log *slog.Logger

	http    *http.Client
	folders map[string]string // folder id -> path relative to DataRoot
	fetched time.Time
}

type stEvent struct {
	ID   int64           `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (s *Syncthing) get(ctx context.Context, p string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.API.URL, "/")+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", s.API.APIKey)
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", p, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *Syncthing) refreshFolders(ctx context.Context) error {
	var folders []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := s.get(ctx, "/rest/config/folders", &folders); err != nil {
		return err
	}
	root := path.Clean(s.API.DataRoot)
	m := map[string]string{}
	for _, f := range folders {
		p := path.Clean(f.Path)
		if p == root {
			m[f.ID] = ""
		} else if strings.HasPrefix(p, root+"/") {
			m[f.ID] = strings.TrimPrefix(p, root+"/")
		} else {
			s.Log.Info("syncthing folder is outside data_root, ignoring", "folder", f.ID, "path", f.Path)
		}
	}
	s.folders, s.fetched = m, time.Now()
	return nil
}

func (s *Syncthing) Run(ctx context.Context) {
	s.http = &http.Client{Timeout: 90 * time.Second}
	var since int64 = -1
	backoff := time.Second
	for ctx.Err() == nil {
		if err := s.poll(ctx, &since); err != nil && ctx.Err() == nil {
			s.Log.Warn("syncthing events", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (s *Syncthing) poll(ctx context.Context, since *int64) error {
	if s.folders == nil || time.Since(s.fetched) > 10*time.Minute {
		if err := s.refreshFolders(ctx); err != nil {
			return err
		}
	}
	if *since < 0 {
		// Start from the newest event; anything older is already reflected
		// in a cache that has only just been created.
		var latest []stEvent
		if err := s.get(ctx, "/rest/events?limit=1&timeout=0", &latest); err != nil {
			return err
		}
		*since = 0
		if len(latest) > 0 {
			*since = latest[len(latest)-1].ID
		}
		s.Log.Info("syncthing event feed connected", "since", *since, "folders", len(s.folders))
	}
	var evs []stEvent
	q := fmt.Sprintf("/rest/events?events=ItemFinished,LocalChangeDetected&since=%d&timeout=60", *since)
	if err := s.get(ctx, q, &evs); err != nil {
		return err
	}
	for _, e := range evs {
		if e.ID > *since {
			*since = e.ID
		}
		var d struct {
			Folder string `json:"folder"`
			Item   string `json:"item"` // ItemFinished
			Path   string `json:"path"` // LocalChangeDetected
			Action string `json:"action"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		rel := d.Item
		if rel == "" {
			rel = d.Path
		}
		base, ok := s.folders[d.Folder]
		if !ok || rel == "" {
			continue
		}
		kind := invalidate.Changed
		if d.Action == "delete" || d.Action == "deleted" {
			kind = invalidate.Removed
		}
		s.Inv.Notify(invalidate.Change{Path: path.Join(base, rel), Kind: kind})
	}
	return nil
}
