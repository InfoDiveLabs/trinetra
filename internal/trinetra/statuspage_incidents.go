package trinetra

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type incidentManager struct {
	data        *statusPageData
	now         func() time.Time
	autoResolve func() time.Duration
	echo        func(inc core.StatusIncident, u core.IncidentUpdate)
	newID       func() string
}

func randomStatusID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (m *incidentManager) serviceName(id string) string {
	for _, s := range m.data.Services {
		if s.ID == id {
			return s.Name
		}
	}
	return id
}

func (m *incidentManager) serviceExists(id string) bool {
	return slices.ContainsFunc(m.data.Services, func(s core.StatusService) bool { return s.ID == id })
}

func (m *incidentManager) names(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = m.serviceName(id)
	}
	return strings.Join(out, ", ")
}

func (m *incidentManager) find(id string) (int, bool) {
	for i, inc := range m.data.Incidents {
		if inc.ID == id {
			return i, true
		}
	}
	return -1, false
}

// openAutoFor returns the index of the open automatic incident covering svc.
func (m *incidentManager) openAutoFor(svc string) (int, bool) {
	for i, inc := range m.data.Incidents {
		if inc.Auto && inc.Status != core.IncidentResolved && slices.Contains(inc.Services, svc) {
			return i, true
		}
	}
	return -1, false
}

const maxTriggers = 20

// cloneStatusIncident deep-copies an incident so callers can use it after the
// manager's lock is released.
func cloneStatusIncident(inc core.StatusIncident) core.StatusIncident {
	out := inc
	out.Services = slices.Clone(inc.Services)
	out.Triggers = slices.Clone(inc.Triggers)
	if inc.Updates != nil {
		out.Updates = make([]core.IncidentUpdate, len(inc.Updates))
		for i, u := range inc.Updates {
			u.Edits = slices.Clone(u.Edits)
			out.Updates[i] = u
		}
	}
	return out
}

func addTrigger(inc *core.StatusIncident, t string) {
	inc.Triggers = append(inc.Triggers, t)
	if n := len(inc.Triggers) - maxTriggers; n > 0 {
		inc.Triggers = slices.Clone(inc.Triggers[n:])
	}
}

func (m *incidentManager) addUpdate(i int, status, msg, author string) {
	inc := &m.data.Incidents[i]
	now := m.now().Unix()
	wasResolved := inc.Status == core.IncidentResolved
	u := core.IncidentUpdate{ID: m.newID(), TS: now, Status: status, Message: msg, Author: author}
	inc.Updates = append(inc.Updates, u)
	inc.Status, inc.Updated = status, now
	if status == core.IncidentResolved {
		if !wasResolved || inc.Resolved == 0 {
			inc.Resolved = now
		}
	} else {
		inc.Resolved = 0
		if wasResolved { // reopened: auto-resolve must not fire on the stale recovery
			inc.RecoveredAt = 0
		}
	}
	if m.echo != nil {
		m.echo(cloneStatusIncident(*inc), u)
	}
}

func impactPhrase(s core.ServiceState, names string) string {
	if s == core.StateOutage {
		return "an outage affecting " + names
	}
	return "degraded performance of " + names
}

func impactTitle(s core.ServiceState, names string) string {
	if s == core.StateOutage {
		return "Outage: " + names
	}
	return "Degraded performance: " + names
}

func isProblem(s core.ServiceState) bool { return s == core.StateDegraded || s == core.StateOutage }

func (m *incidentManager) allRecovered(inc core.StatusIncident) bool {
	for _, id := range inc.Services {
		if st := m.data.State[id]; st != nil && isProblem(st.State) {
			return false
		}
	}
	return true
}

// markRecovered records that every service of incident i recovered and posts
// the monitoring update.
func (m *incidentManager) markRecovered(i int) {
	inc := &m.data.Incidents[i]
	inc.RecoveredAt = m.now().Unix()
	verb := "has"
	if len(inc.Services) > 1 {
		verb = "have"
	}
	m.addUpdate(i, core.IncidentMonitoring, fmt.Sprintf("%s %s recovered. We're monitoring.", m.names(inc.Services), verb), core.SystemAuthor)
}

// sweepRecovered recovers open automatic incidents whose services are no
// longer a problem without a change event having arrived (a service deleted
// or edited out from under the incident). Services missing from State count
// as recovered.
func (m *incidentManager) sweepRecovered() bool {
	changed := false
	for i, inc := range m.data.Incidents {
		if !inc.Auto || inc.Status == core.IncidentResolved || inc.RecoveredAt != 0 || !m.allRecovered(inc) {
			continue
		}
		// A person's latest word always stands (e.g. a reopened incident).
		if n := len(inc.Updates); n > 0 && inc.Updates[n-1].Author != core.SystemAuthor {
			continue
		}
		m.markRecovered(i)
		changed = true
	}
	return changed
}

func (m *incidentManager) applyChanges(changes []stateChange) bool {
	changed := false
	var fresh []stateChange
	for _, c := range changes {
		switch {
		case isProblem(c.To):
			i, ok := m.openAutoFor(c.ServiceID)
			if !ok {
				fresh = append(fresh, c)
				continue
			}
			inc := &m.data.Incidents[i]
			addTrigger(inc, c.ServiceID+": "+c.Reason)
			name := m.serviceName(c.ServiceID)
			switch {
			case inc.RecoveredAt != 0:
				inc.RecoveredAt = 0
				inc.Impact = core.WorseState(inc.Impact, c.To)
				m.addUpdate(i, core.IncidentInvestigating, fmt.Sprintf("%s is affected again. We're investigating.", name), core.SystemAuthor)
			case core.WorseState(inc.Impact, c.To) != inc.Impact:
				inc.Impact = c.To
				m.addUpdate(i, inc.Status, fmt.Sprintf("We're now seeing %s.", impactPhrase(c.To, name)), core.SystemAuthor)
			}
			changed = true
		case isProblem(c.From):
			i, ok := m.openAutoFor(c.ServiceID)
			if !ok || m.data.Incidents[i].RecoveredAt != 0 || !m.allRecovered(m.data.Incidents[i]) {
				continue
			}
			m.markRecovered(i)
			changed = true
		}
	}
	if len(fresh) > 0 {
		var ids, triggers []string
		impact := core.StateDegraded
		for _, c := range fresh {
			ids = append(ids, c.ServiceID)
			triggers = append(triggers, c.ServiceID+": "+c.Reason)
			impact = core.WorseState(impact, c.To)
		}
		if n := len(triggers) - maxTriggers; n > 0 {
			triggers = triggers[n:]
		}
		names := m.names(ids)
		now := m.now().Unix()
		m.data.Incidents = append(m.data.Incidents, core.StatusIncident{
			ID: m.newID(), Title: impactTitle(impact, names), Impact: impact, Services: ids,
			Auto: true, Opened: now, Triggers: triggers,
		})
		m.addUpdate(len(m.data.Incidents)-1, core.IncidentInvestigating, fmt.Sprintf("We're investigating %s.", impactPhrase(impact, names)), core.SystemAuthor)
		changed = true
	}
	return changed
}

func (m *incidentManager) tickAutoResolve() bool {
	after := m.autoResolve()
	if after <= 0 {
		return false
	}
	changed := false
	now := m.now()
	for i, inc := range m.data.Incidents {
		if inc.Status == core.IncidentResolved || inc.RecoveredAt == 0 || !m.allRecovered(inc) {
			continue
		}
		if now.Sub(time.Unix(inc.RecoveredAt, 0)) >= after {
			m.addUpdate(i, core.IncidentResolved, "This incident has been resolved.", core.SystemAuthor)
			changed = true
		}
	}
	return changed
}

func (m *incidentManager) checkServices(ids []string) error {
	for _, id := range ids {
		if !m.serviceExists(id) {
			return fmt.Errorf("no such service %q: %w", id, core.ErrNotFound)
		}
	}
	return nil
}

func (m *incidentManager) create(in core.NewIncident, actor string) (core.StatusIncident, error) {
	if err := core.ValidateNewIncident(in); err != nil {
		return core.StatusIncident{}, err
	}
	if err := m.checkServices(in.Services); err != nil {
		return core.StatusIncident{}, err
	}
	m.data.Incidents = append(m.data.Incidents, core.StatusIncident{
		ID: m.newID(), Title: strings.TrimSpace(in.Title), Impact: in.Impact,
		Services: slices.Clone(in.Services), Opened: m.now().Unix(),
	})
	i := len(m.data.Incidents) - 1
	m.addUpdate(i, in.Update.Status, in.Update.Message, actor)
	return cloneStatusIncident(m.data.Incidents[i]), nil
}

func (m *incidentManager) post(id string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	if err := core.ValidateUpdate(u); err != nil {
		return core.StatusIncident{}, err
	}
	i, ok := m.find(id)
	if !ok {
		return core.StatusIncident{}, fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	m.addUpdate(i, u.Status, u.Message, actor)
	return cloneStatusIncident(m.data.Incidents[i]), nil
}

func (m *incidentManager) editUpdate(id, updateID string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	if err := core.ValidateUpdate(u); err != nil {
		return core.StatusIncident{}, err
	}
	i, ok := m.find(id)
	if !ok {
		return core.StatusIncident{}, fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	inc := &m.data.Incidents[i]
	for j := range inc.Updates {
		up := &inc.Updates[j]
		if up.ID != updateID {
			continue
		}
		up.Edits = append(up.Edits, core.UpdateEdit{TS: m.now().Unix(), Editor: actor, Status: up.Status, Message: up.Message})
		up.Status, up.Message = u.Status, u.Message
		if j == len(inc.Updates)-1 { // editing the latest update changes the incident status
			inc.Status = u.Status
			if u.Status == core.IncidentResolved && inc.Resolved == 0 {
				inc.Resolved = m.now().Unix()
			} else if u.Status != core.IncidentResolved {
				if inc.Resolved != 0 {
					inc.RecoveredAt = 0
				}
				inc.Resolved = 0
			}
		}
		inc.Updated = m.now().Unix()
		return cloneStatusIncident(*inc), nil
	}
	return core.StatusIncident{}, fmt.Errorf("no such update %q: %w", updateID, core.ErrNotFound)
}

func (m *incidentManager) editIncident(id, title string, services []string, actor string) (core.StatusIncident, error) {
	if err := core.ValidateTitle(title); err != nil {
		return core.StatusIncident{}, err
	}
	if err := m.checkServices(services); err != nil {
		return core.StatusIncident{}, err
	}
	i, ok := m.find(id)
	if !ok {
		return core.StatusIncident{}, fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	inc := &m.data.Incidents[i]
	inc.Title = strings.TrimSpace(title)
	if len(services) > 0 {
		inc.Services = slices.Clone(services)
	}
	inc.Updated = m.now().Unix()
	_ = actor // recorded by the caller's audit log
	return cloneStatusIncident(*inc), nil
}

func (m *incidentManager) delete(id string) error {
	i, ok := m.find(id)
	if !ok {
		return fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	m.data.Incidents = slices.Delete(m.data.Incidents, i, i+1)
	return nil
}
