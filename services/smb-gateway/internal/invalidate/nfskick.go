package invalidate

import (
	"errors"
	"fmt"
	"os"
	"path"
	"sync"

	"github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// NFSKicker bumps a directory's mtime by talking NFSv3 to rclone directly,
// next to (not through) the kernel mount. The kernel then sees a changed
// mtime on its next GETATTR - which close-to-open semantics issue on every
// open/opendir - and drops its cached listing. Going through the kernel
// mount instead would not work: the kernel would update its own cached
// mtime along with the SETATTR and keep the stale listing.
type NFSKicker struct {
	Addr string // 127.0.0.1:port of rclone serve nfs

	mu     sync.Mutex
	target *nfs.Target
	client *rpc.Client
}

func (k *NFSKicker) connect() (*nfs.Target, error) {
	if k.target != nil {
		return k.target, nil
	}
	c, err := rpc.DialTCP("tcp", k.Addr, false)
	if err != nil {
		return nil, err
	}
	// rclone serves MOUNT and NFS on the same port and there is no
	// portmapper, so reuse one connection for both.
	m := &nfs.Mount{Client: c}
	t, err := m.Mount("/", rpc.NewAuthUnix("smb-gateway", 0, 0).Auth())
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("nfs mount: %w", err)
	}
	k.client, k.target = c, t
	return t, nil
}

func (k *NFSKicker) reset() {
	if k.client != nil {
		k.client.Close()
	}
	k.client, k.target = nil, nil
}

func (k *NFSKicker) Kick(dir string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var t *nfs.Target
		t, err = k.connect()
		if err != nil {
			k.reset()
			continue
		}
		p := "."
		if dir != "" {
			p = path.Clean(dir)
		}
		var fh []byte
		_, fh, err = t.Lookup(p)
		if err != nil {
			// A missing dir is not worth a reconnect.
			if nfs.IsNotDirError(err) || isNoEnt(err) {
				return err
			}
			k.reset()
			continue
		}
		_, err = t.SetAttr(fh, nfs.Sattr3{Mtime: nfs.SetTime{SetIt: nfs.SetToServerTime}})
		if err == nil {
			return nil
		}
		k.reset()
	}
	return err
}

func isNoEnt(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
