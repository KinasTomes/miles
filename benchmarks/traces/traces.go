// Package traces provides embedded benchmark traces for cost simulation and modeled reporting.
package traces

import _ "embed"

//go:embed cloud-boundary.json
var CloudBoundary []byte

//go:embed edge-revival.json
var EdgeRevival []byte

// Get returns the raw trace bytes for the specified workload or file name.
func Get(name string) []byte {
	switch name {
	case "cloud-boundary.json", "cloud-boundary", "cloud-tiering-boundary":
		return CloudBoundary
	case "edge-revival.json", "edge-revival", "edge-revival-accounting":
		return EdgeRevival
	default:
		return nil
	}
}
