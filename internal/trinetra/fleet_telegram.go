// Package trinetra: fleet_telegram.go is the master's side of a Telegram inline-button tap.
package trinetra

import (
	"fmt"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// telegramCallbackAnswer decides how the master responds to one inbound Telegram
// callback_query:
func telegramCallbackAnswer(fleetAPI core.FleetAPI, ownerChatID string, u telegram.Update, now func() time.Time) string {
	if ownerChatID == "" || u.CallbackChat != ownerChatID {
		return "not authorized"
	}
	if fleetAPI == nil {
		return "unknown/expired"
	}
	switch {
	case strings.HasPrefix(u.CallbackData, "ack:"):
		id := strings.TrimPrefix(u.CallbackData, "ack:")
		if id == "" {
			return "unknown/expired"
		}
		if err := fleetAPI.AckIncident(id, "telegram"); err != nil {
			return "unknown/expired"
		}
		return "acked"
	case strings.HasPrefix(u.CallbackData, "sil1h:"):
		id := strings.TrimPrefix(u.CallbackData, "sil1h:")
		if id == "" {
			return "unknown/expired"
		}
		if err := silenceIncidentFor1h(fleetAPI, id, now); err != nil {
			return "unknown/expired"
		}
		return "silenced 1h"
	default:
		return "unknown/expired"
	}
}

// silenceIncidentFor1h creates a 1h silence (author "telegram", comment "from Telegram")
// matching incidentID's still-OPEN members by (node, rule).
func silenceIncidentFor1h(fleetAPI core.FleetAPI, incidentID string, now func() time.Time) error {
	inc, err := fleetAPI.Incident(incidentID)
	if err != nil {
		return err
	}
	if inc.State == "resolved" {
		return fmt.Errorf("incident %q is already resolved", incidentID)
	}
	matchers := make([]core.Matcher, 0, len(inc.Alerts))
	for _, al := range inc.Alerts {
		if al.ResolvedAt != 0 {
			continue
		}
		matchers = append(matchers, core.Matcher{Node: al.Node, Rule: al.Key})
	}
	if len(matchers) == 0 {
		return fmt.Errorf("incident %q has no open members to silence", incidentID)
	}
	n := now()
	_, err = fleetAPI.CreateSilence(core.Silence{
		Matchers: matchers,
		Start:    n.Unix(),
		End:      n.Add(time.Hour).Unix(),
		Author:   "telegram",
		Comment:  "from Telegram",
	})
	return err
}
