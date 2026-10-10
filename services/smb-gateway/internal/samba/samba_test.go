package samba

import (
	"strings"
	"testing"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/config"
)

func TestRender(t *testing.T) {
	c, err := config.Parse([]byte(`
s3: {endpoint: http://x, access_key_id: a, secret_access_key: b}
notifications: {endpoint_url: http://gw:8090, token: 0123456789abcdef}
mounts:
  - {path: /media, type: s3, bucket: m}
  - {path: /backups/r, type: restic, repository: r, password: p}
users: [{name: alice, password: p}, {name: bob, password: p}]
groups: {fam: [alice, bob]}
shares:
  - {name: media, path: /media, read: ["@fam"], write: [alice], veto: ["*.part"]}
  - {name: backups, path: /backups, read: [alice], write: [alice]}
`))
	if err != nil {
		t.Fatal(err)
	}
	ro := func(p string) bool { return strings.HasPrefix(p, "/backups") }
	out := Render(c, "/srv/smb", ro)
	section := func(name string) string {
		i := strings.Index(out, "["+name+"]")
		if i < 0 {
			t.Fatalf("no [%s] section", name)
		}
		rest := out[i+1:]
		if j := strings.Index(rest, "\n["); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	media := section("media")
	for _, want := range []string{
		"path = /srv/smb/media", "valid users = alice bob", "write list = alice", "read only = yes", "*.part/",
	} {
		if !strings.Contains(media, want) {
			t.Errorf("[media] lacks %q:\n%s", want, media)
		}
	}
	if strings.Contains(section("backups"), "write list") {
		t.Error("a share on read-only mounts must not get a write list")
	}
	for _, want := range []string{"map to guest = never", "posix locking = no", "force user = smbdata", "wide links = no"} {
		if !strings.Contains(section("global"), want) {
			t.Errorf("[global] lacks %q", want)
		}
	}
}
