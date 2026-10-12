package samba

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/obs"
)

// Real lines from Samba 4.23 with "debug syslog format = always",
// auth_json_audit:3 and vfs_full_audit.
const sample = `2026-10-12T19:56:08.295898+00:00 gw smbd[1]: {"timestamp": "2026-10-12T19:56:11.251551+0000", "type": "Authentication", "Authentication": {"version": {"major": 1, "minor": 3}, "eventId": 4625, "status": "NT_STATUS_WRONG_PASSWORD", "remoteAddress": "ipv4:10.0.0.5:55916", "serviceDescription": "SMB2", "clientDomain": "WORKGROUP", "clientAccount": "sander", "workstation": "MAC", "passwordType": "NTLMv2"}}
2026-10-12T19:56:08.295898+00:00 gw smbd[1]: {"timestamp": "2026-10-12T19:56:11.251551+0000", "type": "Authentication", "Authentication": {"status": "NT_STATUS_NO_SUCH_USER", "remoteAddress": "ipv4:10.0.0.9:1", "clientAccount": "admin", "workstation": null}}
2026-10-12T19:56:22.599827+00:00 gw smbd[18]: sander|10.0.0.5|files|openat|ok|r|/srv/smb
2026-10-12T19:56:22.599866+00:00 gw smbd[18]: sander|10.0.0.5|files|openat|ok|w|/srv/smb/media/new.mkv
2026-10-12T19:56:22.600297+00:00 gw smbd[18]: sander|10.0.0.5|files|renameat|ok|/srv/smb/media/a.txt|/srv/smb/media/b.txt
2026-10-12T19:56:22.602482+00:00 gw smbd[18]: sander|10.0.0.5|files|unlinkat|fail (Read-only file system)|/srv/smb/photos/x.jpg
2026-10-12T19:56:08.245697+00:00 gw smbd[1]: smbd version 4.23.8 started.`

func TestLogParser(t *testing.T) {
	var buf bytes.Buffer
	p := &LogParser{
		Log:       slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		TreeRoot:  "/srv/smb",
		KnownUser: func(u string) bool { return u == "sander" },
	}
	p.Run(strings.NewReader(sample))

	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %s", l)
		}
		lines = append(lines, m)
	}
	if len(lines) != 7 {
		t.Fatalf("want 7 lines, got %d", len(lines))
	}
	if lines[0]["msg"] != "smb login failed" || lines[0]["status"] != "NT_STATUS_WRONG_PASSWORD" || lines[0]["client"] != "10.0.0.5:55916" {
		t.Errorf("auth line: %v", lines[0])
	}
	if lines[2]["level"] != "DEBUG" || lines[2]["op"] != "open_read" || lines[2]["path"] != "/" {
		t.Errorf("directory open: %v", lines[2])
	}
	if lines[3]["level"] != "INFO" || lines[3]["op"] != "open_write" || lines[3]["path"] != "/media/new.mkv" {
		t.Errorf("write open: %v", lines[3])
	}
	if lines[4]["path"] != "/media/a.txt" || lines[4]["to"] != "/media/b.txt" {
		t.Errorf("rename: %v", lines[4])
	}
	if lines[5]["level"] != "WARN" || lines[5]["reason"] != "Read-only file system" {
		t.Errorf("failed unlink: %v", lines[5])
	}
	if lines[6]["msg"] != "smbd version 4.23.8 started." {
		t.Errorf("plain line: %v", lines[6])
	}
	if v := testutil.ToFloat64(obs.SMBAuth.WithLabelValues("unknown", "NT_STATUS_NO_SUCH_USER")); v != 1 {
		t.Errorf("made-up user names must be counted as unknown, got %v", v)
	}
	if v := testutil.ToFloat64(obs.SMBFileOps.WithLabelValues("files", "sander", "unlinkat", "fail")); v != 1 {
		t.Errorf("file op metric = %v", v)
	}
}
