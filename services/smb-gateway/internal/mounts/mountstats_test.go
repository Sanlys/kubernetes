package mounts

import (
	"strings"
	"testing"
)

// Trimmed real /proc/self/mountstats (Linux 6.x, NFSv3).
const mountstats = `device proc mounted on /proc with fstype proc
device 127.0.0.1:/ mounted on /srv/smb/media with fstype nfs statvers=1.1
	opts:	rw,vers=3,rsize=1048576,wsize=1048576
	age:	120
	caps:	caps=0x3fe7,wtmult=512,dtsize=8192,bsize=0,namlen=255
	sec:	flavor=1,pseudoflavor=1
	events:	3 50 0 0 3 4 60 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
	bytes:	134217728 0 0 0 134217728 0 32768 0
	RPC iostats version: 1.1  p/v: 100003/3 (nfs)
	xprt:	tcp 0 0 1 0 0 200 200 0 200 0 2 3 0
	per-op statistics
	        NULL: 1 1 0 44 24 0 0 0 0
	     GETATTR: 40 40 0 4960 4480 0 12 15 0
	        READ: 128 128 0 17408 134234112 5 640 700 0
	       WRITE: 0 0 0 0 0 0 0 0 0
device 127.0.0.1:/ mounted on /srv/smb/my\040photos with fstype nfs statvers=1.1
	per-op statistics
	        READ: 2 2 0 272 2048 0 1 1 0
`

func TestParseMountstats(t *testing.T) {
	got := ParseMountstats(strings.NewReader(mountstats))
	if len(got) != 2 {
		t.Fatalf("want 2 nfs mounts, got %d: %v", len(got), got)
	}
	r := got["/srv/smb/media"]["READ"]
	if r.Ops != 128 || r.BytesRecv != 134234112 || r.RTTMs != 640 || r.ExecMs != 700 {
		t.Errorf("READ = %+v", r)
	}
	if got["/srv/smb/media"]["GETATTR"].Ops != 40 {
		t.Error("GETATTR missing")
	}
	if got["/srv/smb/my photos"]["READ"].Ops != 2 {
		t.Errorf("escaped mountpoint not decoded: %v", got)
	}
}
