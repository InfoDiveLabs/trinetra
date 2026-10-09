package web

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

const usersUsage = `usage: trinetra users list [--json]
       trinetra users invite --role admin|responder|viewer [--ttl 24h] [--json]
       trinetra users set-role <name> <role>
       trinetra users remove <name>`

// enrollURL builds the link an invitee opens. note is non-empty when the
// address is a guess from web.listen.
func enrollURL(cfg *config.Config, token string) (string, string) {
	if o := strings.TrimRight(cfg.Web.Origin, "/"); o != "" {
		return o + "/enroll?token=" + token, ""
	}
	return "http://" + cfg.Web.Listen + "/enroll?token=" + token,
		"web.origin is not set, so this link uses web.listen. Passkeys only work over HTTPS or on localhost."
}

// RunUsersCommand implements `trinetra users …` against the web plugin's
// stores. actor is recorded in the audit log.
func RunUsersCommand(args []string, stateDir string, cfg *config.Config, actor string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usersUsage)
		return 2
	}
	audit := func(action, key, old, new string) {
		_ = appendAudit(stateDir, AuditRecord{User: actor, Action: action, Key: key, Old: old, New: new})
	}
	switch args[0] {
	case "list":
		return usersList(args[1:], newUserStore(stateDir), stdout, stderr)
	case "invite":
		fs := flag.NewFlagSet("users invite", flag.ContinueOnError)
		fs.SetOutput(stderr)
		role := fs.String("role", "", "admin, responder or viewer")
		ttl := fs.Duration("ttl", 24*time.Hour, "how long the link stays valid")
		asJSON := fs.Bool("json", false, "print JSON")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		if !validRole(Role(*role)) || *ttl <= 0 {
			fmt.Fprintln(stderr, "users invite: --role must be admin, responder or viewer, and --ttl must be positive")
			return 2
		}
		tok := newTokenStore(stateDir).Issue(Role(*role), *ttl)
		if tok == "" {
			fmt.Fprintf(stderr, "users invite: could not save the invite (check permissions on %s)\n", stateDir)
			return 1
		}
		link, note := enrollURL(cfg, tok)
		expires := time.Now().Add(*ttl)
		audit("user.invite", *role, "", "ttl="+ttl.String())
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(map[string]any{"url": link, "role": *role, "expires": expires.Unix()})
			return 0
		}
		fmt.Fprintln(stdout, link)
		fmt.Fprintf(stdout, "Single use, %s role, expires %s.\n", *role, expires.Format("2006-01-02 15:04"))
		if note != "" {
			fmt.Fprintln(stdout, note)
		}
		return 0
	case "set-role", "remove":
		want := 2
		if args[0] == "set-role" {
			want = 3
		}
		if len(args) != want {
			fmt.Fprintln(stderr, usersUsage)
			return 2
		}
		store := newUserStore(stateDir)
		u, ok := store.ByName(args[1])
		if !ok {
			fmt.Fprintf(stderr, "no web user named %q\n", args[1])
			return 1
		}
		var err error
		if args[0] == "set-role" {
			r := Role(args[2])
			if !validRole(r) {
				fmt.Fprintln(stderr, "role must be admin, responder or viewer")
				return 2
			}
			if err = store.SetRoleUnlessLastAdmin(u.ID, r); err == nil {
				audit("user.role", u.ID, string(u.Role), string(r))
			}
		} else if err = store.RemoveUnlessLastAdmin(u.ID); err == nil {
			audit("user.remove", u.ID, fmt.Sprintf("name=%s role=%s", u.Name, u.Role), "")
		}
		if errors.Is(err, errLastAdmin) {
			fmt.Fprintln(stderr, "refusing: that would leave no admin (last admin)")
			return 1
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, usersUsage)
	return 2
}

func usersList(args []string, store *jsonUserStore, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("users list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if fs.Parse(args) != nil {
		return 2
	}
	users := store.List()
	if *asJSON {
		type row struct {
			Name     string `json:"name"`
			Role     Role   `json:"role"`
			Passkeys int    `json:"passkeys"`
			Created  int64  `json:"created"`
		}
		rows := make([]row, 0, len(users))
		for _, u := range users {
			rows = append(rows, row{u.Name, u.Role, len(u.Credentials), u.Created})
		}
		_ = json.NewEncoder(stdout).Encode(rows)
		return 0
	}
	if len(users) == 0 {
		fmt.Fprintln(stdout, "No web users yet. Create the first admin with: sudo trinetra users invite --role admin")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tPASSKEYS\tCREATED")
	for _, u := range users {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", u.Name, u.Role, len(u.Credentials), time.Unix(u.Created, 0).Format("2006-01-02"))
	}
	tw.Flush()
	return 0
}
