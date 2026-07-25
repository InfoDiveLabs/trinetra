package serverwatch

import (
	"fmt"
	"text/template"
)

// slackWebhookTemplate is the body template for the "slack" channel preset.
// Slack incoming webhooks expect a JSON object with a "text" field. The
// composed message (emoji Marker + Title, plus " — " + Body when Body is
// non-empty) mirrors formatAlert's (notify.go) plain-text style, safely
// JSON-encoded via the "json" func exactly as defaultWebhookTemplate does —
// it just swaps in the emoji Marker for the textual Severity so users get
// the same 🚨/⚠️/ℹ️/✅ markers they'd see on Telegram.
const slackWebhookTemplate = `{"text":{{if .Body}}{{printf "%s %s — %s" .Marker .Title .Body | json}}{{else}}{{printf "%s %s" .Marker .Title | json}}{{end}}}`

// discordMaxContentLen is the number of runes discordWebhookTemplate keeps
// of the composed message before JSON-encoding it. Discord rejects webhook
// payloads whose "content" exceeds 2000 characters; truncating to 1900
// leaves headroom for the severity marker and any characters Discord's own
// limit check counts differently than Go's rune count.
const discordMaxContentLen = 1900

// discordWebhookTemplate is the body template for the "discord" channel
// preset. Discord incoming webhooks expect a JSON object with a "content"
// field. It mirrors slackWebhookTemplate's message composition (emoji
// Marker + Title, plus " — " + Body when present) but pipes the composed
// string through "truncate" before "json" to respect discordMaxContentLen.
var discordWebhookTemplate = fmt.Sprintf(
	`{"content":{{if .Body}}{{printf "%%s %%s — %%s" .Marker .Title .Body | truncate %d | json}}{{else}}{{printf "%%s %%s" .Marker .Title | truncate %d | json}}{{end}}}`,
	discordMaxContentLen, discordMaxContentLen,
)

// slackTmpl and discordTmpl are the parsed presets, built once at package
// init. Both template strings above are fixed and known-valid, so a parse
// failure here would be a programming error caught immediately by any test
// run (or `go build` of a package that references them), not something that
// needs runtime error handling in buildNotifier.
var (
	slackTmpl   = template.Must(parseWebhookTemplate(slackWebhookTemplate))
	discordTmpl = template.Must(parseWebhookTemplate(discordWebhookTemplate))
)
