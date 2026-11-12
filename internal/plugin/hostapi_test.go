package plugin

import (
	"reflect"
	"sort"
	"testing"
)

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestAllowedMethods(t *testing.T) {
	base := []string{MethodGetConfig, MethodHello, MethodLog, MethodReportStatus, MethodWatchConfig}
	sort.Strings(base)
	if got := keys(AllowedMethods(nil)); !reflect.DeepEqual(got, base) {
		t.Fatalf("nil: %v", got)
	}
	got := AllowedMethods([]Permission{PermMetricsRead})
	if !got[MethodSnapshot] || !got[MethodSeries] || got[MethodContainers] || len(got) != len(base)+2 {
		t.Fatalf("metrics.read: %v", keys(got))
	}
	got = AllowedMethods([]Permission{PermDockerRead})
	if !got[MethodContainers] || got[MethodContainerLogs] {
		t.Fatalf("docker.read: %v", keys(got))
	}
	got = AllowedMethods([]Permission{PermDockerRead, PermLogsRead})
	if !got[MethodContainerLogs] {
		t.Fatal("docker.read+logs.read should allow ContainerLogs")
	}
}

func TestEveryHostMethodHasPermissionEntry(t *testing.T) {
	consts := []string{MethodHello, MethodGetConfig, MethodWatchConfig, MethodReportStatus, MethodLog, MethodPublishSeries,
		MethodRaiseAlert, MethodResolveAlert, MethodSnapshot, MethodSeries, MethodActiveAlerts, MethodContainers,
		MethodContainerLogs, MethodFleetNodes}
	if len(consts) != 14 || len(MethodPermissions) != 14 {
		t.Fatalf("consts=%d map=%d", len(consts), len(MethodPermissions))
	}
	for _, c := range consts {
		if _, ok := MethodPermissions[c]; !ok {
			t.Fatalf("%s missing", c)
		}
	}
}

func TestHostRenderedSlots(t *testing.T) {
	for _, s := range V1Slots {
		want := s == SlotSettingsForm || s == SlotAlertRules
		if s.HostRendered() != want {
			t.Fatalf("%s HostRendered=%v", s, s.HostRendered())
		}
	}
}

func TestFieldErrorsMessageSorted(t *testing.T) {
	e := FieldErrors{"b": "bad", "a": "worse"}
	if e.Error() != "invalid config: a: worse; b: bad" {
		t.Fatal(e.Error())
	}
}
