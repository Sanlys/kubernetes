package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestExampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/media":             TransportNFS,
		"/photos":            TransportNFS,
		"/backups/syncthing": TransportFUSE,
		"/syncthing":         TransportNFS,
	}
	for _, m := range c.Mounts {
		if w, ok := want[m.Path]; ok && c.ResolveTransport(m) != w {
			t.Errorf("%s: transport %s, want %s", m.Path, c.ResolveTransport(m), w)
		}
	}
	for _, m := range c.Mounts {
		if m.Type == TypeRestic && !m.ReadOnly {
			t.Error("restic mounts must be forced read-only")
		}
	}
	if got := c.ExpandPrincipals([]string{"@tv", "sander"}); strings.Join(got, ",") != "guest-tv,sander" {
		t.Errorf("ExpandPrincipals = %v", got)
	}
}

const base = `
s3: {endpoint: http://x, access_key_id: a, secret_access_key: b}
notifications: {endpoint_url: http://gw:8090, token: 0123456789abcdef}
users: [{name: u, password: p}]
shares: [{name: s, path: /, read: [u]}]
`

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"nested":        "mounts: [{path: /a, type: s3, bucket: x}, {path: /a/b, type: s3, bucket: y}]",
		"root mount":    "mounts: [{path: /, type: s3, bucket: x}]",
		"dotdot":        "mounts: [{path: /a/../../b, type: s3, bucket: x}]",
		"restic nfs":    "mounts: [{path: /r, type: restic, repository: r, password: p, transport: nfs}]",
		"unknown field": "mounts: [{path: /a, type: s3, bucket: x, buckett: y}]",
		"unknown type":  "mounts: [{path: /a, type: ftp}]",
	}
	for name, mounts := range cases {
		if _, err := Parse([]byte(base + mounts)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := Parse([]byte(base + "mounts: [{path: /a, type: s3, bucket: x}, {path: /ab, type: s3, bucket: y}]")); err != nil {
		t.Errorf("sibling paths sharing a prefix must be allowed: %v", err)
	}
	bad := strings.Replace(base, "read: [u]", "read: [nobody]", 1)
	if _, err := Parse([]byte(bad + "mounts: []")); err == nil {
		t.Error("unknown user in share must fail")
	}
	inj := strings.Replace(base, "path: /, read", "path: /, extra: {\"x\": \"y\\n[evil]\"}, read", 1)
	if _, err := Parse([]byte(inj + "mounts: []")); err == nil {
		t.Error("newline in extra option must fail")
	}
}

func TestNoFeedMeansFUSE(t *testing.T) {
	c, err := Parse([]byte(base + "mounts: [{path: /a, type: s3, bucket: x, notifications: false}, {path: /b, type: syncthing, sftp: {host: h, user: u, password: p}}]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Mounts {
		if c.ResolveTransport(m) != TransportFUSE {
			t.Errorf("%s has no change feed, want fuse", m.Path)
		}
		if c.DirCacheTime(m) != time.Minute {
			t.Errorf("%s: dir cache time %s", m.Path, c.DirCacheTime(m))
		}
	}
}

func TestDigestIgnoresUsers(t *testing.T) {
	a, _ := Parse([]byte(base + "mounts: [{path: /a, type: s3, bucket: x}]"))
	b, _ := Parse([]byte(strings.Replace(base, "password: p", "password: q", 1) + "mounts: [{path: /a, type: s3, bucket: x}]"))
	c, _ := Parse([]byte(base + "mounts: [{path: /a, type: s3, bucket: y}]"))
	if a.MountsDigest() != b.MountsDigest() {
		t.Error("password change must not require a restart")
	}
	if a.MountsDigest() == c.MountsDigest() {
		t.Error("bucket change must require a restart")
	}
}
