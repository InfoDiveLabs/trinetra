// Package trinetra: fleet_telegram.go is the master's side of a Telegram
// inline-button tap: daemon.go's pollLoop/processUpdates route a callback_query
// here instead of through handleCommand's text-command router. Solo and child
// never reach telegramCallbackAnswer's action branches for a real reason: their
// core.FleetAPI fails every write with core.ErrNotMaster, so a callback there
// is answered "unknown/expired" and otherwise ignored.
package trinetra

import (
	"fmt"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// telegramCallbackAnswer decides how the master responds to one inbound
// Telegram callback_query:
//
//   - a callback from any chat other than the configured, enrolled owner
//     (including "no owner enrolled yet", ownerChatID == "") is answered
//     "not authorized" and nothing is done. Authorization is per CHAT, not per
//     Telegram user id: every member of the enrolled group chat can tap the
//     buttons, as every member can run text commands (processUpdates'
//     `u.ChatID != c.Telegram.ChatID` gate); callback_query.from.id is
//     deliberately never checked;
//   - "ack:<incident id>" acknowledges that incident (AckIncident, actor
//     "telegram") and answers "acked"; AckIncident refuses an already-resolved
//     incident (incidentStore.Ack), surfaced as "unknown/expired" like any
//     other fleetAPI error;
//   - "sil1h:<incident id>" creates a 1h silence matching that incident's
//     still-open members (node + rule) and answers "silenced 1h"; a resolved
//     incident, or one with no open members left, is answered
//     "unknown/expired" and creates no silence (see silenceIncidentFor1h);
//   - anything else (unknown data, a vanished incident, a fleetAPI action that
//     errors) is answered "unknown/expired", with nothing done.
//
// The caller (pollLoop) must ALWAYS call AnswerCallbackQuery with the text this
// returns: Telegram requires an answer for every callback_query, authorized or
// not.
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

// silenceIncidentFor1h creates a 1h silence (author "telegram", comment "from
// Telegram") matching incidentID's still-OPEN members by (node, rule).
// Matchers is ORed (core.Silence), so one silence covers every open member.
//
// A resolved incident (or one whose members all resolved) must create no
// silence: there is nothing firing to suppress, and a stale button on an old
// message would otherwise create a real, operator-visible 1h silence for no
// reason. Two guards enforce this: the incident's own State is checked
// (mirroring AckIncident's "already resolved" refusal), AND each member is
// checked individually (al.ResolvedAt == 0), so a recovered member is dropped
// from the matcher list. Both are kept because an incident's State can lag its
// members' ResolvedAt, or vice versa, depending on when recomputeState last
// ran.
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
