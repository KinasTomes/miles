// Package buildinfo exposes version information shared by Miles binaries.
package buildinfo

import (
	"fmt"
	"runtime"
)

// Version is set at build time with -ldflags; local builds default to dev.
var Version = "dev"

// String returns the binary name, project version and Go toolchain version.
func String(binary string) string {
	return fmt.Sprintf("%s %s (%s)", binary, Version, runtime.Version())
}
