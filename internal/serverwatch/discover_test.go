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

func TestParseSmartScan(t *testing.T) {
	s := "/dev/sda -d sat # /dev/sda [SAT], ATA device\n/dev/sdb -d sat # ...\n"
	d := parseSmartScan(s)
	if len(d) != 2 || d[0] != "/dev/sda" {
		t.Fatalf("devices = %+v", d)
	}
}
