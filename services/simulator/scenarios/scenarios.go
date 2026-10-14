// Package scenarios holds the built-in scenario files, compiled into the binary so that
// `simulator run -scenario small` works in a container with no files mounted.
package scenarios

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.yaml
var files embed.FS

// Get returns the YAML of the built-in scenario called name (the file name without .yaml).
func Get(name string) ([]byte, bool) {
	data, err := files.ReadFile(name + ".yaml")
	return data, err == nil
}

// Names lists the built-in scenarios in alphabetical order.
func Names() []string {
	entries, _ := fs.ReadDir(files, ".") // the embedded directory always exists
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(names)
	return names
}
