package plugin

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	HostAPIMajor = 1
	HostAPIMinor = 0
)

// ParseHostAPI parses "^MAJOR.MINOR" or "MAJOR.MINOR".
func ParseHostAPI(rng string) (major, minor int, err error) {
	s := strings.TrimPrefix(rng, "^")
	a, b, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, fmt.Errorf("host_api %q: want MAJOR.MINOR", rng)
	}
	major, err1 := strconv.Atoi(a)
	minor, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || major < 0 || minor < 0 {
		return 0, 0, fmt.Errorf("host_api %q: want MAJOR.MINOR", rng)
	}
	return major, minor, nil
}

// HostAPICompatible returns nil when rng is served by this core.
func HostAPICompatible(rng string) error {
	major, minor, err := ParseHostAPI(rng)
	if err != nil {
		return err
	}
	switch {
	case major == HostAPIMajor && minor <= HostAPIMinor:
		return nil
	case HostAPIMajor > 1 && major == HostAPIMajor-1:
		return nil
	}
	return fmt.Errorf("%w: plugin wants %s, this core serves %s", ErrIncompatible, rng, HostAPIString())
}

// HostAPIString is "1.0".
func HostAPIString() string { return fmt.Sprintf("%d.%d", HostAPIMajor, HostAPIMinor) }
