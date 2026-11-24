package main

import (
	"bytes"
	"strings"
)

// distroListed reports whether `wsl.exe -l --running -q` output names the
// distro. wsl.exe prints UTF-16, so drop the NULs (and BOM) first.
func distroListed(out []byte, distro string) bool {
	out = bytes.ReplaceAll(out, []byte{0}, nil)
	out = bytes.TrimPrefix(out, []byte("\xff\xfe"))
	for _, line := range strings.Split(string(out), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), distro) {
			return true
		}
	}
	return false
}
