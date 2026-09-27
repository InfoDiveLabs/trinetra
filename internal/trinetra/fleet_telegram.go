// Package trinetra: fleet_telegram.go is the master's side of a Telegram
// inline-button tap (task 9): daemon.go's pollLoop/processUpdates route a
// callback_query here instead of through handleCommand's text-command
// router. Solo and child never reach telegramCallbackAnswer's action
// branches for a real reason to act on -- their core.FleetAPI (via
// fleetProvider.Fleet) fails every write with core.ErrNotMaster, so a
// callback there is answered "unknown/expired" and otherwise ignored,
// matching the ruling that solo/child behaviour stays unchanged beyond
// always answering.
package trinetra

import (
	"fmt"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// telegramCallbackAnswer decides how the master responds to one inbound
// Telegram callback_query (task 9 ruling):
//
//   - a callback from any chat other than the configured, enrolled owner
//     (including "no owner enrolled yet", ownerChatID == "") is answered
//     "not authorized" and nothing is done;
//   - "ack:<incident id>" acknowledges that incident (AckIncident, actor
//     "telegram") and answers "acked";
//   - "sil1h:<incident id>" creates a 1h silence matching that incident's
//     members (node + rule) and answers "silenced 1h";
//   - anything else -- unknown data, an incident that no longer exists, a
//     fleetAPI action that errors for any other reason -- is answered
//     "unknown/expired", also with nothing done.
//
// The caller (pollLoop) is responsible for ALWAYS calling
// AnswerCallbackQuery with the text this returns: Telegram requires an
// answer for every callback_query it delivers, authorized or not.
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

// silenceIncidentFor1h creates a 1h silence (task-9 ruling: author
// "telegram", comment "from Telegram") matching every one of incidentID's
// members by (node, rule) -- Matchers is ORed (core.Silence's doc comment),
// so this covers every member with a single silence exactly as if an
// operator had silenced each of them individually.
func silenceIncidentFor1h(fleetAPI core.FleetAPI, incidentID string, now func() time.Time) error {
	inc, err := fleetAPI.Incident(incidentID)
	if err != nil {
		return err
	}
	matchers := make([]core.Matcher, 0, len(inc.Alerts))
	for _, al := range inc.Alerts {
		matchers = append(matchers, core.Matcher{Node: al.Node, Rule: al.Key})
	}
	if len(matchers) == 0 {
		return fmt.Errorf("incident %q has no members to silence", incidentID)
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
