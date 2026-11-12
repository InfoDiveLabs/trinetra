package pluginrt

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// Limits are the per-plugin resource ceilings.
type Limits struct {
	MemoryMax string
	CPUQuota  string
	TasksMax  int
}

// DefaultLimits applies to every plugin.
var DefaultLimits = Limits{MemoryMax: "256M", CPUQuota: "50%", TasksMax: 64}

const TemplateUnitPath = "/etc/systemd/system/trinetra-plugin@.service"

// DropInPath is the per-plugin drop-in file.
func DropInPath(id string) string {
	return "/etc/systemd/system/trinetra-plugin@" + id + ".service.d/50-trinetra.conf"
}

// RenderTemplateUnit is the shared hardened template.
func RenderTemplateUnit() string {
	return `[Unit]
Description=Trinetra plugin %i
PartOf=trinetra.service
After=trinetra.service

[Service]
Type=simple
Restart=no
DynamicUser=yes
SupplementaryGroups=trinetra-plugin
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictSUIDSGID=yes
RestrictNamespaces=yes
RestrictRealtime=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
CapabilityBoundingSet=
AmbientCapabilities=
UMask=0077
StateDirectory=trinetra-plugins/%i
RuntimeDirectory=trinetra-plugin/%i
RuntimeDirectoryMode=0700
`
}

func unitEscape(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// RenderDropIn sets ExecStart, credentials, env and the per-permission network policy; it returns "" for an invalid id.
func RenderDropIn(id, execPath, hostSocket, tokenPath string, perms []plugin.Permission, lim Limits) string {
	if !plugin.IDPattern.MatchString(id) {
		return ""
	}
	has := map[plugin.Permission]bool{}
	for _, p := range perms {
		has[p] = true
	}
	net := has[plugin.PermNetOutbound] || has[plugin.PermNetListen]
	var b strings.Builder
	b.WriteString("[Service]\n")
	b.WriteString("ExecStart=\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", unitEscape(execPath))
	fmt.Fprintf(&b, "LoadCredential=token:%s\n", unitEscape(tokenPath))
	fmt.Fprintf(&b, "Environment=%s=%s\n", plugin.EnvPluginID, id)
	fmt.Fprintf(&b, "Environment=%s=%s\n", plugin.EnvHostSocket, unitEscape(hostSocket))
	fmt.Fprintf(&b, "Environment=%s=/run/trinetra-plugin/%s/plugin.sock\n", plugin.EnvPluginSocket, id)
	fmt.Fprintf(&b, "Environment=%s=%%d/token\n", plugin.EnvTokenFile)
	fmt.Fprintf(&b, "Environment=%s=%%S/trinetra-plugins/%s\n", plugin.EnvDataDir, id)
	fmt.Fprintf(&b, "MemoryMax=%s\n", lim.MemoryMax)
	fmt.Fprintf(&b, "CPUQuota=%s\n", unitEscape(lim.CPUQuota))
	fmt.Fprintf(&b, "TasksMax=%d\n", lim.TasksMax)
	families := "AF_UNIX"
	if net {
		families += " AF_INET AF_INET6"
	}
	fmt.Fprintf(&b, "RestrictAddressFamilies=%s\n", families)
	if !net {
		b.WriteString("IPAddressDeny=any\n")
	}
	if !has[plugin.PermNetListen] {
		b.WriteString("SocketBindDeny=any\n")
	}
	return b.String()
}

// Rlimit is one resource limit for the non-systemd fallback.
type Rlimit struct {
	Resource int
	Cur, Max uint64
}

func parseSize(s string) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	return n * mult, nil
}

// Rlimits is the non-systemd fallback: RLIMIT_AS = 2×MemoryMax, NPROC = TasksMax, NOFILE 1024, CORE 0.
func Rlimits(lim Limits) ([]Rlimit, error) {
	mem, err := parseSize(lim.MemoryMax)
	if err != nil {
		return nil, err
	}
	if lim.TasksMax <= 0 {
		return nil, fmt.Errorf("tasks max %d must be positive", lim.TasksMax)
	}
	return []Rlimit{
		{Resource: syscall.RLIMIT_AS, Cur: 2 * mem, Max: 2 * mem},
		{Resource: rlimitNproc, Cur: uint64(lim.TasksMax), Max: uint64(lim.TasksMax)},
		{Resource: syscall.RLIMIT_NOFILE, Cur: 1024, Max: 1024},
		{Resource: syscall.RLIMIT_CORE, Cur: 0, Max: 0},
	}, nil
}
