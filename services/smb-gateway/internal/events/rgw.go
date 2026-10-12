// Package events receives change notifications - Ceph RGW bucket
// notifications pushed over HTTP, and Syncthing's event API - and routes them
// to the invalidator of every mount they affect.
package events

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/invalidate"
)

// Target is one s3 mount listening for changes in a bucket (under prefix).
type Target struct {
	Bucket string
	Prefix string
	Inv    *invalidate.Invalidator
}

// S3 event records, as sent by RGW (and AWS/MinIO, which share the format).
type s3Event struct {
	Records []struct {
		EventSource string `json:"eventSource"`
		EventName   string `json:"eventName"`
		S3          struct {
			Bucket struct {
				Name string `json:"name"`
			} `json:"bucket"`
			Object struct {
				Key  string `json:"key"`
				Size int64  `json:"size"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

// RGWHandler accepts pushes at /rgw/<token>.
type RGWHandler struct {
	Token   string
	Targets []Target
	Log     *slog.Logger
}

func (h *RGWHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.URL.Path, "/rgw/")
	if h.Token == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(h.Token)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var ev s3Event
	if err := json.Unmarshal(body, &ev); err != nil {
		// RGW retries persistent notifications on non-2xx; a body we can't
		// parse will never parse, so ack it.
		h.Log.Warn("unparseable notification", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	for _, rec := range ev.Records {
		key := rec.S3.Object.Key
		// AWS and MinIO URL-encode keys in events; RGW sends them raw.
		if !strings.HasPrefix(rec.EventSource, "ceph") {
			if k, err := url.QueryUnescape(key); err == nil {
				key = k
			}
		}
		kind := invalidate.Changed
		if strings.Contains(rec.EventName, "ObjectRemoved") {
			kind = invalidate.Removed
		}
		h.route(rec.S3.Bucket.Name, key, kind, rec.S3.Object.Size)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *RGWHandler) route(bucket, key string, kind invalidate.Kind, size int64) {
	for _, t := range h.Targets {
		if t.Bucket != bucket {
			continue
		}
		rel := key
		if t.Prefix != "" {
			if !strings.HasPrefix(key, t.Prefix+"/") {
				continue
			}
			rel = strings.TrimPrefix(key, t.Prefix+"/")
		}
		t.Inv.Notify(invalidate.Change{Path: rel, Kind: kind, Size: size})
	}
}
