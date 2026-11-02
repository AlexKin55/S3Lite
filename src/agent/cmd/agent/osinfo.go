package main

import (
	"os"
	"runtime"
	"strings"
)

// osVersion returns a human-readable OS name and version, e.g.
// "Ubuntu 22.04.4 LTS" or "ALT Server 10.2". It reads PRETTY_NAME from
// /etc/os-release on Linux; on other systems (or when the file is missing)
// it falls back to the Go runtime OS string.
func osVersion() string {
	if v := prettyName(); v != "" {
		return v
	}
	return runtime.GOOS
}

// prettyName extracts PRETTY_NAME=... from /etc/os-release.
func prettyName() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			v = strings.TrimSpace(v)
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
