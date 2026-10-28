package s3

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

// Stat and Get deliberately expose different revisions, as an overwrite can.
// Get selects from exactly the head whose bytes it emits.
type boundObjectReader struct {
	oldSize   int64
	body      []byte
	statCalls int
}

func (b *boundObjectReader) Stat(context.Context, object.GetRequest) (object.GetResult, error) {
	b.statCalls++
	return objectInfo(b.oldSize, `"old"`), nil
}
func (b *boundObjectReader) GetObject(ctx context.Context, r object.GetRequest) (object.GetResult, error) {
	out := objectInfo(int64(len(b.body)), `"new"`)
	span := r.Range
	if r.SelectRange != nil {
		var err error
		span, err = r.SelectRange(out.Info)
		if err != nil {
			return out, err
		}
	}
	if span != nil {
		out.Range = *span
	}
	if err := out.Range.Validate(out.Info.Size); err != nil {
		return out, err
	}
	if err := r.Ready(out); err != nil {
		return out, err
	}
	return out, r.Emit(ctx, b.body[out.Range.Offset:out.Range.Offset+out.Range.Length])
}
func TestRangeSelectsBoundHeadDuringOverwrite(t *testing.T) {
	for _, tc := range []struct {
		name, spec       string
		oldSize, newSize int64
		want             storage.ByteRange
	}{
		{"suffix growth", "bytes=-10", 100, 200, storage.ByteRange{Offset: 190, Length: 10}},
		{"open growth", "bytes=90-", 100, 200, storage.ByteRange{Offset: 90, Length: 110}},
		{"clamped growth", "bytes=90-1000", 100, 200, storage.ByteRange{Offset: 90, Length: 110}},
		{"suffix shrink", "bytes=-10", 200, 100, storage.ByteRange{Offset: 90, Length: 10}},
		{"new head makes range valid", "bytes=150-", 100, 200, storage.ByteRange{Offset: 150, Length: 50}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &boundObjectReader{oldSize: tc.oldSize, body: []byte(strings.Repeat("a", int(tc.newSize)-10) + "0123456789")}
			r := objectRequest(http.MethodGet, "demo", "file", nil, map[string]string{"Range": tc.spec})
			rec := httptest.NewRecorder()
			if err := (&Objects{Reader: reader}).GetObject(rec, r); err != nil {
				t.Fatal(err)
			}
			want := reader.body[tc.want.Offset : tc.want.Offset+tc.want.Length]
			if rec.Code != 206 || rec.Body.String() != string(want) || rec.Header().Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", tc.want.Offset, tc.want.Offset+tc.want.Length-1, tc.newSize) || reader.statCalls != 0 {
				t.Fatalf("range used another head: status=%d range=%s body=%q statCalls=%d", rec.Code, rec.Header().Get("Content-Range"), rec.Body.String(), reader.statCalls)
			}
		})
	}
}
func TestIfMatchUsesBoundHeadBeforeEmission(t *testing.T) {
	reader := &boundObjectReader{oldSize: 100, body: []byte("new body")}
	r := objectRequest(http.MethodGet, "demo", "file", nil, map[string]string{"If-Match": `"old"`})
	rec := httptest.NewRecorder()
	err := (&Objects{Reader: reader}).GetObject(rec, r)
	if code(t, err) != ErrPreconditionFailed || rec.Body.Len() != 0 || rec.Header().Get("ETag") != "" || reader.statCalls != 0 {
		t.Fatal("precondition used wrong head or emitted data", err, rec)
	}
}
