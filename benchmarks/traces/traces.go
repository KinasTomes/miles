// Package traces provides benchmark trace access delegating to the internal simulator workloads package.
package traces

import "github.com/KinasTomes/miles/internal/simulator/workloads"

// Get returns the raw trace bytes for the specified workload or file name.
func Get(name string) []byte {
	return workloads.Get(name)
}
