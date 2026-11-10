package s3

import (
	"context"
	"net/http"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

type fakeRestorer struct {
	fakeObjectReader
	got    []object.RestoreRequest
	result object.RestoreResult
	err    error
}

func (f *fakeRestorer) RestoreObject(_ context.Context, r object.RestoreRequest) (object.RestoreResult, error) {
	f.got = append(f.got, r)
	return f.result, f.err
}

func restoreDoc(inner string) string {
	return `<RestoreRequest xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` + inner + `</RestoreRequest>`
}

func TestRestoreObjectStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result object.RestoreResult
		err    error
		status int
		code   ErrorCode
	}{
		{name: "started", result: object.RestoreResult{Initiated: 1, InProgress: 2}, status: http.StatusAccepted},
		{name: "already restoring", result: object.RestoreResult{InProgress: 1}, code: ErrRestoreAlreadyInProgress},
		{name: "restored for Days", status: http.StatusOK},
		{name: "Warm or IR only", err: object.ErrNotArchived, code: ErrInvalidRequest},
		{name: "pack mid-transition", err: storage.ErrCold, code: ErrInvalidObjectState},
		{name: "missing key", err: storage.ErrNotFound, code: ErrNoSuchKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBucketWorld(t)
			restorer := &fakeRestorer{result: tc.result, err: tc.err}
			w.router.Handlers = NewObjectHandlers(nil, restorer, nil, nil)
			rec, e := w.send(t, http.MethodPost, "/demo/k?restore", "reader", restoreDoc(`<Days>7</Days><GlacierJobParameters><Tier>Expedited</Tier></GlacierJobParameters>`))
			if tc.code != "" {
				if e == nil || e.Code != tc.code {
					t.Fatalf("error %v, want %s", e, tc.code)
				}
			} else if e != nil || rec.Code != tc.status || rec.Body.Len() != 0 {
				t.Fatalf("status %d body %q error %v, want %d", rec.Code, rec.Body, e, tc.status)
			}
			want := object.RestoreRequest{Bucket: "demo", BucketID: "bkt-demo-0001", Key: "k", CredentialID: accessKey("reader"), Days: 7, Tier: "Expedited"}
			if len(restorer.got) != 1 || restorer.got[0] != want {
				t.Fatalf("service saw %+v", restorer.got)
			}
		})
	}
}

func TestRestoreObjectValidatesBeforeService(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       ErrorCode
	}{
		{"empty body", "", ErrMalformedXML},
		{"other document", `<Delete><Object><Key>k</Key></Object></Delete>`, ErrMalformedXML},
		{"Days not a number", restoreDoc(`<Days>one</Days>`), ErrMalformedXML},
		{"no Days", restoreDoc(`<GlacierJobParameters><Tier>Bulk</Tier></GlacierJobParameters>`), ErrInvalidRequest},
		{"zero Days", restoreDoc(`<Days>0</Days>`), ErrInvalidRequest},
		{"Days above the DSG cap", restoreDoc(`<Days>31</Days>`), ErrInvalidRequest},
		{"unknown Tier", restoreDoc(`<Days>1</Days><GlacierJobParameters><Tier>Instant</Tier></GlacierJobParameters>`), ErrInvalidRequest},
		{"SELECT restore", restoreDoc(`<Days>1</Days><Type>SELECT</Type>`), ErrNotImplemented},
		{"output location", restoreDoc(`<Days>1</Days><OutputLocation><S3><BucketName>x</BucketName></S3></OutputLocation>`), ErrNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBucketWorld(t)
			restorer := &fakeRestorer{}
			w.router.Handlers = NewObjectHandlers(nil, restorer, nil, nil)
			_, e := w.send(t, http.MethodPost, "/demo/k?restore", "owner", tc.body)
			if e == nil || e.Code != tc.code || len(restorer.got) != 0 {
				t.Fatalf("error %v, want %s; service calls %d", e, tc.code, len(restorer.got))
			}
		})
	}
}

func TestRestoreObjectDefaultsAndPermission(t *testing.T) {
	w := newBucketWorld(t)
	restorer := &fakeRestorer{}
	w.router.Handlers = NewObjectHandlers(nil, restorer, nil, nil)
	if _, e := w.send(t, http.MethodPost, "/demo/k?restore", "owner", restoreDoc(`<Days>1</Days>`)); e != nil || len(restorer.got) != 1 || restorer.got[0].Tier != "Standard" {
		t.Fatal("default tier", e, restorer.got)
	}
	// Restore needs read, not write: a write-only grantee is denied first.
	if _, e := w.send(t, http.MethodPost, "/demo/k?restore", "writer", restoreDoc(`<Days>1</Days>`)); e == nil || e.Code != ErrAccessDenied || len(restorer.got) != 1 {
		t.Fatal("writer restored", e)
	}
	// A reader without the restore port does not pretend to accept.
	w.router.Handlers = NewObjectHandlers(nil, &fakeObjectReader{}, nil, nil)
	if _, e := w.send(t, http.MethodPost, "/demo/k?restore", "owner", restoreDoc(`<Days>1</Days>`)); e == nil || e.Code != ErrNotImplemented {
		t.Fatal(e)
	}
}

func TestHeadObjectRestoreHeader(t *testing.T) {
	for _, restore := range []string{"", `ongoing-request="true"`, `ongoing-request="false", expiry-date="Fri, 13 Nov 2026 00:00:00 GMT"`} {
		w := newBucketWorld(t)
		stat := objectInfo(3, `"etag"`)
		stat.Info.Restore = restore
		w.router.Handlers = NewObjectHandlers(nil, &fakeObjectReader{stat: stat}, nil, nil)
		rec, e := w.send(t, http.MethodHead, "/demo/k", "reader", "")
		values := rec.Header().Values("X-Amz-Restore")
		if e != nil || rec.Code != http.StatusOK || restore == "" && len(values) != 0 || restore != "" && (len(values) != 1 || values[0] != restore) {
			t.Fatalf("restore %q: status %d headers %q error %v", restore, rec.Code, values, e)
		}
	}
}
