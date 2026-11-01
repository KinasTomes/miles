package multipart

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/multipart/core"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func completeParts(c *awss3.Client, id string, etags ...string) (*awss3.CompleteMultipartUploadOutput, error) {
	var parts []types.CompletedPart
	for i, e := range etags {
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: aws.String(e)})
	}
	return c.CompleteMultipartUpload(context.Background(), &awss3.CompleteMultipartUploadInput{Bucket: aws.String("demo"), Key: aws.String("key"),
		UploadId: aws.String(id), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
}

// TestSDKComplete runs Complete through the router with the pinned SDK: the
// state table with the input digest decides before the service, and the
// receipt result parses as CompleteMultipartUploadResult.
func TestSDKComplete(t *testing.T) {
	etag := `"0123456789ABCDEF0123456789ABCDEF"`
	in := core.Input{V: 1, BucketID: "bucket-1", Key: "key", ChecksumAlgorithm: "CRC64NVME", ChecksumType: "FULL_OBJECT",
		Parts: []core.Part{{PartNumber: 1, ETag: "0123456789abcdef0123456789abcdef"}}}
	head := storage.ObjectHead{ETag: `"9b2cf535f27731c974343645a3985328-1"`, Checksum: storage.Checksum{Algorithm: "CRC64NVME", Type: "FULL_OBJECT", Value: "Y53hf1fT3M4=", PartCount: 1}}
	for _, tc := range []struct {
		name, state    string
		digest         string        // stored input digest
		lease          time.Duration // lease end from now
		receipt        bool
		serviceErr     error
		client         string
		want           string // error code, "" = completed
		reachesService bool
	}{
		{name: "active", state: "Active", client: "writer-key", reachesService: true},
		{name: "owner is not the initiator", state: "Active", client: "owner-key", want: "AccessDenied"},
		{name: "completing in its lease", state: "Completing", digest: in.Digest(), lease: time.Minute, client: "writer-key", want: "OperationAborted"},
		{name: "completing in its grace", state: "Completing", digest: in.Digest(), lease: -4 * time.Second, client: "writer-key", want: "OperationAborted"},
		{name: "completing with other parts", state: "Completing", digest: strings.Repeat("0", 64), lease: -time.Hour, client: "writer-key", want: "InvalidRequest"},
		{name: "completing past its grace", state: "Completing", digest: in.Digest(), lease: -6 * time.Second, client: "writer-key", reachesService: true},
		{name: "completed replays", state: "Completed", digest: in.Digest(), receipt: true, client: "writer-key", reachesService: true},
		{name: "completed with other parts", state: "Completed", digest: strings.Repeat("0", 64), receipt: true, client: "writer-key", want: "InvalidRequest"},
		{name: "completed receipt expired", state: "Completed", digest: in.Digest(), client: "writer-key", want: "NoSuchUpload"},
		{name: "aborted", state: "Aborted", client: "writer-key", want: "NoSuchUpload"},
		{name: "part check fails", state: "Active", client: "writer-key", serviceErr: &core.ValidationError{Code: "InvalidPart", Message: "part 1"}, want: "InvalidPart", reachesService: true},
		{name: "quota at commit", state: "Active", client: "writer-key", serviceErr: storage.ErrQuotaExceeded, want: "QuotaExceeded", reachesService: true},
		{name: "unknown outcome", state: "Active", client: "writer-key", serviceErr: storage.ErrUnavailable, want: "ServiceUnavailable", reachesService: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			u := w.upload(tc.state)
			u.Input.SHA256, u.LeaseUntil = tc.digest, w.now.Add(tc.lease)
			w.svc.set(u)
			if tc.receipt {
				w.svc.receipts[u.ID] = w.now.Add(time.Hour)
			}
			w.svc.completeHead, w.svc.completeErr = head, tc.serviceErr
			out, err := completeParts(w.clients[tc.client], u.ID, etag)
			if code(err) != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if got := len(w.svc.completed) == 1; got != tc.reachesService {
				t.Fatalf("service called %d times", len(w.svc.completed))
			}
			if tc.reachesService {
				r := w.svc.completed[0]
				if r.Input.Digest() != in.Digest() || r.UserID != "writer" || r.CredentialID != "writer-key" || r.Upload.ID != u.ID {
					t.Fatalf("service got %+v", r)
				}
			}
			if tc.want != "" {
				return
			}
			if aws.ToString(out.ETag) != head.ETag || aws.ToString(out.ChecksumCRC64NVME) != "Y53hf1fT3M4=" || out.ChecksumType != types.ChecksumTypeFullObject ||
				aws.ToString(out.Bucket) != "demo" || aws.ToString(out.Key) != "key" || !strings.HasSuffix(aws.ToString(out.Location), "/demo/key") {
				t.Fatalf("result %+v", out)
			}
		})
	}
}
