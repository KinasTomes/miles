package s3

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// authorization.json, multipart.json and quota.json are the identity, quota
// and multipart contract of API v2 2.2.3, 2.6 and 4.3.1. No handler serves
// them yet (W02/W04), so these tests check every expected value against the
// rules written in those sections, encoded below as reference functions.
// Handlers must pass the same fixtures; their tests then replace these
// functions.

type principal struct {
	UserID      string `json:"user_id"`
	Read        bool
	Write       bool
	SystemAdmin bool   `json:"system_admin"`
	UserStatus  string `json:"user_status"`
	AuthStatus  string `json:"auth_status"`
}

type authCase struct {
	Name      string
	Op        Operation
	Principal string
	Bucket    string // "missing": no bucket of that name
	Key       string // "missing": no object at the key
	Initiator *bool  // nil: the caller started the upload
	Binding   string // other_key, other_bucket_id, unknown_id: upload_id does not bind
	Source    *struct {
		Bucket string
		Key    string
		Read   bool
	}
	Expect string
}

// rightTerms are the terms of the requires column: '|' separates
// alternatives, '+' joins terms that must all hold.
var rightTerms = []string{"read", "write", "owner", "admin", "initiator", "source_read", "self"}

func holds(requires string, rights map[string]bool, skip ...string) bool {
	for alt := range strings.SplitSeq(requires, "|") {
		ok := true
		for term := range strings.SplitSeq(alt, "+") {
			ok = ok && (rights[term] || slices.Contains(skip, term))
		}
		if ok {
			return true
		}
	}
	return false
}

// authorize is the check order of API v2 2.2.3 for one S3 request on the
// fixture bucket.
func authorize(requires string, p principal, owner bool, c authCase) string {
	switch {
	case p.AuthStatus == "revoked":
		return string(ErrInvalidAccessKeyID)
	case p.UserStatus == "disabled":
		return string(ErrAccessDenied)
	}
	rights := map[string]bool{
		"owner":       owner,
		"read":        owner || p.Read,
		"write":       owner || p.Write,
		"admin":       p.SystemAdmin,
		"initiator":   c.Initiator == nil || *c.Initiator,
		"source_read": owner || p.Read,
	}
	if c.Source != nil {
		rights["source_read"] = c.Source.Read
	}
	switch requires {
	case "listing":
		if rights["read"] || rights["write"] {
			return "listed"
		}
		return "hidden"
	case "create":
		switch {
		case c.Bucket == "missing":
			return "allow"
		case owner:
			return string(ErrBucketAlreadyOwnedByYou)
		}
		return string(ErrBucketAlreadyExists)
	}
	if c.Bucket == "missing" {
		return string(ErrNoSuchBucket)
	}
	// Destination rights come before any source or upload lookup.
	if !holds(requires, rights, "initiator", "source_read") {
		return string(ErrAccessDenied)
	}
	if c.Source != nil && c.Source.Bucket == "missing" {
		return string(ErrNoSuchBucket)
	}
	if !holds(requires, rights, "initiator") {
		return string(ErrAccessDenied)
	}
	if c.Source != nil && c.Source.Key == "missing" {
		return string(ErrNoSuchKey)
	}
	if c.Binding != "" {
		return string(ErrNoSuchUpload)
	}
	if !holds(requires, rights) {
		return string(ErrAccessDenied)
	}
	if c.Key == "missing" && c.Op != DeleteObject {
		return string(ErrNoSuchKey)
	}
	return "allow"
}

type snapshotCase struct {
	Name       string
	Phase      string // admit, publish or stream
	ReadAt     int64  `json:"read_at"`
	At         int64
	RevokedAt  *int64 `json:"revoked_at"`
	ExpiresAt  *int64 `json:"expires_at"`
	Expect     string
	MustReread bool `json:"must_reread"`
}

// snapshotVerdict applies the 5 s bound of API v2 2.2.3. may_allow: the
// node may still use a snapshot read before the revocation.
func snapshotVerdict(c snapshotCase) (verdict string, reread bool) {
	if c.Phase == "stream" {
		return "allow", false
	}
	validUntil := c.ReadAt + 5000
	if c.ExpiresAt != nil {
		validUntil = min(validUntil, *c.ExpiresAt)
	}
	reread = c.At >= validUntil
	seen := c.ReadAt // metadata time the snapshot reflects
	if reread {
		seen = c.At
	}
	switch {
	case c.ExpiresAt != nil && c.At >= *c.ExpiresAt, c.RevokedAt != nil && *c.RevokedAt <= seen:
		return "deny", reread
	case c.RevokedAt != nil && *c.RevokedAt <= c.At:
		return "may_allow", reread
	}
	return "allow", reread
}

func TestAuthorizationFixture(t *testing.T) {
	var f struct {
		Bucket struct {
			OwnerID string `json:"owner_id"`
		}
		Principals map[string]principal
		S3         []struct {
			Op       Operation
			Requires string
			Expect   map[string]string
		}
		S3Cases  []authCase `json:"s3_cases"`
		Snapshot []snapshotCase
		Admin    []struct {
			Endpoint string
			Requires string
			UserID   string `json:"user_id"`
			Expect   map[string]string
		}
	}
	readFixture(t, "authorization.json", &f)
	checkTerms := func(row, requires string) {
		if requires == "listing" || requires == "create" {
			return
		}
		for term := range strings.FieldsFuncSeq(requires, func(r rune) bool { return r == '|' || r == '+' }) {
			if !slices.Contains(rightTerms, term) {
				t.Errorf("%s: unknown term %q in requires", row, term)
			}
		}
	}
	checkColumns := func(row string, expect map[string]string) {
		for name := range f.Principals {
			if _, ok := expect[name]; !ok {
				t.Errorf("%s: no cell for %s", row, name)
			}
		}
		for name := range expect {
			if _, ok := f.Principals[name]; !ok {
				t.Errorf("%s: unknown principal %s", row, name)
			}
		}
	}

	requires := map[Operation]string{}
	for _, row := range f.S3 {
		requires[row.Op] = row.Requires
		checkTerms(string(row.Op), row.Requires)
		checkColumns(string(row.Op), row.Expect)
		for name, want := range row.Expect {
			p := f.Principals[name]
			got := authorize(row.Requires, p, p.UserID == f.Bucket.OwnerID, authCase{Op: row.Op})
			if got != want {
				t.Errorf("%s by %s: got %s, want %s", row.Op, name, got, want)
			}
		}
	}
	for _, r := range routes {
		if _, ok := requires[r.op]; r.scope <= p1 && !ok {
			t.Errorf("%s has no row in s3", r.op)
		}
	}
	for _, c := range f.S3Cases {
		p, ok := f.Principals[c.Principal]
		req, known := requires[c.Op]
		if !ok || !known {
			t.Errorf("%s: unknown principal %q or operation %q", c.Name, c.Principal, c.Op)
			continue
		}
		if got := authorize(req, p, p.UserID == f.Bucket.OwnerID, c); got != c.Expect {
			t.Errorf("%s: got %s, want %s", c.Name, got, c.Expect)
		}
	}
	for _, c := range f.Snapshot {
		if got, reread := snapshotVerdict(c); got != c.Expect || reread != c.MustReread {
			t.Errorf("%s: got %s must_reread=%t, want %s must_reread=%t", c.Name, got, reread, c.Expect, c.MustReread)
		}
	}
	for _, row := range f.Admin {
		checkTerms(row.Endpoint, row.Requires)
		checkColumns(row.Endpoint, row.Expect)
		for name, want := range row.Expect {
			p := f.Principals[name]
			got := "403"
			switch {
			case p.AuthStatus == "revoked" || p.UserStatus == "disabled":
				got = "401"
			case holds(row.Requires, map[string]bool{"owner": p.UserID == f.Bucket.OwnerID, "admin": p.SystemAdmin, "self": p.UserID == row.UserID}):
				got = "allow"
			}
			if got != want {
				t.Errorf("%s by %s: got %s, want %s", row.Endpoint, name, got, want)
			}
		}
	}
}

type storedPart struct {
	PartNumber int `json:"part_number"`
	ETag       string
	Size       int64
	Checksum   string
}

type storedUpload struct {
	BucketID          string `json:"bucket_id"`
	Key               string
	ChecksumAlgorithm string `json:"checksum_algorithm"`
	ChecksumType      string `json:"checksum_type"`
	Parts             []storedPart
}

// completeInput is the canonical Complete input v1 of API v2 4.3.1, fields
// in serialization order.
type completeInput struct {
	V                 int     `json:"v"`
	BucketID          string  `json:"bucket_id"`
	Key               string  `json:"key"`
	ChecksumAlgorithm string  `json:"checksum_algorithm"`
	ChecksumType      string  `json:"checksum_type"`
	ObjectChecksum    *string `json:"object_checksum"`
	ObjectSize        *int64  `json:"object_size"`
	Parts             []struct {
		PartNumber int     `json:"part_number"`
		ETag       string  `json:"etag"`
		Checksum   *string `json:"checksum"`
	} `json:"parts"`
}

func TestMultipartFixture(t *testing.T) {
	var f struct {
		Uploads   map[string]storedUpload
		Canonical []struct {
			Name        string
			Upload      storedUpload
			Canonical   string
			InputDigest string `json:"input_digest"`
			Response    struct {
				ETag         string
				Checksum     string
				ChecksumType string `json:"checksum_type"`
			}
		}
		Validation []struct {
			Name   string
			Upload string
			Error  ErrorCode
			State  string
		}
		States []struct {
			Op     Operation
			State  string
			Expect string
		}
	}
	readFixture(t, "multipart.json", &f)

	for _, c := range f.Canonical {
		t.Run(c.Name, func(t *testing.T) {
			sum := sha256.Sum256([]byte(c.Canonical))
			if got := hex.EncodeToString(sum[:]); got != c.InputDigest {
				t.Errorf("input_digest %s, SHA-256 of canonical is %s", c.InputDigest, got)
			}
			dec := json.NewDecoder(strings.NewReader(c.Canonical))
			dec.DisallowUnknownFields()
			var in completeInput
			if err := dec.Decode(&in); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(in); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSuffix(buf.String(), "\n"); got != c.Canonical {
				t.Errorf("not in canonical encoding, want\n%s", got)
			}
			u := c.Upload
			if in.V != 1 || in.BucketID != u.BucketID || in.Key != u.Key || in.ChecksumAlgorithm != u.ChecksumAlgorithm || in.ChecksumType != u.ChecksumType {
				t.Errorf("canonical %+v does not match upload %+v", in, u)
			}

			// The stored response: ETag is MD5 of the binary part MD5s,
			// COMPOSITE checksum the algorithm over the binary part
			// digests, both suffixed with the part count (API v2 2.7).
			// FULL_OBJECT over several parts needs the CRC combine of W04;
			// its fixture value is the CRC of the whole object.
			var md5s, digests []byte
			for _, p := range in.Parts {
				i := slices.IndexFunc(u.Parts, func(s storedPart) bool { return s.PartNumber == p.PartNumber })
				if i < 0 {
					t.Fatalf("part %d is not stored", p.PartNumber)
				}
				m, err1 := hex.DecodeString(u.Parts[i].ETag)
				d, err2 := base64.StdEncoding.DecodeString(u.Parts[i].Checksum)
				if err1 != nil || err2 != nil {
					t.Fatalf("part %d: %v %v", p.PartNumber, err1, err2)
				}
				md5s, digests = append(md5s, m...), append(digests, d...)
			}
			n := "-" + strconv.Itoa(len(in.Parts))
			etag := md5.Sum(md5s)
			if want := `"` + hex.EncodeToString(etag[:]) + n + `"`; c.Response.ETag != want {
				t.Errorf("response ETag %s, want %s", c.Response.ETag, want)
			}
			want := ""
			switch {
			case in.ChecksumType == "COMPOSITE" && in.ChecksumAlgorithm == "SHA256":
				h := sha256.Sum256(digests)
				want = base64.StdEncoding.EncodeToString(h[:]) + n
			case in.ChecksumType == "COMPOSITE" && in.ChecksumAlgorithm == "CRC32":
				want = base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(digests))) + n
			case in.ChecksumType == "COMPOSITE":
				t.Fatalf("no reference for %s COMPOSITE", in.ChecksumAlgorithm)
			case len(in.Parts) == 1:
				want = base64.StdEncoding.EncodeToString(digests)
			default:
				want = c.Response.Checksum
			}
			if c.Response.Checksum != want || c.Response.ChecksumType != in.ChecksumType {
				t.Errorf("response checksum %s %s, want %s %s", c.Response.Checksum, c.Response.ChecksumType, want, in.ChecksumType)
			}
		})
	}

	for _, c := range f.Validation {
		_, upload := f.Uploads[c.Upload]
		_, known := errorCodes[c.Error]
		if !upload || c.Error != "" && !known || !slices.Contains([]string{"unchanged", "active", "completed"}, c.State) || (c.Error == "") != (c.State == "completed") {
			t.Errorf("validation %q: upload %q, error %q or state %q is invalid", c.Name, c.Upload, c.Error, c.State)
		}
	}
	multipartOps := []Operation{CompleteMultipartUpload, UploadPart, AbortMultipartUpload, ListParts}
	for _, c := range f.States {
		_, known := errorCodes[ErrorCode(c.Expect)]
		if !slices.Contains(multipartOps, c.Op) || !known && !slices.Contains([]string{"proceed", "receipt", "takeover"}, c.Expect) {
			t.Errorf("states %s %s: invalid op or expect %q", c.Op, c.State, c.Expect)
		}
	}
}

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
