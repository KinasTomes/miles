package multipart

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"hash/crc32"
	"hash/crc64"
	"math/rand/v2"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
)

// multipart.json is the Complete contract of API v2 4.3.1, taken from
// requests of the pinned clients. Bodies are decoded with xml.Unmarshal: the
// document checks of s3.Payload.DecodeXML, which handlers use, have their
// own tests in internal/api/s3.

type fixtureUpload struct {
	BucketID          string `json:"bucket_id"`
	Key               string
	Initiator         string
	ChecksumAlgorithm string `json:"checksum_algorithm"`
	ChecksumType      string `json:"checksum_type"`
	Parts             []struct {
		PartNumber int `json:"part_number"`
		ETag       string
		Size       int64
		Checksum   string
	}
}

func (u fixtureUpload) upload() Upload {
	return Upload{s3.UploadBinding{BucketID: u.BucketID, Key: u.Key, Initiator: u.Initiator}, u.ChecksumAlgorithm, u.ChecksumType}
}

func (u fixtureUpload) heads() map[int]Head {
	heads := map[int]Head{}
	for _, p := range u.Parts {
		heads[p.PartNumber] = Head{PartNumber: p.PartNumber, ETag: p.ETag, Size: p.Size, Checksum: p.Checksum}
	}
	return heads
}

type multipartFixture struct {
	Uploads   map[string]fixtureUpload
	Canonical []struct {
		Name        string
		Upload      fixtureUpload
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
		Error   s3.ErrorCode
		State   string
	}
	States []struct {
		Op      s3.Operation
		State   string
		Expired bool   // now >= expires_at
		Digest  string // same or different input
		Lease   string // valid, grace or expired
		Receipt string // valid or expired
		Race    string // freeze or abort won before the part publish
		Expect  string
	}
}

func loadFixture(t *testing.T) multipartFixture {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/s3/multipart.json")
	if err != nil {
		t.Fatal(err)
	}
	var f multipartFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// normalize decodes body and normalizes the request on u; a body that is not
// XML is MalformedXML, as DecodeXML answers.
func normalize(u Upload, headers map[string]string, body string) (Input, *s3.Error) {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	var doc CompleteXML
	if xml.Unmarshal([]byte(body), &doc) != nil {
		return Input{}, &s3.Error{Code: s3.ErrMalformedXML}
	}
	return Normalize(u, h, &doc)
}

func TestCanonicalFixture(t *testing.T) {
	for _, c := range loadFixture(t).Canonical {
		t.Run(c.Name, func(t *testing.T) {
			in, e := normalize(c.Upload.upload(), c.Headers, c.Body)
			if e != nil {
				t.Fatalf("request rejected: %v", e)
			}
			if got := string(in.Canonical()); got != c.Canonical {
				t.Errorf("canonical\n%s\nwant\n%s", got, c.Canonical)
			}
			if got := in.Digest(); got != c.InputDigest {
				t.Errorf("input_digest %s, want %s", got, c.InputDigest)
			}
			_, res, err := Assemble(in, c.Upload.heads())
			if err != nil {
				t.Fatalf("frozen parts rejected: %v", err)
			}
			if want := (Result{ETag: c.Response.ETag, Checksum: c.Response.Checksum, ChecksumType: c.Response.ChecksumType}); res != want {
				t.Errorf("response %+v, want %+v", res, want)
			}
		})
	}
}

// TestValidationFixture runs each request through Normalize, then Assemble
// on the stored part heads: a Normalize error leaves the upload unchanged,
// an Assemble error reopens it Active.
func TestValidationFixture(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.Validation {
		t.Run(c.Name, func(t *testing.T) {
			u, ok := f.Uploads[c.Upload]
			if !ok {
				t.Fatalf("unknown upload %q", c.Upload)
			}
			code, state := s3.ErrorCode(""), "completed"
			in, e := normalize(u.upload(), c.Headers, c.Body)
			if e != nil {
				code, state = e.Code, "unchanged"
			} else if _, _, err := Assemble(in, u.heads()); err != nil {
				var se *s3.Error
				if !errors.As(err, &se) {
					t.Fatalf("stored heads rejected: %v", err)
				}
				code, state = se.Code, "active"
			}
			if code != c.Error || state != c.State {
				t.Errorf("got %q state %s, want %q state %s", code, state, c.Error, c.State)
			}
		})
	}
}

// TestStateFixture runs the state table rows through Dispatch at a fixed
// now, each time at its boundary: expired is now == expires_at, lease
// expired is now == lease_expires_at + grace, receipt expired is now ==
// receipt_expires_at. A lost race is Dispatch on the state that won. The
// unreadable row is the handlers' mapping of storage.ErrUnavailable.
func TestStateFixture(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, c := range loadFixture(t).States {
		if c.State == "unreadable" {
			if c.Expect != string(s3.ErrServiceUnavailable) {
				t.Errorf("unreadable %s: want ServiceUnavailable, fixture says %s", c.Op, c.Expect)
			}
			continue
		}
		st := Status{State: State(c.State), ExpiresAt: now.Add(time.Hour), InputDigest: "frozen"}
		switch c.Race {
		case "freeze":
			st.State = Completing
		case "abort":
			st.State = Aborting
		}
		if c.Expired {
			st.ExpiresAt = now
		}
		switch c.Lease {
		case "valid":
			st.LeaseExpiresAt = now.Add(time.Nanosecond)
		case "grace":
			st.LeaseExpiresAt = now.Add(-LeaseGrace + time.Nanosecond)
		default:
			st.LeaseExpiresAt = now.Add(-LeaseGrace)
		}
		st.ReceiptExpiresAt = now.Add(time.Nanosecond)
		if c.Receipt == "expired" {
			st.ReceiptExpiresAt = now
		}
		digest := "frozen"
		if c.Digest == "different" {
			digest = "other"
		}

		got := ""
		switch action, err := Dispatch(c.Op, st, digest, now); {
		case err != nil:
			var se *s3.Error
			if !errors.As(err, &se) {
				t.Fatalf("%+v: %v", c, err)
			}
			got = string(se.Code)
		default:
			got = map[Action]string{Proceed: "proceed", Takeover: "takeover", Replay: "receipt"}[action]
		}
		if got != c.Expect {
			t.Errorf("%+v: got %s", c, got)
		}
	}
}

// combine must give the CRC of the concatenation for every FULL_OBJECT
// algorithm, including empty and single-byte parts.
func TestCombine(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	direct := map[string]func([]byte) uint64{
		"CRC32":     func(b []byte) uint64 { return uint64(crc32.ChecksumIEEE(b)) },
		"CRC32C":    func(b []byte) uint64 { return uint64(crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))) },
		"CRC64NVME": func(b []byte) uint64 { return crc64.Checksum(b, crc64.MakeTable(0x9a6c9329ac4bc9b5)) },
	}
	for alg, sum := range direct {
		c := crcs[alg]
		for _, sizes := range [][]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}, {7, 4096, 1}, {5 << 20, 32}, {3, 0, 1000, 65537}} {
			var all []byte
			var crc uint64
			for i, n := range sizes {
				part := make([]byte, n)
				for j := range part {
					part[j] = byte(r.Uint32())
				}
				all = append(all, part...)
				if i == 0 {
					crc = sum(part)
				} else {
					crc = c.combine(crc, sum(part), int64(n))
				}
			}
			if want := sum(all); crc != want {
				t.Errorf("%s parts %v: combined %x, want %x", alg, sizes, crc, want)
			}
		}
	}
}
