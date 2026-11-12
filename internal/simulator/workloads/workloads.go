// Package workloads provides embedded neutral workload traces for simulator evaluations and modeled cost reporting.
package workloads

import _ "embed"

//go:embed cloud-boundary.json
var CloudBoundary []byte

//go:embed edge-revival.json
var EdgeRevival []byte

// Get returns the embedded workload trace bytes for the specified file name or workload identifier.
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
