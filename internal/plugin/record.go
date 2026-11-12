package plugin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Trust string

const (
	TrustVerified   Trust = "verified"
	TrustUnverified Trust = "unverified"
	TrustFirstParty Trust = "first-party"
	TrustDev        Trust = "dev"
)

type State string

const (
	StateInstalled    State = "installed"
	StateEnabled      State = "enabled"
	StateFailed       State = "failed"
	StateRevoked      State = "revoked"
	StateIncompatible State = "incompatible"
)

const (
	RecordFileName = "record.json"
	ConfigFileName = "config.json"
	TokenFileName  = "token"
	AuditFileName  = "audit.jsonl"
)

type ConsentRecord struct {
	Time        int64        `json:"time"`
	Actor       string       `json:"actor"`
	Via         string       `json:"via"` // cli | web | fleet:<masterNodeID>
	Version     string       `json:"version"`
	Permissions []Permission `json:"permissions"`
	Unverified  bool         `json:"unverified,omitempty"`
	Nodes       []string     `json:"nodes,omitempty"`
}

type PendingUpdate struct {
	Version        string       `json:"version"`
	Manifest       Manifest     `json:"manifest"`
	PackageSHA256  string       `json:"package_sha256"`
	Trust          Trust        `json:"trust"`
	NewPermissions []Permission `json:"new_permissions"`
}

type Installed struct {
	ID            string          `json:"id"`
	Version       string          `json:"version"`
	Trust         Trust           `json:"trust"`
	Manifest      Manifest        `json:"manifest"`
	State         State           `json:"state"`
	Reason        string          `json:"reason,omitempty"`
	Granted       []Permission    `json:"granted,omitempty"`
	Consents      []ConsentRecord `json:"consents,omitempty"`
	Source        string          `json:"source"` // registry | file:<path> | url:<url> | fleet:<masterNodeID> | dev:<dir>
	PackageSHA256 string          `json:"package_sha256"`
	InstalledAt   int64           `json:"installed_at"`
	UpdatedAt     int64           `json:"updated_at"`
	Pending       *PendingUpdate  `json:"pending,omitempty"`
}

type Health struct {
	Status         StatusReport `json:"status"`
	ReportedAt     int64        `json:"reported_at,omitempty"`
	Connected      bool         `json:"connected"`
	SeriesCount    int          `json:"series_count"`
	SeriesRejected int64        `json:"series_rejected"`
	LogsDropped    int64        `json:"logs_dropped"`
	EventsDropped  int64        `json:"events_dropped"`
	RecentLogs     []LogEntry   `json:"recent_logs,omitempty"`
}

type Status struct {
	Installed
	Running         bool   `json:"running"`
	Restarts        int    `json:"restarts"`
	Health          Health `json:"health"`
	UpdateAvailable string `json:"update_available,omitempty"`
	RevokedReason   string `json:"revoked_reason,omitempty"`
}

type AuditEntry struct {
	TS      int64  `json:"ts"`
	Actor   string `json:"actor"`
	Via     string `json:"via"`
	Action  string `json:"action"` // plugin.install|enable|consent|disable|remove|update|config|revoked|failed|push
	Plugin  string `json:"plugin"`
	Version string `json:"version,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// FieldErrors maps config keys to validation messages.
type FieldErrors map[string]string

func (e FieldErrors) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %s", k, e[k])
	}
	return "invalid config: " + strings.Join(parts, "; ")
}

type RedactedConfig struct {
	Schema     *ConfigSchema              `json:"schema,omitempty"`
	Values     map[string]json.RawMessage `json:"values"`
	SecretsSet []string                   `json:"secrets_set,omitempty"`
	Revision   int64                      `json:"revision"`
}
