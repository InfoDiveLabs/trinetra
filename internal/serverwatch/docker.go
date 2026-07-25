package serverwatch

import "strings"

type dockerAccess struct {
	method    string // "socket" | "group" | "sudo"
	sudo      bool
	available bool
}

const dockerPSFormat = "{{.Names}}\t{{.State}}\t{{.Status}}"

func probeDocker(x Exec, fs FileSource) dockerAccess {
	// Try a plain `docker ps` first (works as root or with group membership).
	if _, err := x.Run("docker", "ps", "-a", "--format", dockerPSFormat); err == nil {
		method := "socket"
		if _, e := fs.Read("/var/run/docker.sock"); e != nil {
			method = "group"
		}
		return dockerAccess{method: method, available: true}
	}
	// Fall back to sudo.
	if _, err := x.Run("sudo", "docker", "ps", "-a", "--format", dockerPSFormat); err == nil {
		return dockerAccess{method: "sudo", sudo: true, available: true}
	}
	return dockerAccess{available: false}
}

func (a dockerAccess) list(x Exec) ([]Container, error) {
	var out []byte
	var err error
	if a.sudo {
		out, err = x.Run("sudo", "docker", "ps", "-a", "--format", dockerPSFormat)
	} else {
		out, err = x.Run("docker", "ps", "-a", "--format", dockerPSFormat)
	}
	if err != nil {
		return nil, err
	}
	return parseDockerPS(string(out)), nil
}

type Container struct {
	Name     string
	State    string
	Restarts int
}

func parseDockerPS(s string) []Container {
	var cs []Container
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		cs = append(cs, Container{Name: f[0], State: f[1]})
	}
	return cs
}
