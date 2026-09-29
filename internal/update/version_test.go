// internal/update/version_test.go
package update

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.5.0", "0.4.1", 1},
		{"v0.5.0", "0.5.0", 0},
		{"0.5.0-beta.2", "0.5.0", -1},
		{"0.5.0-beta.10", "0.5.0-beta.2", 1},
		{"0.5.0-beta.2", "0.5.0-alpha.9", 1},
		{"1.0.0", "0.99.99", 1},
	}
	for _, c := range cases {
		a, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.a, err)
		}
		b, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.b, err)
		}
		if got := CompareVersions(a, b); got != c.want {
			t.Errorf("Compare(%s,%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseVersionRejects(t *testing.T) {
	for _, s := range []string{"", "dev", "1.2", "1.2.3.4", "01.2.3", "1.2.3-", "1.2.3+meta", "v", "1.2.x"} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) accepted", s)
		}
	}
}
