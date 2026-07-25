package serverwatch

import "testing"

type fakeFS struct {
	files map[string]string
	globs map[string][]string
}

func (f fakeFS) Read(p string) ([]byte, error) {
	v, ok := f.files[p]
	if !ok {
		return nil, errNotExist
	}
	return []byte(v), nil
}
func (f fakeFS) Glob(p string) ([]string, error) { return f.globs[p], nil }

func TestParseFailedUnits(t *testing.T) {
	s := "nginx.service loaded failed failed A high performance web server\n" +
		"cron.service  loaded failed failed Regular background program\n"
	u := parseFailedUnits(s)
	if len(u) != 2 || u[0] != "nginx.service" {
		t.Fatalf("units = %+v", u)
	}
}

func TestParseUnits(t *testing.T) {
	s := "nginx.service    loaded active   running A high performance web server\n" +
		"cron.service     loaded active   running Regular background program\n" +
		"foo.service      loaded active\n" + // malformed/short line: skip
		"\n" + // blank line: ignored
		"  \n" // whitespace-only line: ignored
	units := parseUnits(s)
	if len(units) != 2 {
		t.Fatalf("len(units) = %d, want 2: %+v", len(units), units)
	}
	got := units[0]
	want := UnitInfo{Name: "nginx.service", Load: "loaded", Active: "active", Sub: "running", Description: "A high performance web server"}
	if got != want {
		t.Fatalf("units[0] = %+v, want %+v", got, want)
	}
	if units[1].Name != "cron.service" || units[1].Description != "Regular background program" {
		t.Fatalf("units[1] = %+v", units[1])
	}
}

func TestDiscoverTempSingleID(t *testing.T) {
	// Two thermal zones present, but Discover must emit exactly ONE target with
	// the plain id "temp" so `monitor disable temp` lines up with buildChecks.
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		return nil, errNotExist // no docker/df/smartctl
	}}
	fs := fakeFS{
		files: map[string]string{},
		globs: map[string][]string{
			"/sys/class/thermal/thermal_zone*/temp": {
				"/sys/class/thermal/thermal_zone0/temp",
				"/sys/class/thermal/thermal_zone1/temp",
			},
		},
	}
	ts := Discover(x, fs)
	var temps []Target
	for _, tg := range ts {
		if tg.Kind == "temp" {
			temps = append(temps, tg)
		}
	}
	if len(temps) != 1 {
		t.Fatalf("want exactly 1 temp target, got %+v", temps)
	}
	if temps[0].ID != "temp" {
		t.Fatalf("temp id = %q, want %q", temps[0].ID, "temp")
	}
	if !temps[0].Available {
		t.Fatal("temp target should be Available")
	}
}

func TestParseSmartScan(t *testing.T) {
	s := "/dev/sda -d sat # /dev/sda [SAT], ATA device\n/dev/sdb -d sat # ...\n"
	d := parseSmartScan(s)
	if len(d) != 2 || d[0] != "/dev/sda" {
		t.Fatalf("devices = %+v", d)
	}
}
