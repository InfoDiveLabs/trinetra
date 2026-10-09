// Package trinetra: fleet_telegram.go is the master's side of a Telegram
// inline-button tap: daemon.go's pollLoop/processUpdates route a
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
// Telegram callback_query:
//
//   - a callback from any chat other than the configured, enrolled owner
//     (including "no owner enrolled yet", ownerChatID == "") is answered
//     "not authorized" and nothing is done. Authorization is per CHAT, not
//     per Telegram user id: every member of the enrolled group chat can tap
//     the buttons, exactly like every member can already run text commands
//     there (processUpdates' existing `u.ChatID != c.Telegram.ChatID` gate)
//     -- callback_query.from.id is deliberately never checked here;
//   - "ack:<incident id>" acknowledges that incident (AckIncident, actor
//     "telegram") and answers "acked" -- AckIncident itself refuses an
//     already-resolved incident (incidentStore.Ack), which this surfaces as
//     "unknown/expired" like any other fleetAPI error;
//   - "sil1h:<incident id>" creates a 1h silence matching that incident's
//     still-open members (node + rule) and answers "silenced 1h"; a
//     resolved incident, or one with no open members left, is answered
//     "unknown/expired" and creates no silence (round-1 review fix -- see
//     silenceIncidentFor1h);
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
// "telegram", comment "from Telegram") matching incidentID's still-OPEN
// members by (node, rule) -- Matchers is ORed (core.Silence's doc comment),
// so this covers every open member with a single silence exactly as if an
// operator had silenced each of them individually.
//
// Round-1 review fix: a resolved incident (or one every member of which has
// already resolved) must create no silence at all -- there is nothing left
// firing for it to suppress, and doing so anyway would be a real,
// operator-visible 1h silence created for no reason by a stale button on an
// old message. Two guards enforce this: the incident's own State is
// checked directly (mirroring AckIncident's/incidentStore.Ack's own
// "already resolved" refusal), AND each member is checked individually
// (al.ResolvedAt == 0) rather than silencing every member wholesale like
// AckIncident's push does -- a member that already recovered needs nothing
// more suppressed, so it's dropped from the matcher list rather than
// included pointlessly. Either guard alone would already prevent the
// reported bug; both are kept because they cover slightly different cases
// (an incident's State can lag an instant behind its members' ResolvedAt,
// or vice versa, depending on exactly when recomputeState last ran).
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
