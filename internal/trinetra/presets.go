package trinetra

import (
	"fmt"
	"text/template"
)

// slackWebhookTemplate is the body template for the "slack" channel preset.
const slackWebhookTemplate = `{"text":{{if .Body}}{{printf "%s %s -- %s" .Marker .Title .Body | json}}{{else}}{{printf "%s %s" .Marker .Title | json}}{{end}}}`

// discordMaxContentLen is the number of runes discordWebhookTemplate keeps of the composed
// message before JSON-encoding it.
const discordMaxContentLen = 1900

// discordWebhookTemplate is the body template for the "discord" channel preset.
var discordWebhookTemplate = fmt.Sprintf(
	`{"content":{{if .Body}}{{printf "%%s %%s -- %%s" .Marker .Title .Body | truncate %d | json}}{{else}}{{printf "%%s %%s" .Marker .Title | truncate %d | json}}{{end}}}`,
	discordMaxContentLen, discordMaxContentLen,
)

// slackTmpl and discordTmpl are the parsed presets, built once at package init.
var (
	slackTmpl   = template.Must(parseWebhookTemplate(slackWebhookTemplate))
	discordTmpl = template.Must(parseWebhookTemplate(discordWebhookTemplate))
)
