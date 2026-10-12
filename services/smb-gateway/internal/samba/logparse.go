package samba

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/obs"
)

// smbd logs in syslog format ("debug syslog format = always"), one line
// per message: "<time> <host> smbd[<pid>]: <message>".
var syslogPrefix = regexp.MustCompile(`^\S+ \S+ (smbd|samba-dcerpcd|rpcd_\w+)\[\d+\]: ?`)

// LogParser re-emits smbd's output as structured JSON log lines and turns
// login and file-operation audit records into metrics.
type LogParser struct {
	Log      *slog.Logger
	TreeRoot string
	// KnownUser reports whether a name is a configured user. Unknown names
	// are counted as "unknown" so login attempts with made-up names can't
	// blow up the metric's cardinality.
	KnownUser func(string) bool
}

func (p *LogParser) Run(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		p.Line(sc.Text())
	}
}

func (p *LogParser) Line(line string) {
	msg := syslogPrefix.ReplaceAllString(line, "")
	if strings.TrimSpace(msg) == "" {
		return
	}
	if strings.HasPrefix(msg, "{") && p.auth(msg) {
		return
	}
	if p.audit(msg) {
		return
	}
	p.Log.Info(msg, "component", "smbd")
}

type authEvent struct {
	Type string `json:"type"`
	Auth *struct {
		Status        string  `json:"status"`
		ClientAccount string  `json:"clientAccount"`
		ClientDomain  string  `json:"clientDomain"`
		RemoteAddress string  `json:"remoteAddress"`
		Service       string  `json:"serviceDescription"`
		PasswordType  string  `json:"passwordType"`
		Workstation   *string `json:"workstation"`
	} `json:"Authentication"`
}

func (p *LogParser) auth(msg string) bool {
	var ev authEvent
	if json.Unmarshal([]byte(msg), &ev) != nil || ev.Type != "Authentication" || ev.Auth == nil {
		return false
	}
	a := ev.Auth
	result := "ok"
	if a.Status != "NT_STATUS_OK" {
		result = a.Status
	}
	user := a.ClientAccount
	label := user
	if p.KnownUser == nil || !p.KnownUser(user) {
		label = "unknown"
	}
	obs.SMBAuth.WithLabelValues(label, result).Inc()
	ws := ""
	if a.Workstation != nil {
		ws = *a.Workstation
	}
	attrs := []any{"component", "smbd", "event", "login", "user", user, "domain", a.ClientDomain,
		"client", strings.TrimPrefix(a.RemoteAddress, "ipv4:"), "workstation", ws,
		"status", a.Status, "password_type", a.PasswordType, "service", a.Service}
	if result == "ok" {
		p.Log.Info("smb login", attrs...)
	} else {
		p.Log.Warn("smb login failed", attrs...)
	}
	return true
}

// audit parses vfs_full_audit lines: user|client|share|op|result|args...
func (p *LogParser) audit(msg string) bool {
	f := strings.Split(msg, "|")
	if len(f) < 5 {
		return false
	}
	user, client, share, op, res := f[0], f[1], f[2], f[3], f[4]
	if !(res == "ok" || strings.HasPrefix(res, "fail")) {
		return false
	}
	args := f[5:]
	level := slog.LevelInfo
	switch op {
	case "openat":
		// Every directory listing opens the directory too: reads are
		// debug-level, opens for writing are interesting.
		op = "open_read"
		if len(args) > 0 && strings.Contains(args[0], "w") {
			op = "open_write"
		} else {
			level = slog.LevelDebug
		}
		if len(args) > 0 {
			args = args[1:]
		}
	case "connect":
		op = "connect_share"
	case "disconnect":
		op = "disconnect_share"
	}
	result := "ok"
	reason := ""
	if res != "ok" {
		result = "fail"
		reason = strings.TrimSuffix(strings.TrimPrefix(res, "fail ("), ")")
		// Clients probe for files that don't exist all the time.
		if strings.Contains(reason, "No such file") {
			level = slog.LevelDebug
		} else {
			level = slog.LevelWarn
		}
	}
	label := user
	if p.KnownUser == nil || !p.KnownUser(user) {
		label = "unknown"
	}
	obs.SMBFileOps.WithLabelValues(share, label, op, result).Inc()

	attrs := []any{"component", "smbd", "event", "file_op", "user", user, "client", client, "share", share, "op", op, "result", result}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	paths := make([]string, 0, len(args))
	for _, a := range args {
		paths = append(paths, p.relPath(a))
	}
	switch {
	case op == "renameat" && len(paths) >= 2:
		attrs = append(attrs, "path", paths[0], "to", paths[1])
	case len(paths) > 0:
		attrs = append(attrs, "path", paths[0])
	}
	p.Log.Log(context.Background(), level, "smb "+op, attrs...)
	return true
}

func (p *LogParser) relPath(s string) string {
	if p.TreeRoot != "" && strings.HasPrefix(s, p.TreeRoot) {
		s = strings.TrimPrefix(s, p.TreeRoot)
		if s == "" {
			s = "/"
		}
	}
	return s
}
