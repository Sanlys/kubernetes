// Package config loads and validates the gateway's single YAML config file:
// which buckets/repos/folders get mounted where, and who may access them.
// It is stored in the cluster as a sops-encrypted Secret, so it may contain
// credentials and passwords directly.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mount types.
const (
	TypeS3        = "s3"
	TypeRestic    = "restic"
	TypeSyncthing = "syncthing"
)

// Transports for rclone-backed mounts.
const (
	TransportNFS  = "nfs"
	TransportFUSE = "fuse"
)

type Config struct {
	Server        Server              `yaml:"server"`
	S3            S3                  `yaml:"s3"`
	Notifications Notifications       `yaml:"notifications"`
	Cache         Cache               `yaml:"cache"`
	Mounts        []*Mount            `yaml:"mounts"`
	Users         []User              `yaml:"users"`
	Groups        map[string][]string `yaml:"groups"`
	Shares        []*Share            `yaml:"shares"`
}

type Server struct {
	// Name is used as the NetBIOS name and to namespace the RGW topic and
	// bucket notification ids, so two gateways never clobber each other.
	Name        string `yaml:"name"`
	Workgroup   string `yaml:"workgroup"`
	Description string `yaml:"description"`
	MinProtocol string `yaml:"min_protocol"`
	LogLevel    int    `yaml:"log_level"`
	// Extra lines appended verbatim to [global] - escape hatch.
	ExtraGlobal map[string]string `yaml:"extra_global"`
}

// S3 connection settings. The top-level block is the default for every
// mount; a mount's own s3 block overrides individual fields.
type S3 struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
}

func (s S3) merge(o *S3) S3 {
	if o == nil {
		return s
	}
	if o.Endpoint != "" {
		s.Endpoint = o.Endpoint
	}
	if o.Region != "" {
		s.Region = o.Region
	}
	if o.AccessKeyID != "" {
		s.AccessKeyID = o.AccessKeyID
	}
	if o.SecretAccessKey != "" {
		s.SecretAccessKey = o.SecretAccessKey
	}
	return s
}

type Notifications struct {
	// Enabled turns on change-feed driven cache invalidation for s3 mounts.
	Enabled *bool `yaml:"enabled"`
	// Manage makes the gateway create the RGW topic and bucket notification
	// configs itself (idempotently) instead of expecting them to exist.
	Manage *bool `yaml:"manage"`
	// URL RGW pushes events to. Must be reachable from the RGW pods.
	EndpointURL string `yaml:"endpoint_url"`
	// Token is part of the push URL path; requests without it are rejected.
	Token string `yaml:"token"`
	// TopicTTL bounds how long RGW keeps retrying an undeliverable event.
	TopicTTL Duration `yaml:"topic_ttl"`
}

type Cache struct {
	// Defaults for every rclone mount, overridable per mount.
	MaxSize      string   `yaml:"max_size"`
	MaxAge       Duration `yaml:"max_age"`
	MinFreeSpace string   `yaml:"min_free_space"`
	WriteBack    Duration `yaml:"write_back"`
	ReadAhead    string   `yaml:"read_ahead"`
	BufferSize   string   `yaml:"buffer_size"`
	// Debounce is how long the invalidator coalesces change events before
	// refreshing, so a burst of uploads into one folder costs one LIST.
	Debounce Duration `yaml:"debounce"`
}

type MountCache struct {
	MaxSize string   `yaml:"max_size"`
	MaxAge  Duration `yaml:"max_age"`
	// DirCacheTime is how long rclone trusts a directory listing without a
	// change event. Defaults depend on whether the mount has a change feed.
	DirCacheTime Duration `yaml:"dir_cache_time"`
	// PrefetchMaxSize: when a new object shows up (via the change feed) in a
	// folder someone is currently browsing, and it is at most this big, read
	// it into the local cache ahead of time. "0" (default) disables.
	PrefetchMaxSize string `yaml:"prefetch_max_size"`
}

type Mount struct {
	// Path in the gateway's tree, e.g. /media or /backups/syncthing.
	Path     string `yaml:"path"`
	Type     string `yaml:"type"`
	ReadOnly bool   `yaml:"read_only"`
	// Transport for rclone-backed types: nfs or fuse. Empty = decided from
	// whether the mount has a change feed (see ResolveTransport).
	Transport string `yaml:"transport"`

	// type: s3
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
	S3     *S3    `yaml:"s3"`
	// PreserveModtime stores/reads client mtimes as object metadata. Off by
	// default: reading it costs one HEAD request per file on every listing.
	PreserveModtime bool `yaml:"preserve_modtime"`
	// Notifications can be disabled for a single mount.
	Notifications *bool `yaml:"notifications"`

	// type: restic
	Repository string `yaml:"repository"`
	Password   string `yaml:"password"`

	// type: syncthing
	SFTP      *SFTP         `yaml:"sftp"`
	Syncthing *SyncthingAPI `yaml:"syncthing"`

	Cache MountCache `yaml:"cache"`

	// Resolved by Validate.
	ID         string `yaml:"-"`
	S3Resolved S3     `yaml:"-"`
}

type SFTP struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// PrivateKey (PEM) can be used instead of Password.
	PrivateKey string `yaml:"private_key"`
	// Path on the SFTP server that is mounted, e.g. "upload".
	Path string `yaml:"path"`
}

type SyncthingAPI struct {
	URL    string `yaml:"url"`
	APIKey string `yaml:"api_key"`
	// DataRoot is the path inside the Syncthing container that corresponds
	// to the SFTP path, e.g. /data. Used to translate event paths.
	DataRoot string `yaml:"data_root"`
}

type User struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password"`
}

type Share struct {
	Name    string `yaml:"name"`
	Path    string `yaml:"path"`
	Comment string `yaml:"comment"`
	// Read/Write list users or @groups. Write implies read.
	Read       []string `yaml:"read"`
	Write      []string `yaml:"write"`
	Browseable *bool    `yaml:"browseable"`
	// Extra veto patterns on top of the defaults.
	Veto  []string          `yaml:"veto"`
	Extra map[string]string `yaml:"extra"`
}

// Duration accepts Go durations plus a "d" suffix for days.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		var n float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%g", &n); err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func Load(file string) (*Config, []byte, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	c, err := Parse(raw)
	return c, raw, err
}

func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func boolPtr(b bool) *bool { return &b }

func (c *Config) applyDefaults() {
	if c.Server.Name == "" {
		c.Server.Name = "smb-gateway"
	}
	if c.Server.Workgroup == "" {
		c.Server.Workgroup = "WORKGROUP"
	}
	if c.Server.Description == "" {
		c.Server.Description = "SMB gateway"
	}
	if c.Server.MinProtocol == "" {
		c.Server.MinProtocol = "SMB2_10"
	}
	if c.S3.Region == "" {
		c.S3.Region = "us-east-1"
	}
	if c.Notifications.Enabled == nil {
		c.Notifications.Enabled = boolPtr(true)
	}
	if c.Notifications.Manage == nil {
		c.Notifications.Manage = boolPtr(true)
	}
	if c.Notifications.TopicTTL == 0 {
		c.Notifications.TopicTTL = Duration(time.Hour)
	}
	if c.Cache.MaxSize == "" {
		c.Cache.MaxSize = "10G"
	}
	if c.Cache.MaxAge == 0 {
		c.Cache.MaxAge = Duration(7 * 24 * time.Hour)
	}
	if c.Cache.MinFreeSpace == "" {
		c.Cache.MinFreeSpace = "2G"
	}
	if c.Cache.WriteBack == 0 {
		c.Cache.WriteBack = Duration(10 * time.Second)
	}
	if c.Cache.ReadAhead == "" {
		c.Cache.ReadAhead = "64M"
	}
	if c.Cache.BufferSize == "" {
		c.Cache.BufferSize = "16M"
	}
	if c.Cache.Debounce == 0 {
		c.Cache.Debounce = Duration(time.Second)
	}
	for _, m := range c.Mounts {
		if m.Cache.MaxSize == "" {
			m.Cache.MaxSize = c.Cache.MaxSize
		}
		if m.Cache.MaxAge == 0 {
			m.Cache.MaxAge = c.Cache.MaxAge
		}
		if m.Type == TypeRestic {
			m.ReadOnly = true
		}
		if m.SFTP != nil && m.SFTP.Port == 0 {
			m.SFTP.Port = 22
		}
		if m.Syncthing != nil && m.Syncthing.DataRoot == "" {
			m.Syncthing.DataRoot = "/data"
		}
	}
}

var (
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	shareRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,79}$`)
)

func cleanTreePath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q must be absolute (start with /)", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q must not contain ..", p)
		}
	}
	return path.Clean(p), nil
}

// within reports whether p is a or below a.
func within(p, a string) bool {
	return a == "/" || p == a || strings.HasPrefix(p, a+"/")
}

func (c *Config) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if !nameRe.MatchString(c.Server.Name) || len(c.Server.Name) > 15 {
		add("server.name %q must be 1-15 chars of [A-Za-z0-9._-] (it is used as the NetBIOS name)", c.Server.Name)
	}

	ids := map[string]bool{}
	for i, m := range c.Mounts {
		where := fmt.Sprintf("mounts[%d]", i)
		p, err := cleanTreePath(m.Path)
		if err != nil {
			add("%s: %v", where, err)
			continue
		}
		if p == "/" {
			add("%s: cannot mount at / - mounts live below the root of the tree", where)
			continue
		}
		m.Path = p
		m.ID = mountID(p)
		if ids[m.ID] {
			add("%s: duplicate mount id for path %s", where, p)
		}
		ids[m.ID] = true

		switch m.Transport {
		case "", TransportNFS, TransportFUSE:
		default:
			add("%s: transport must be nfs or fuse", where)
		}

		switch m.Type {
		case TypeS3:
			if m.Bucket == "" {
				add("%s: s3 mount needs bucket", where)
			}
			m.Prefix = strings.Trim(m.Prefix, "/")
			m.S3Resolved = c.S3.merge(m.S3)
			if m.S3Resolved.Endpoint == "" || m.S3Resolved.AccessKeyID == "" || m.S3Resolved.SecretAccessKey == "" {
				add("%s: s3 endpoint/access_key_id/secret_access_key must be set (top-level s3 or per mount)", where)
			}
		case TypeRestic:
			if m.Repository == "" || m.Password == "" {
				add("%s: restic mount needs repository and password", where)
			}
			if m.Transport == TransportNFS {
				add("%s: restic can only be mounted via fuse (restic mount)", where)
			}
			m.S3Resolved = c.S3.merge(m.S3)
		case TypeSyncthing:
			if m.SFTP == nil || m.SFTP.Host == "" || m.SFTP.User == "" || (m.SFTP.Password == "" && m.SFTP.PrivateKey == "") {
				add("%s: syncthing mount needs sftp.host, sftp.user and sftp.password or sftp.private_key", where)
			}
			if m.Syncthing != nil && (m.Syncthing.URL == "" || m.Syncthing.APIKey == "") {
				add("%s: syncthing.url and syncthing.api_key must both be set (or omit the syncthing block)", where)
			}
		default:
			add("%s: unknown type %q (want s3, restic or syncthing)", where, m.Type)
		}
	}
	// Mount points must not nest: a mount inside another would have to be
	// created as a directory inside the outer remote.
	for i, a := range c.Mounts {
		for j, b := range c.Mounts {
			if i != j && a.Path != "" && b.Path != "" && a.Path != b.Path && within(b.Path, a.Path) {
				add("mount %s is nested inside mount %s; mounts may share parent folders but not nest", b.Path, a.Path)
			}
		}
	}

	if *c.Notifications.Enabled && *c.Notifications.Manage && c.hasNotifiedS3() {
		if c.Notifications.EndpointURL == "" {
			add("notifications.endpoint_url must be set when notifications are managed")
		}
		if len(c.Notifications.Token) < 16 {
			add("notifications.token must be at least 16 characters")
		}
	}

	users := map[string]bool{}
	for i, u := range c.Users {
		if !nameRe.MatchString(u.Name) || strings.ContainsAny(u.Name, ".") {
			add("users[%d]: invalid name %q", i, u.Name)
		}
		if u.Password == "" {
			add("users[%d] (%s): password must be set", i, u.Name)
		}
		if users[strings.ToLower(u.Name)] {
			add("users[%d]: duplicate user %s", i, u.Name)
		}
		users[strings.ToLower(u.Name)] = true
	}
	for g, members := range c.Groups {
		for _, m := range members {
			if !users[strings.ToLower(m)] {
				add("groups.%s: unknown user %q", g, m)
			}
		}
	}

	shareNames := map[string]bool{}
	for i, s := range c.Shares {
		where := fmt.Sprintf("shares[%d]", i)
		if !shareRe.MatchString(s.Name) {
			add("%s: invalid share name %q", where, s.Name)
		}
		switch strings.ToLower(s.Name) {
		case "global", "homes", "printers", "ipc$":
			add("%s: share name %q is reserved", where, s.Name)
		}
		if shareNames[strings.ToLower(s.Name)] {
			add("%s: duplicate share name %s", where, s.Name)
		}
		shareNames[strings.ToLower(s.Name)] = true
		p, err := cleanTreePath(s.Path)
		if err != nil {
			add("%s: %v", where, err)
		} else {
			s.Path = p
		}
		if len(s.Read)+len(s.Write) == 0 {
			add("%s: no users have access (set read and/or write)", where)
		}
		for _, who := range append(append([]string{}, s.Read...), s.Write...) {
			if strings.HasPrefix(who, "@") {
				if _, ok := c.Groups[who[1:]]; !ok {
					add("%s: unknown group %s", where, who)
				}
			} else if !users[strings.ToLower(who)] {
				add("%s: unknown user %s", where, who)
			}
		}
		for k, v := range s.Extra {
			if strings.ContainsAny(k+v, "\n\r[]") {
				add("%s: extra option %q contains illegal characters", where, k)
			}
		}
		for _, v := range s.Veto {
			if strings.ContainsAny(v, "/\n\r") {
				add("%s: veto pattern %q must not contain / or newlines", where, v)
			}
		}
		if strings.ContainsAny(s.Comment, "\n\r") {
			add("%s: comment must be a single line", where)
		}
	}
	for k, v := range c.Server.ExtraGlobal {
		if strings.ContainsAny(k+v, "\n\r[]") {
			add("server.extra_global %q contains illegal characters", k)
		}
	}
	if strings.ContainsAny(c.Server.Description+c.Server.Workgroup, "\n\r") {
		add("server.description/workgroup must be single line")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func (c *Config) hasNotifiedS3() bool {
	for _, m := range c.Mounts {
		if m.Type == TypeS3 && c.MountNotified(m) {
			return true
		}
	}
	return false
}

// MountNotified reports whether a mount receives a change feed.
func (c *Config) MountNotified(m *Mount) bool {
	switch m.Type {
	case TypeS3:
		if m.Notifications != nil {
			return *m.Notifications && *c.Notifications.Enabled
		}
		return *c.Notifications.Enabled
	case TypeSyncthing:
		return m.Syncthing != nil
	}
	return false
}

// ResolveTransport picks NFS or FUSE for a mount.
//
// NFS (rclone serve nfs + the kernel NFS client over loopback) is faster to
// open files on this cluster and survives an rclone restart without
// remounting, but the kernel's directory cache is only invalidated when a
// directory's mtime changes, and rclone reports a constant mtime for
// directories on S3/SFTP-like remotes. The gateway can bump that mtime when it
// learns about a change - but only if the mount has a change feed. Without
// one, FUSE (whose kernel caches expire after a second) is the only way to
// eventually see changes made by others, so that is the default there.
// restic has no NFS mode at all.
func (c *Config) ResolveTransport(m *Mount) string {
	if m.Type == TypeRestic {
		return TransportFUSE
	}
	if m.Transport != "" {
		return m.Transport
	}
	if c.MountNotified(m) {
		return TransportNFS
	}
	return TransportFUSE
}

// DirCacheTime resolves the rclone --dir-cache-time for a mount.
func (c *Config) DirCacheTime(m *Mount) time.Duration {
	if m.Cache.DirCacheTime != 0 {
		return m.Cache.DirCacheTime.D()
	}
	if c.MountNotified(m) {
		// The change feed is the primary invalidation path; this only bounds
		// the damage of a lost event for rclone's own view.
		return time.Hour
	}
	return time.Minute
}

func mountID(p string) string {
	s := strings.Trim(p, "/")
	s = regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.ToLower(strings.Trim(s, "-"))
	if len(s) > 40 {
		s = s[:40]
	}
	h := sha256.Sum256([]byte(p))
	return s + "-" + hex.EncodeToString(h[:3])
}

// ExpandPrincipals turns user names and @groups into a sorted, de-duplicated
// list of user names.
func (c *Config) ExpandPrincipals(list []string) []string {
	set := map[string]bool{}
	for _, who := range list {
		if strings.HasPrefix(who, "@") {
			for _, u := range c.Groups[who[1:]] {
				set[c.canonicalUser(u)] = true
			}
		} else {
			set[c.canonicalUser(who)] = true
		}
	}
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func (c *Config) canonicalUser(n string) string {
	for _, u := range c.Users {
		if strings.EqualFold(u.Name, n) {
			return u.Name
		}
	}
	return n
}

// MountsDigest identifies everything that requires a restart to change
// (mounts, server, s3, cache, notifications). Users and shares can be
// reloaded live.
func (c *Config) MountsDigest() string {
	type restartRelevant struct {
		Server        Server
		S3            S3
		Notifications Notifications
		Cache         Cache
		Mounts        []*Mount
	}
	b, _ := yaml.Marshal(restartRelevant{c.Server, c.S3, c.Notifications, c.Cache, c.Mounts})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
