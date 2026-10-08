package core

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrStatusPageOnChild is returned by every StatusPage method on a fleet
// child: the public status page is evaluated and served by the master (or a
// standalone host) only.
var ErrStatusPageOnChild = errors.New("status page runs on the fleet master, not on a child")

// ServiceState is a public service's status.
type ServiceState string

const (
	StateOperational ServiceState = "operational"
	StateDegraded    ServiceState = "degraded"
	StateOutage      ServiceState = "outage"
	StateMaintenance ServiceState = "maintenance"
)

// stateRank orders states for WorseState: outage > degraded > maintenance >
// operational > "" (no data).
func stateRank(s ServiceState) int {
	switch s {
	case StateOutage:
		return 4
	case StateDegraded:
		return 3
	case StateMaintenance:
		return 2
	case StateOperational:
		return 1
	}
	return 0
}

// WorseState returns the more severe of a and b.
func WorseState(a, b ServiceState) ServiceState {
	if stateRank(b) > stateRank(a) {
		return b
	}
	return a
}

// Target kinds a service can map to.
const (
	TargetHost      = "host"      // this host (standalone or the master itself)
	TargetNode      = "node"      // a fleet node by id (master only)
	TargetTag       = "tag"       // every fleet node carrying a tag (master only)
	TargetContainer = "container" // docker:<Value> on Node ("" = this host)
	TargetUnit      = "unit"      // service:<Value> on Node
	TargetMount     = "mount"     // disk:<Value> on Node
)

// StatusTarget is one internal thing a public service depends on. Never
// exposed publicly.
type StatusTarget struct {
	Kind  string `json:"kind"`
	Node  string `json:"node,omitempty"`
	Value string `json:"value,omitempty"`
}

// StatusService is a public, customer-facing service.
type StatusService struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Group       string         `json:"group,omitempty"`
	Order       int            `json:"order"`
	Targets     []StatusTarget `json:"targets"`
	HoldDownSec int            `json:"hold_down_sec"`
}

// DefaultHoldDownSec is what the web form and CLI pre-fill for a new service.
const DefaultHoldDownSec = 180

// Incident statuses.
const (
	IncidentInvestigating = "investigating"
	IncidentIdentified    = "identified"
	IncidentMonitoring    = "monitoring"
	IncidentResolved      = "resolved"
)

// Limits.
const (
	MaxUpdateBytes  = 4096
	MaxTitleRunes   = 200
	MaxNameRunes    = 60
	MaxDescRunes    = 200
	MaxHoldDownSec  = 3600
	SystemAuthor    = "system"
)

// UpdateEdit records a previous version of an edited update.
type UpdateEdit struct {
	TS      int64  `json:"ts"`
	Editor  string `json:"editor"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// IncidentUpdate is one posted update on an incident.
type IncidentUpdate struct {
	ID      string       `json:"id"`
	TS      int64        `json:"ts"`
	Status  string       `json:"status"`
	Message string       `json:"message"`
	Author  string       `json:"author"`
	Edits   []UpdateEdit `json:"edits,omitempty"`
}

// StatusIncident is a public incident with its internal bookkeeping.
type StatusIncident struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Impact      ServiceState     `json:"impact"`
	Services    []string         `json:"services"`
	Status      string           `json:"status"`
	Auto        bool             `json:"auto"`
	Opened      int64            `json:"opened"`
	Updated     int64            `json:"updated"`
	Resolved    int64            `json:"resolved,omitempty"`
	RecoveredAt int64            `json:"recovered_at,omitempty"` // internal: when every service recovered
	Updates     []IncidentUpdate `json:"updates"`
	Triggers    []string         `json:"triggers,omitempty"` // internal only, never public
}

// NewUpdate is the input for posting or editing an update.
type NewUpdate struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// NewIncident is the input for creating an incident by hand.
type NewIncident struct {
	Title    string       `json:"title"`
	Services []string     `json:"services"`
	Impact   ServiceState `json:"impact"`
	Update   NewUpdate    `json:"update"`
}

// ServiceEvaluation is the internal live view of one service (admin page).
type ServiceEvaluation struct {
	ServiceID    string       `json:"service_id"`
	State        ServiceState `json:"state"`
	Computed     ServiceState `json:"computed"`
	PendingSince int64        `json:"pending_since,omitempty"`
	Reason       string       `json:"reason,omitempty"`
	Missing      []string     `json:"missing,omitempty"` // targets not found, human-readable
}

// Public view: the ONLY shape that reaches anonymous output. It has no ids
// of nodes, no targets, no triggers, no authors.
type PublicService struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Group       string         `json:"group,omitempty"`
	State       ServiceState   `json:"state"`
	History     []PublicDay    `json:"history"` // oldest first, 90 entries
}

type PublicDay struct {
	Date  string       `json:"date"`            // YYYY-MM-DD (UTC)
	State ServiceState `json:"state,omitempty"` // "" = no data
}

type PublicUpdate struct {
	TS      int64  `json:"ts"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type PublicIncident struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Impact   ServiceState   `json:"impact"`
	Services []string       `json:"services"` // service NAMES
	Status   string         `json:"status"`
	Opened   int64          `json:"opened"`
	Updated  int64          `json:"updated"`
	Resolved int64          `json:"resolved,omitempty"`
	Updates  []PublicUpdate `json:"updates"` // newest first
}

// Overall banners.
const (
	OverallOperational  = "operational"
	OverallPartial      = "partial_outage"
	OverallMajor        = "major_outage"
	OverallMaintenance  = "maintenance"
)

type PublicStatus struct {
	Schema    int              `json:"schema"` // 1
	Title     string           `json:"title"`
	Overall   string           `json:"overall"`
	Generated int64            `json:"generated"`
	Services  []PublicService  `json:"services"`
	Active    []PublicIncident `json:"active"`
	Recent    []PublicIncident `json:"recent"` // resolved within 90 days, newest first
}

// StatusPageAPI is the status-page surface served over the control socket
// ("StatusPage.*"). Writes take the acting user's name for the record.
type StatusPageAPI interface {
	Services() ([]StatusService, error)
	SetService(s StatusService, actor string) (StatusService, error)
	DeleteService(id, actor string) error
	Evaluation() ([]ServiceEvaluation, error)
	Incidents(includeResolved bool) ([]StatusIncident, error)
	Incident(id string) (StatusIncident, error)
	CreateIncident(in NewIncident, actor string) (StatusIncident, error)
	PostUpdate(incidentID string, u NewUpdate, actor string) (StatusIncident, error)
	EditUpdate(incidentID, updateID string, u NewUpdate, actor string) (StatusIncident, error)
	EditIncident(id, title string, services []string, actor string) (StatusIncident, error)
	DeleteIncident(id, actor string) error
	Public() (PublicStatus, error)
}

// StatusPageProvider is implemented by the daemon's served API and by
// control.Client. Consumers type-assert to it.
type StatusPageProvider interface {
	StatusPage() StatusPageAPI
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func runeLen(s string) int { return len([]rune(s)) }

// ValidateStatusService checks a service definition.
func ValidateStatusService(s StatusService) error {
	if !slugRE.MatchString(s.ID) {
		return fmt.Errorf("service id %q: want 1-40 chars of a-z, 0-9, '-'", s.ID)
	}
	if n := runeLen(strings.TrimSpace(s.Name)); n == 0 || n > MaxNameRunes {
		return fmt.Errorf("service name must be 1-%d characters", MaxNameRunes)
	}
	if runeLen(s.Description) > MaxDescRunes {
		return fmt.Errorf("description must be at most %d characters", MaxDescRunes)
	}
	if runeLen(s.Group) > MaxNameRunes {
		return fmt.Errorf("group must be at most %d characters", MaxNameRunes)
	}
	if s.HoldDownSec < 0 || s.HoldDownSec > MaxHoldDownSec {
		return fmt.Errorf("hold-down must be 0-%d seconds", MaxHoldDownSec)
	}
	for _, t := range s.Targets {
		switch t.Kind {
		case TargetHost:
		case TargetNode:
			if t.Node == "" {
				return errors.New("node target needs a node")
			}
		case TargetTag, TargetContainer, TargetUnit, TargetMount:
			if strings.TrimSpace(t.Value) == "" {
				return fmt.Errorf("%s target needs a value", t.Kind)
			}
		default:
			return fmt.Errorf("unknown target kind %q", t.Kind)
		}
	}
	return nil
}

func validIncidentStatus(s string) bool {
	switch s {
	case IncidentInvestigating, IncidentIdentified, IncidentMonitoring, IncidentResolved:
		return true
	}
	return false
}

// ValidateUpdate checks an update's status and message.
func ValidateUpdate(u NewUpdate) error {
	if !validIncidentStatus(u.Status) {
		return fmt.Errorf("unknown status %q", u.Status)
	}
	if strings.TrimSpace(u.Message) == "" {
		return errors.New("message is required")
	}
	if len(u.Message) > MaxUpdateBytes {
		return fmt.Errorf("message must be at most %d bytes", MaxUpdateBytes)
	}
	return nil
}

// ValidateTitle checks an incident title.
func ValidateTitle(t string) error {
	if n := runeLen(strings.TrimSpace(t)); n == 0 || n > MaxTitleRunes {
		return fmt.Errorf("title must be 1-%d characters", MaxTitleRunes)
	}
	return nil
}

// ValidateNewIncident checks a manually created incident.
func ValidateNewIncident(in NewIncident) error {
	if err := ValidateTitle(in.Title); err != nil {
		return err
	}
	switch in.Impact {
	case StateDegraded, StateOutage, StateMaintenance:
	default:
		return fmt.Errorf("impact must be degraded, outage or maintenance")
	}
	return ValidateUpdate(in.Update)
}
