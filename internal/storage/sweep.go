package storage

// SweepVersion identifies one immutable pack or sidecar object version.
type SweepVersion struct {
	Category  string
	VersionID string
}

func ValidSweepVersions(versions []SweepVersion) bool {
	if len(versions) == 0 || len(versions) > 64 {
		return false
	}
	seen := map[SweepVersion]bool{}
	for _, v := range versions {
		if v.Category != "pack" && v.Category != "sidecar" || v.VersionID == "" || v.VersionID == "null" || len(v.VersionID) > 1024 || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
