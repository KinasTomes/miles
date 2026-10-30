package s3

import "testing"

// quota.json is the quota contract of API v2 2.6. No handler serves it yet
// (W03), so this test runs every fixture input through a reference function
// written from that section and compares the results. Handlers must pass
// the same fixture; their tests then replace the function. The S3 rows of
// authorization.json run through the Router in authz_test.go, its Admin
// rows through the Admin server in internal/api/admin, multipart.json
// through internal/multipart.

type ledgerResult struct {
	R             int64
	Reserved      int64
	Delta         int64
	Used          int64
	Staged        int64
	ActiveUploads int64 `json:"active_uploads"`
	Error         ErrorCode
}

type ledgerStep struct {
	Name          string
	Step          string
	Used          int64
	Reserved      int64
	Limit         int64
	R             int64
	Base          int64
	Size          int64
	Head          int64
	Received      int64
	Staged        int64
	ActiveUploads int64 `json:"active_uploads"`
	Released      bool
	Expect        ledgerResult
}

// apply is one owner ledger step of API v2 2.6 and architecture 9.2. It
// returns only the counters the step changes.
func (s ledgerStep) apply() (ledgerResult, bool) {
	const (
		growStep         = 64 << 20
		stagingHeadroom  = 100 << 30
		maxActiveUploads = 16
	)
	switch s.Step {
	case "reserve", "grow":
		r := max(s.R, s.Size-s.Base) // a reservation only grows here
		if s.Step == "grow" {
			for r = s.R; s.Base+r < s.Received; r += growStep {
			}
		}
		if s.Used+s.Reserved+r-s.R > s.Limit {
			return ledgerResult{R: s.R, Reserved: s.Reserved, Error: ErrQuotaExceeded}, true
		}
		return ledgerResult{R: r, Reserved: s.Reserved + r - s.R}, true
	case "finish":
		r := max(0, s.Size-s.Base)
		return ledgerResult{R: r, Reserved: s.Reserved - (s.R - r)}, true
	case "commit":
		d := s.Size - s.Head
		if s.Used+d+s.Reserved-s.R > s.Limit {
			return ledgerResult{Delta: d, Used: s.Used, Reserved: s.Reserved, Error: ErrQuotaExceeded}, true
		}
		return ledgerResult{Delta: d, Used: s.Used + d, Reserved: s.Reserved - s.R}, true
	case "release":
		if s.Released {
			return ledgerResult{Reserved: s.Reserved}, true
		}
		return ledgerResult{Reserved: s.Reserved - s.R}, true
	case "create_upload":
		if s.ActiveUploads >= maxActiveUploads {
			return ledgerResult{ActiveUploads: s.ActiveUploads, Error: ErrQuotaExceeded}, true
		}
		return ledgerResult{ActiveUploads: s.ActiveUploads + 1}, true
	case "part_admit":
		if s.Staged+s.Size > s.Limit+stagingHeadroom {
			return ledgerResult{Staged: s.Staged, Error: ErrQuotaExceeded}, true
		}
		return ledgerResult{Staged: s.Staged + s.Size}, true
	}
	return ledgerResult{}, false
}

func TestQuotaFixture(t *testing.T) {
	var f struct {
		Attribution []struct {
			Name   string
			Bucket struct {
				OwnerID     string `json:"owner_id"`
				DedupDomain string `json:"dedup_domain"`
			}
			Expect struct {
				ChargedUser string `json:"charged_user"`
				Domain      string
			}
		}
		Ledger []ledgerStep
	}
	readFixture(t, "quota.json", &f)
	for _, a := range f.Attribution {
		if a.Expect.ChargedUser != a.Bucket.OwnerID || a.Expect.Domain != a.Bucket.DedupDomain {
			t.Errorf("%s: charged %s in %s, the destination bucket is owned by %s in %s", a.Name, a.Expect.ChargedUser, a.Expect.Domain, a.Bucket.OwnerID, a.Bucket.DedupDomain)
		}
	}
	for _, s := range f.Ledger {
		got, ok := s.apply()
		switch {
		case !ok:
			t.Errorf("%s: unknown step %q", s.Name, s.Step)
		case got != s.Expect:
			t.Errorf("%s: got %+v, want %+v", s.Name, got, s.Expect)
		}
	}
}
