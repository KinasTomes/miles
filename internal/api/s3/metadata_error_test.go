package s3

import (
	"errors"
	"net/http"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestMetadataWriteError(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   ErrorCode
		status int
	}{
		{storage.ErrQuotaExceeded, ErrQuotaExceeded, http.StatusForbidden},
		{storage.ErrConflict, ErrOperationAborted, http.StatusConflict},
		{storage.ErrUnavailable, ErrServiceUnavailable, http.StatusServiceUnavailable},
		{storage.ErrFenced, ErrServiceUnavailable, http.StatusServiceUnavailable},
		{storage.ErrAccessDenied, ErrAccessDenied, http.StatusForbidden},
		{storage.ErrInvalid, ErrInvalidRequest, http.StatusBadRequest},
		{errors.New("backend fault"), ErrInternalError, http.StatusInternalServerError},
	} {
		got := MetadataWriteError(tc.err)
		if got.Code != tc.code || got.Status() != tc.status {
			t.Fatalf("%v: %+v", tc.err, got)
		}
	}
}
