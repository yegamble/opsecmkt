package ramsecrets

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
)

func validateMountInfo(path string, data []byte, exact bool) error {
	selected := ""
	valid := false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) < 10 {
			continue
		}
		separator := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || separator+3 >= len(fields) {
			continue
		}
		mount := unescapeMountPath(fields[4])
		if mount != "/" && path != mount && !strings.HasPrefix(path, mount+string(filepath.Separator)) {
			continue
		}
		if len(mount) <= len(selected) {
			continue
		}
		selected = mount
		options := map[string]bool{}
		for _, option := range strings.Split(fields[5], ",") {
			options[option] = true
		}
		valid = fields[separator+1] == "tmpfs" && options["nosuid"] && options["nodev"] && options["noexec"]
	}
	if selected == "" || !valid || exact && selected != path {
		return errors.New("mount is not hardened tmpfs")
	}
	return nil
}
func unescapeMountPath(path string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(path)
}
