package plugin

import (
	"fmt"
	"sort"
)

// Permission is one grantable plugin ability.
type Permission string

const (
	PermMetricsRead    Permission = "metrics.read"
	PermMetricsPublish Permission = "metrics.publish"
	PermAlertsRead     Permission = "alerts.read"
	PermAlertsRaise    Permission = "alerts.raise"
	PermDockerRead     Permission = "docker.read"
	PermLogsRead       Permission = "logs.read"
	PermFleetRead      Permission = "fleet.read"
	PermNetOutbound    Permission = "net.outbound"
	PermNetListen      Permission = "net.listen"
	PermDockerControl  Permission = "docker.control"
	PermExec           Permission = "exec"
)

// V1Permissions is the grantable vocabulary of host_api 1.
var V1Permissions = []Permission{PermMetricsRead, PermMetricsPublish, PermAlertsRead, PermAlertsRaise,
	PermDockerRead, PermLogsRead, PermFleetRead, PermNetOutbound, PermNetListen}

// ReservedPermissions are names held for wave 2 and refused in v1 manifests.
var ReservedPermissions = []Permission{PermDockerControl, PermExec}

// Known reports whether p is in V1Permissions.
func (p Permission) Known() bool { return containsPerm(V1Permissions, p) }

// Reserved reports whether p is in ReservedPermissions.
func (p Permission) Reserved() bool { return containsPerm(ReservedPermissions, p) }

func containsPerm(list []Permission, p Permission) bool {
	for _, q := range list {
		if q == p {
			return true
		}
	}
	return false
}

var permDescriptions = map[Permission]string{
	PermMetricsRead:    "can read this host's metrics and history",
	PermMetricsPublish: "can add its own metrics to Trinetra's charts and history",
	PermAlertsRead:     "can see active and past alerts",
	PermAlertsRaise:    "can raise and resolve its own alerts through your notification channels",
	PermDockerRead:     "can see your Docker containers and their resource use",
	PermLogsRead:       "can read container logs, which may contain secrets",
	PermFleetRead:      "can see the fleet's nodes and their status",
	PermNetOutbound:    "can make outbound network connections",
	PermNetListen:      "can open a network port on this host",
}

// Describe returns the plain-language consent line for p.
func Describe(p Permission) string {
	if d, ok := permDescriptions[p]; ok {
		return d
	}
	return "unknown permission"
}

// Missing returns requested minus granted, deduplicated and sorted.
func Missing(granted, requested []Permission) []Permission {
	have := map[Permission]bool{}
	for _, p := range granted {
		have[p] = true
	}
	var out []Permission
	for _, p := range requested {
		if !have[p] {
			have[p] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RiskWarning is the unverified-plugin warning shown by the CLI and web.
func RiskWarning(name, version, publisher string) string {
	return fmt.Sprintf("WARNING: %s %s is an UNVERIFIED community plugin.\n"+
		"It was not reviewed or signed by the Trinetra registry. It runs on this host\n"+
		"with the permissions you grant below. Only continue if you trust its publisher\n"+
		"(%s).", name, version, publisher)
}
