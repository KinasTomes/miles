package object

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPutLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		meta map[string]string
		want error
	}{
		{"5 GiB boundary", MaxPutBytes, nil, nil},
		{"single PUT too large", MaxPutBytes + 1, nil, ErrObjectTooLarge},
		{"metadata exact boundary", 0, map[string]string{"x-amz-meta-k": strings.Repeat("x", MaxUserMetadataBytes-len("x-amz-meta-k"))}, nil},
		{"metadata over boundary", 0, map[string]string{"x-amz-meta-k": strings.Repeat("x", MaxUserMetadataBytes-len("x-amz-meta-k")+1)}, ErrMetadataTooLarge},
		{"UTF-8 exact boundary", 0, map[string]string{"x-amz-meta-k": strings.Repeat("é", 1018)}, nil},
		{"UTF-8 over boundary", 0, map[string]string{"x-amz-meta-k": strings.Repeat("é", 1019)}, ErrMetadataTooLarge},
		{"multiple entries", 0, map[string]string{"x-amz-meta-a": strings.Repeat("x", 1023), "x-amz-meta-b": strings.Repeat("x", 1024)}, ErrMetadataTooLarge},
		{"system metadata excluded", 0, map[string]string{"content-type": "text/plain", commitTimeKey: strings.Repeat("x", 2049)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidatePut(tc.size, tc.meta); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
}

func TestServiceRejectsPutLimitsBeforeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		meta map[string]string
		want error
	}{
		{"single PUT", MaxPutBytes + 1, nil, ErrObjectTooLarge},
		{"metadata", 0, map[string]string{"x-amz-meta-large": strings.Repeat("x", 2049)}, ErrMetadataTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixture(t)
			opts := f.options
			opts.AdmitWriter = func(context.Context, WriterRequest) (AdmittedWriter, error) {
				t.Error("oversize request acquired writer authority")
				return AdmittedWriter{}, nil
			}
			svc, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.PutObject(context.Background(), PutRequest{Bucket: f.bucket, Key: "file", Body: body(nil), ContentLength: tc.size, Metadata: tc.meta, UserID: "owner", CredentialID: "owner-key"})
			if !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			q, err := f.metadata.GetQuota(context.Background(), "owner")
			if err != nil || q.Used != 0 || q.Reserved != 0 || q.Staged != 0 {
				t.Fatal("oversize request changed quota", q, err)
			}
		})
	}
}
