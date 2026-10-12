package mounts

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// NFSOpStats are the kernel NFS client's cumulative per-operation counters
// for one mount, from /proc/self/mountstats.
type NFSOpStats struct {
	Ops, Trans, Timeouts   float64
	BytesSent, BytesRecv   float64
	QueueMs, RTTMs, ExecMs float64
	Errors                 float64
}

// ParseMountstats returns per-mountpoint, per-operation NFS stats.
func ParseMountstats(r io.Reader) map[string]map[string]NFSOpStats {
	out := map[string]map[string]NFSOpStats{}
	var cur map[string]NFSOpStats
	inOps := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "device ") {
			cur, inOps = nil, false
			// device <src> mounted on <mp> with fstype nfs ...
			f := strings.Fields(line)
			if len(f) >= 8 && f[2] == "mounted" && f[3] == "on" && f[5] == "with" && f[6] == "fstype" && strings.HasPrefix(f[7], "nfs") {
				cur = map[string]NFSOpStats{}
				out[unescapeMountinfo(f[4])] = cur
			}
			continue
		}
		if cur == nil {
			continue
		}
		t := strings.TrimSpace(line)
		if t == "per-op statistics" {
			inOps = true
			continue
		}
		if !inOps {
			continue
		}
		name, rest, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 8 {
			continue
		}
		v := make([]float64, 9)
		for i := 0; i < len(f) && i < 9; i++ {
			v[i], _ = strconv.ParseFloat(f[i], 64)
		}
		cur[name] = NFSOpStats{Ops: v[0], Trans: v[1], Timeouts: v[2], BytesSent: v[3], BytesRecv: v[4],
			QueueMs: v[5], RTTMs: v[6], ExecMs: v[7], Errors: v[8]}
	}
	return out
}
