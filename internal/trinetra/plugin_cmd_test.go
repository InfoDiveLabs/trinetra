package trinetra

import (
	"bytes"
	"strings"
	"testing"
)

func stubPluginHandlers(t *testing.T) *[]string {
	t.Helper()
	var ran []string
	pc, pp, pr := pluginCatalogHandler, pluginPushHandler, pluginRuntimeHandler
	t.Cleanup(func() { pluginCatalogHandler, pluginPushHandler, pluginRuntimeHandler = pc, pp, pr })
	pluginCatalogHandler = func(args []string) int { ran = append(ran, "catalog:"+strings.Join(args, ",")); return 0 }
	pluginPushHandler = func(args []string) int { ran = append(ran, "push:"+strings.Join(args, ",")); return 0 }
	pluginRuntimeHandler = func(sub string, args []string) int {
		ran = append(ran, "runtime:"+sub+":"+strings.Join(args, ","))
		return 0
	}
	return &ran
}

func TestCmdPluginRoutesSubcommands(t *testing.T) {
	ran := stubPluginHandlers(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"catalog", "--refresh"}, "catalog:--refresh"},
		{[]string{"push", "x", "--tag", "t"}, "push:x,--tag,t"},
		{[]string{"install", "demo"}, "runtime:install:demo"},
		{[]string{"enable", "demo"}, "runtime:enable:demo"},
		{[]string{"disable", "demo"}, "runtime:disable:demo"},
		{[]string{"remove", "demo"}, "runtime:remove:demo"},
		{[]string{"update", "--all"}, "runtime:update:--all"},
		{[]string{"set", "demo", "k=v"}, "runtime:set:demo,k=v"},
		{[]string{"list"}, "runtime:list:"},
		{[]string{"status", "demo"}, "runtime:status:demo"},
		{[]string{"dev", "./dir"}, "runtime:dev:./dir"},
	}
	for _, c := range cases {
		*ran = nil
		if code := cmdPlugin(c.args); code != 0 {
			t.Fatalf("%v exit %d", c.args, code)
		}
		if len(*ran) != 1 || (*ran)[0] != c.want {
			t.Fatalf("%v ran %v, want %s", c.args, *ran, c.want)
		}
	}
}

func TestCmdPluginUnknownPrintsUsage(t *testing.T) {
	ran := stubPluginHandlers(t)
	prev := stderr
	var buf bytes.Buffer
	stderr = &buf
	t.Cleanup(func() { stderr = prev })
	for _, args := range [][]string{{"bogus"}, {}} {
		buf.Reset()
		if code := cmdPlugin(args); code != 2 {
			t.Fatalf("%v exit %d, want 2", args, code)
		}
		if !strings.Contains(buf.String(), "trinetra plugin install") {
			t.Fatalf("usage not printed: %q", buf.String())
		}
	}
	if len(*ran) != 0 {
		t.Fatalf("handlers ran: %v", *ran)
	}
}

func TestPluginStubsReportNotImplemented(t *testing.T) {
	prev := stderr
	var buf bytes.Buffer
	stderr = &buf
	t.Cleanup(func() { stderr = prev })
	if code := cmdPluginCatalog(nil); code != 1 || !strings.Contains(buf.String(), "trinetra plugin catalog: not implemented yet") {
		t.Fatalf("catalog stub: %d %q", code, buf.String())
	}
	buf.Reset()
	if code := cmdPluginPush(nil); code != 1 || !strings.Contains(buf.String(), "trinetra plugin push: not implemented yet") {
		t.Fatalf("push stub: %d %q", code, buf.String())
	}
}

func TestMainRoutesPluginAndHiddenPluginExec(t *testing.T) {
	ran := stubPluginHandlers(t)
	if code := Main([]string{"plugin", "list"}); code != 0 || len(*ran) != 1 {
		t.Fatalf("plugin: %d %v", code, *ran)
	}
	prev := stderr
	var buf bytes.Buffer
	stderr = &buf
	t.Cleanup(func() { stderr = prev })
	if code := cmdPluginExec(nil); code != 2 {
		t.Fatalf("plugin-exec without args exit %d", code)
	}
	if strings.Contains(usage, "plugin-exec") {
		t.Fatal("plugin-exec must stay hidden from usage")
	}
}
