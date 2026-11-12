package trinetra

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const pluginCatalogUsage = "usage: trinetra plugin catalog [--refresh]"

func cmdPluginCatalog(args []string) int {
	refresh := false
	for _, a := range args {
		if a != "--refresh" {
			fmt.Fprintln(stderr, pluginCatalogUsage)
			return 2
		}
		refresh = true
	}
	var cat plugin.Catalog
	err := pluginWithClient(func(m plugin.Manager) error {
		c, err := m.Catalog(refresh)
		cat = c
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "trinetra plugin catalog: %v\n", err)
		return 1
	}
	printPluginCatalog(cat)
	return 0
}

func printPluginCatalog(cat plugin.Catalog) {
	if cat.Error == plugin.ErrNoRegistryKeys.Error() {
		fmt.Fprintln(stdout, cat.Error)
		return
	}
	switch {
	case cat.Fetched == 0 && cat.Expires == 0:
		fmt.Fprintln(stderr, "catalog is empty (never fetched); run trinetra plugin catalog --refresh")
	case cat.Stale:
		fmt.Fprintf(stderr, "catalog is stale (expired %s); run trinetra plugin catalog --refresh\n",
			time.Unix(cat.Expires, 0).UTC().Format("2006-01-02"))
	}
	if cat.Error != "" {
		fmt.Fprintf(stderr, "last refresh failed: %s\n", cat.Error)
	}
	if len(cat.Entries) == 0 {
		return
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLATEST\tINSTALLED\tPERMISSIONS\tDESCRIPTION")
	for _, e := range cat.Entries {
		installed := "-"
		if e.Installed != "" {
			installed = e.Installed
			if e.UpdateAvailable {
				installed += " (update)"
			}
		}
		perms := make([]string, len(e.Permissions))
		for i, p := range e.Permissions {
			perms[i] = string(p)
		}
		permStr := strings.Join(perms, ",")
		if permStr == "" {
			permStr = "-"
		}
		desc := e.Description
		if e.Revoked != "" {
			desc = "REVOKED: " + e.Revoked
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.ID, e.Latest, installed, permStr, desc)
	}
	tw.Flush()
}
