package invalidate

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// TestNFSKickChangesDirMtime runs a real `rclone serve nfs` and checks the
// two facts the NFS transport relies on: rclone reports a constant mtime for
// a directory even after its contents change behind its back, and a kick
// changes the mtime an independent NFS client (standing in for the kernel)
// sees. Set RCLONE to an rclone binary to run it.
func TestNFSKickChangesDirMtime(t *testing.T) {
	bin := os.Getenv("RCLONE")
	if bin == "" {
		t.Skip("RCLONE not set")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	// The memory backend's directories have no mtime of their own, like S3
	// prefixes.
	dir := t.TempDir()
	cmd := exec.Command(bin, "serve", "nfs", ":memory:", "--addr", addr, "--vfs-cache-mode", "full",
		"--cache-dir", filepath.Join(dir, "cache"), "--nfs-cache-type", "disk", "--dir-cache-time", "1h",
		"--rc", "--rc-addr", "127.0.0.1:0", "--rc-no-auth")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	var target *nfs.Target
	for i := 0; i < 50; i++ {
		c, err := rpc.DialTCP("tcp", addr, false)
		if err == nil {
			target, err = (&nfs.Mount{Client: c}).Mount("/", rpc.NewAuthUnix("t", 0, 0).Auth())
			if err == nil {
				break
			}
			c.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if target == nil {
		t.Fatal("could not connect to rclone serve nfs")
	}
	if _, err := target.Mkdir("shows", 0o755); err != nil {
		t.Fatal(err)
	}
	mtime := func() string {
		fi, _, err := target.Lookup("shows")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(fi.ModTime().UnixNano())
	}
	before := mtime()
	time.Sleep(1100 * time.Millisecond)
	if again := mtime(); again != before {
		t.Fatalf("directory mtime changed by itself (%s -> %s)", before, again)
	}
	k := &NFSKicker{Addr: addr}
	if err := k.Kick("shows"); err != nil {
		t.Fatal(err)
	}
	after := mtime()
	if after == before {
		t.Fatal("kick did not change the directory mtime seen by another NFS client")
	}
	if err := k.Kick(""); err != nil {
		t.Fatalf("kicking the root: %v", err)
	}
	if err := k.Kick("does/not/exist"); err == nil {
		t.Fatal("kicking a missing dir should fail")
	}
}
