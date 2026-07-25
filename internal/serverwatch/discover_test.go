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
