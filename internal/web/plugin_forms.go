package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type configField struct {
	Key         string
	Title       string
	Description string
	Kind        string
	Value       string
	Checked     bool
	Options     []string
	Min, Max    string
	Step        string
	Placeholder string
	Required    bool
	Err         string
}

func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, it := range x {
			parts = append(parts, fmt.Sprint(it))
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func fmtBound(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'f', -1, 64)
}

// buildConfigFields renders the schema properties of one section; overlay holds posted values that win over stored ones, and secrets are never populated.
func buildConfigFields(rc plugin.RedactedConfig, section plugin.Slot, overlay map[string]string, errs plugin.FieldErrors) []configField {
	if rc.Schema == nil {
		return nil
	}
	isSet := map[string]bool{}
	for _, k := range rc.SecretsSet {
		isSet[k] = true
	}
	required := map[string]bool{}
	for _, k := range rc.Schema.Required {
		required[k] = true
	}
	var out []configField
	for _, key := range rc.Schema.OrderedKeys() {
		p := rc.Schema.Properties[key]
		if p == nil || p.SectionOf() != section {
			continue
		}
		f := configField{Key: key, Title: p.Title, Description: p.Description, Required: required[key], Err: errs[key]}
		if f.Title == "" {
			f.Title = key
		}
		val := rawToString(rc.Values[key])
		if v, ok := overlay[key]; ok {
			val = v
		}
		switch {
		case p.Format == plugin.FormatSecret:
			f.Kind = "password"
			f.Placeholder = "not set"
			if isSet[key] {
				f.Placeholder = "set — leave blank to keep"
			}
		case p.Type == "boolean":
			f.Kind, f.Checked = "checkbox", val == "true"
		case len(p.Enum) > 0:
			f.Kind, f.Options, f.Value = "select", p.Enum, val
		case p.Type == "array":
			f.Kind, f.Value = "textarea", val
		case p.Type == "integer" || p.Type == "number":
			f.Kind, f.Value, f.Min, f.Max = "number", val, fmtBound(p.Minimum), fmtBound(p.Maximum)
			f.Step = "1"
			if p.Type == "number" {
				f.Step = "any"
			}
		default:
			f.Kind, f.Value = "text", val
		}
		out = append(out, f)
	}
	return out
}

func sectionSlot(name string) (plugin.Slot, bool) {
	switch name {
	case "settings":
		return plugin.SlotSettingsForm, true
	case "alerts":
		return plugin.SlotAlertRules, true
	}
	return "", false
}

func normalizeLines(s string) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// configRequestFromForm builds a SetConfigRequest from the keys of one section: blank secrets are omitted and an absent checkbox is false.
func configRequestFromForm(id string, rc plugin.RedactedConfig, section plugin.Slot, form url.Values) (plugin.SetConfigRequest, map[string]string) {
	req := plugin.SetConfigRequest{ID: id, Values: map[string]string{}}
	posted := map[string]string{}
	if rc.Schema == nil {
		return req, posted
	}
	for _, key := range rc.Schema.OrderedKeys() {
		p := rc.Schema.Properties[key]
		if p == nil || p.SectionOf() != section {
			continue
		}
		vals, submitted := form[key]
		v := ""
		if submitted && len(vals) > 0 {
			v = vals[0]
		}
		switch {
		case p.Format == plugin.FormatSecret:
			if v != "" {
				req.Values[key] = v
			}
		case p.Type == "boolean":
			b := "false"
			if v == "true" || v == "1" || v == "on" {
				b = "true"
			}
			req.Values[key], posted[key] = b, b
		case !submitted:
		case p.Type == "array":
			v = normalizeLines(v)
			req.Values[key], posted[key] = v, v
		case p.Type == "integer" || p.Type == "number" || p.Format == plugin.FormatDuration:
			v = strings.TrimSpace(v)
			posted[key] = v
			if v == "" {
				req.Unset = append(req.Unset, key)
			} else {
				req.Values[key] = v
			}
		default:
			req.Values[key], posted[key] = v, v
		}
	}
	return req, posted
}

func integrationConfigHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m := pluginManager(d)
		if m == nil {
			renderNotFound(w, r, d, "plugins are unavailable")
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		section, ok := sectionSlot(r.PostForm.Get("section"))
		if !ok {
			http.Error(w, "unknown section", http.StatusBadRequest)
			return
		}
		rc, err := m.Config(id)
		if err != nil {
			renderPluginErr(w, r, d, err)
			return
		}
		req, posted := configRequestFromForm(id, rc, section, r.PostForm)
		req.Actor, req.Via = auditUser(r), "web"
		if err := m.SetConfig(req); err != nil {
			var fe plugin.FieldErrors
			opts := integrationPageOpts{Overlay: posted, Section: section}
			status := http.StatusBadRequest
			if errors.As(err, &fe) {
				opts.Errs = fe
			} else {
				log.Printf("web: plugin %s config: %v", id, err)
				opts.Flash, opts.FlashErr = "The settings could not be saved. See the daemon log for details.", true
				status = http.StatusInternalServerError
			}
			renderIntegrationDetail(w, r, d, id, opts, status)
			return
		}
		keys := make([]string, 0, len(req.Values)+len(req.Unset))
		for k := range req.Values {
			keys = append(keys, k)
		}
		keys = append(keys, req.Unset...)
		sort.Strings(keys)
		logAudit(d, r, "plugin.config", id, "", strings.Join(keys, ","))
		redirectIntegrations(w, r, "/integrations/"+url.PathEscape(id), "saved")
	}
}
