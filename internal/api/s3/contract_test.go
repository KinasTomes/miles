package s3

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"hash/crc32"
	"hash/crc64"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// authorization.json, multipart.json and quota.json are the identity, quota
// and multipart contract of API v2 2.2.3, 2.6 and 4.3.1. No handler serves
// them yet (W02/W04), so these tests run every fixture input through
// reference functions written from those sections and compare the results.
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

// snapshotVerdict applies the 5 s bound of API v2 2.2.3 to admissions.
// may_allow: the node may still use a snapshot read before the revocation.
// A write transaction committed at At checks the authorization items
// itself, so its verdict ignores the snapshot.
func snapshotVerdict(c snapshotCase) (verdict string, reread bool) {
	switch c.Phase {
	case "stream":
		return "allow", false
	case "publish":
		if c.RevokedAt != nil && *c.RevokedAt <= c.At {
			return "deny", false
		}
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

type canonicalPart struct {
	PartNumber int     `json:"part_number"`
	ETag       string  `json:"etag"`
	Checksum   *string `json:"checksum"`
}

// completeInput is the canonical Complete input v1 of API v2 4.3.1, fields
// in serialization order.
type completeInput struct {
	V                 int             `json:"v"`
	BucketID          string          `json:"bucket_id"`
	Key               string          `json:"key"`
	ChecksumAlgorithm string          `json:"checksum_algorithm"`
	ChecksumType      string          `json:"checksum_type"`
	ObjectChecksum    *string         `json:"object_checksum"`
	ObjectSize        *int64          `json:"object_size"`
	Parts             []canonicalPart `json:"parts"`
}

func (in completeInput) canonical() string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(in); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

const (
	minPartSize   = 5 << 20
	maxObjectSize = 100 << 30
)

var digestSize = map[string]int{"CRC64NVME": 8, "CRC32": 4, "CRC32C": 4, "SHA1": 20, "SHA256": 32}

// normalizeComplete is rows 1-5 of the API v2 4.3.1 validation: the
// canonical input of a Complete request on upload u, or the first error.
// Header names in h are lower case.
func normalizeComplete(u storedUpload, h map[string]string, body string) (completeInput, ErrorCode) {
	in := completeInput{V: 1, BucketID: u.BucketID, Key: u.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType}
	var doc struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []struct {
			PartNumber, ETag []string
			Checksums        []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"Part"`
	}
	if xml.Unmarshal([]byte(body), &doc) != nil || len(doc.Parts) == 0 {
		return in, ErrMalformedXML
	}
	nums := make([]int, len(doc.Parts))
	for i, p := range doc.Parts {
		if len(p.PartNumber) != 1 || len(p.ETag) != 1 {
			return in, ErrMalformedXML
		}
		var err error
		if nums[i], err = strconv.Atoi(p.PartNumber[0]); err != nil {
			return in, ErrMalformedXML
		}
		for _, c := range p.Checksums {
			if !strings.HasPrefix(c.XMLName.Local, "Checksum") {
				return in, ErrMalformedXML
			}
		}
	}

	digest := func(v string) *string {
		b, err := base64.StdEncoding.Strict().DecodeString(v)
		if err != nil || len(b) != digestSize[u.ChecksumAlgorithm] {
			return nil
		}
		return new(base64.StdEncoding.EncodeToString(b))
	}
	if t, ok := h["x-amz-checksum-type"]; ok && t != u.ChecksumType {
		return in, ErrInvalidRequest
	}
	for name, v := range h {
		alg, ok := strings.CutPrefix(name, "x-amz-checksum-")
		if !ok || alg == "type" || alg == "algorithm" || alg == "mode" {
			continue
		}
		if in.ObjectChecksum = digest(v); in.ObjectChecksum == nil || !strings.EqualFold(alg, u.ChecksumAlgorithm) || u.ChecksumType == "COMPOSITE" {
			return in, ErrInvalidRequest
		}
	}
	sums := make([]*string, len(doc.Parts))
	for i, p := range doc.Parts {
		if len(p.Checksums) == 0 {
			continue
		}
		if sums[i] = digest(p.Checksums[0].Value); len(p.Checksums) > 1 || sums[i] == nil || p.Checksums[0].XMLName.Local != "Checksum"+u.ChecksumAlgorithm {
			return in, ErrInvalidRequest
		}
	}

	for _, n := range nums {
		if n < 1 || n > maxParts {
			return in, ErrInvalidArgument
		}
	}
	if v, ok := h["x-amz-mp-object-size"]; ok {
		n, err := strconv.ParseUint(v, 10, 63)
		if err != nil {
			return in, ErrInvalidArgument
		}
		in.ObjectSize = new(int64(n))
	}
	for i, n := range nums {
		if i > 0 && n <= nums[i-1] || u.ChecksumType == "COMPOSITE" && n != i+1 {
			return in, ErrInvalidPartOrder
		}
	}
	for i, p := range doc.Parts {
		if u.ChecksumType == "COMPOSITE" && sums[i] == nil {
			return in, ErrInvalidRequest
		}
		etag := p.ETag[0]
		if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
			etag = etag[1 : len(etag)-1]
		}
		in.Parts = append(in.Parts, canonicalPart{nums[i], strings.ToLower(etag), sums[i]})
	}
	return in, ""
}

// freeze is rows 6-11 of the API v2 4.3.1 validation: input in against
// the part heads of upload u. It returns the selected heads in input order.
func freeze(u storedUpload, in completeInput) ([]storedPart, ErrorCode) {
	heads := make([]storedPart, len(in.Parts))
	for i, p := range in.Parts {
		j := slices.IndexFunc(u.Parts, func(s storedPart) bool { return s.PartNumber == p.PartNumber })
		if j < 0 || u.Parts[j].ETag != p.ETag {
			return nil, ErrInvalidPart
		}
		heads[i] = u.Parts[j]
	}
	for i, p := range in.Parts {
		if p.Checksum != nil && *p.Checksum != heads[i].Checksum {
			return nil, ErrBadDigest
		}
	}
	var total int64
	for i, h := range heads {
		if i < len(heads)-1 && h.Size < minPartSize {
			return nil, ErrEntityTooSmall
		}
		total += h.Size
	}
	switch {
	case total > maxObjectSize:
		return nil, ErrEntityTooLarge
	case in.ObjectSize != nil && *in.ObjectSize != total:
		return nil, ErrInvalidRequest
	case in.ObjectChecksum != nil && *in.ObjectChecksum != fullObjectChecksum(in.ChecksumAlgorithm, heads):
		return nil, ErrBadDigest
	}
	return heads, ""
}

// fullObjectChecksum combines the part CRCs in order. Update is affine in
// its start value, so crc(A||B) = crc(B) ^ Update(crc(A), 0^n) ^ Update(0, 0^n)
// with n = len(B).
// ponytail: feeds n zero bytes, O(object size); fine for fixtures, W04 needs
// the O(log n) combine.
func fullObjectChecksum(alg string, heads []storedPart) string {
	if alg != "CRC64NVME" {
		panic("no reference combine for " + alg)
	}
	var crc uint64
	for _, h := range heads {
		b, _ := base64.StdEncoding.DecodeString(h.Checksum)
		zeros := make([]byte, h.Size)
		crc = binary.BigEndian.Uint64(b) ^ crc64.Update(crc, crc64NVME, zeros) ^ crc64.Update(0, crc64NVME, zeros)
	}
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint64(nil, crc))
}

type stateCase struct {
	Op      Operation
	State   string
	Expired bool   // now >= expires_at
	Digest  string // same or different input
	Lease   string // valid, grace or expired
	Receipt string // valid or expired
	Race    string // freeze or abort won before the part publish
	Expect  string
}

// dispatch is the state table of API v2 4.3.1.
func dispatch(c stateCase) string {
	switch {
	case c.State == "unreadable":
		return string(ErrServiceUnavailable)
	case c.Race == "freeze":
		return string(ErrOperationAborted)
	case c.Race == "abort":
		return string(ErrNoSuchUpload)
	}
	switch c.State {
	case "active":
		if c.Expired {
			return string(ErrNoSuchUpload)
		}
		return "proceed"
	case "completing":
		switch {
		case c.Op == ListParts:
			return "proceed"
		case c.Op != CompleteMultipartUpload:
			return string(ErrOperationAborted)
		case c.Digest != "same":
			return string(ErrInvalidRequest)
		case c.Lease != "expired":
			return string(ErrOperationAborted)
		}
		return "takeover"
	case "completed":
		switch {
		case c.Receipt != "valid":
			return string(ErrNoSuchUpload)
		case c.Op == AbortMultipartUpload:
			return string(ErrOperationAborted)
		case c.Op != CompleteMultipartUpload:
			return string(ErrNoSuchUpload)
		case c.Digest != "same":
			return string(ErrInvalidRequest)
		}
		return "receipt"
	}
	return string(ErrNoSuchUpload) // aborting, aborted
}

func TestMultipartFixture(t *testing.T) {
	var f struct {
		Uploads   map[string]storedUpload
		Canonical []struct {
			Name        string
			Upload      storedUpload
			Headers     map[string]string
			Body        string
			Canonical   string
			InputDigest string `json:"input_digest"`
			Response    struct {
				ETag         string
				Checksum     string
				ChecksumType string `json:"checksum_type"`
			}
		}
		Validation []struct {
			Name    string
			Upload  string
			Headers map[string]string
			Body    string
			Error   ErrorCode
			State   string
		}
		States []stateCase
	}
	readFixture(t, "multipart.json", &f)

	for _, c := range f.Canonical {
		t.Run(c.Name, func(t *testing.T) {
			in, code := normalizeComplete(c.Upload, c.Headers, c.Body)
			if code != "" {
				t.Fatalf("request rejected with %s", code)
			}
			if got := in.canonical(); got != c.Canonical {
				t.Errorf("canonical\n%s\nwant\n%s", got, c.Canonical)
			}
			sum := sha256.Sum256([]byte(c.Canonical))
			if got := hex.EncodeToString(sum[:]); got != c.InputDigest {
				t.Errorf("input_digest %s, SHA-256 of canonical is %s", c.InputDigest, got)
			}
			heads, code := freeze(c.Upload, in)
			if code != "" {
				t.Fatalf("frozen parts rejected with %s", code)
			}

			// The stored response: ETag is MD5 of the binary part MD5s,
			// COMPOSITE checksum the algorithm over the binary part
			// digests, both suffixed with the part count (API v2 2.7).
			var md5s, digests []byte
			for _, h := range heads {
				m, _ := hex.DecodeString(h.ETag)
				d, _ := base64.StdEncoding.DecodeString(h.Checksum)
				md5s, digests = append(md5s, m...), append(digests, d...)
			}
			n := "-" + strconv.Itoa(len(heads))
			etag := md5.Sum(md5s)
			if want := `"` + hex.EncodeToString(etag[:]) + n + `"`; c.Response.ETag != want {
				t.Errorf("response ETag %s, want %s", c.Response.ETag, want)
			}
			var want string
			switch {
			case in.ChecksumType == "FULL_OBJECT":
				want = fullObjectChecksum(in.ChecksumAlgorithm, heads)
			case in.ChecksumAlgorithm == "SHA256":
				h := sha256.Sum256(digests)
				want = base64.StdEncoding.EncodeToString(h[:]) + n
			case in.ChecksumAlgorithm == "CRC32":
				want = base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(digests))) + n
			default:
				t.Fatalf("no reference for %s COMPOSITE", in.ChecksumAlgorithm)
			}
			if c.Response.Checksum != want || c.Response.ChecksumType != in.ChecksumType {
				t.Errorf("response checksum %s %s, want %s %s", c.Response.Checksum, c.Response.ChecksumType, want, in.ChecksumType)
			}
		})
	}

	for _, c := range f.Validation {
		u, ok := f.Uploads[c.Upload]
		if !ok {
			t.Errorf("validation %q: unknown upload %q", c.Name, c.Upload)
			continue
		}
		state := "completed"
		in, code := normalizeComplete(u, c.Headers, c.Body)
		if code != "" {
			state = "unchanged"
		} else if _, code = freeze(u, in); code != "" {
			state = "active"
		}
		if code != c.Error || state != c.State {
			t.Errorf("validation %q: got %q state %s, want %q state %s", c.Name, code, state, c.Error, c.State)
		}
	}
	for _, c := range f.States {
		if got := dispatch(c); got != c.Expect {
			t.Errorf("states %+v: got %s", c, got)
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
