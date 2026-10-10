package invalidate

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/rclone"
)

func TestDirs(t *testing.T) {
	got := Dirs([]Change{
		{Path: "tv/show/s01/e01.mkv"},
		{Path: "tv/show/s01/e02.mkv"},
		{Path: "top.txt"},
		{Path: "movies/a/a.mkv", Kind: Removed},
	}, 64)
	want := []string{"tv/show/s01", "movies/a", "movies", ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}

	var burst []Change
	for i := 0; i < 1000; i++ {
		burst = append(burst, Change{Path: "sync/" + strconv.Itoa(i) + "/f"})
	}
	if got := Dirs(burst, 64); !reflect.DeepEqual(got, []string{"sync"}) {
		t.Errorf("burst not collapsed: %q", got)
	}
}

type fakeKicker struct {
	mu    sync.Mutex
	kicks []string
}

func (k *fakeKicker) Kick(d string) error {
	k.mu.Lock()
	k.kicks = append(k.kicks, d)
	k.mu.Unlock()
	return nil
}

// A refresh of a directory that doesn't exist (any more) falls back to its
// parent, and only successfully refreshed directories are kicked.
func TestWalkUp(t *testing.T) {
	existing := map[string]bool{"": true, "a": true}
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]string
		_ = json.Unmarshal(body, &p)
		d := p["dir"]
		mu.Lock()
		calls = append(calls, d)
		mu.Unlock()
		res := "OK"
		if !existing[d] {
			res = "file does not exist"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]string{d: res}})
	}))
	defer srv.Close()

	k := &fakeKicker{}
	iv := &Invalidator{Name: "t", RC: &rclone.RC{Addr: srv.URL, Client: srv.Client()}, Kicker: k,
		Debounce: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go iv.Run(ctx)
	iv.Notify(Change{Path: "a/new/deeper/file"})
	iv.Notify(Change{Path: "a/new/deeper/file2"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if iv.Stats().Refreshes > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"a/new/deeper", "a/new", "a"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("refresh calls %q want %q", calls, want)
	}
	if !reflect.DeepEqual(k.kicks, []string{"a"}) {
		t.Errorf("kicks %q", k.kicks)
	}
}
