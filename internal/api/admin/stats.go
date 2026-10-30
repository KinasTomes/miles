package admin

import (
	"errors"
	st "github.com/KinasTomes/miles/internal/storage"
	"net/http"
	"unicode/utf8"
)

func (s *server) stats(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	if s.Statistics == nil {
		return sUnavailable()
	}
	v, err := s.Statistics.Global(c.r.Context())
	if err != nil {
		return err
	}
	c.w.Header().Set("Cache-Control", "no-store")
	c.json(http.StatusOK, v)
	return nil
}
func (s *server) bucketStats(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	if s.Statistics == nil {
		return sUnavailable()
	}
	v, err := s.Statistics.Bucket(c.r.Context(), c.r.PathValue("b"))
	if errors.Is(err, st.ErrNotFound) || errors.Is(err, st.ErrBucketNotFound) {
		return errBucketNotFound
	}
	if err != nil {
		return err
	}
	c.w.Header().Set("Cache-Control", "no-store")
	c.json(http.StatusOK, v)
	return nil
}
func (s *server) layout(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	q, err := query(c.r, "bucket", "key")
	if err != nil {
		return err
	}
	b, k := q.Get("bucket"), q.Get("key")
	if b == "" || k == "" || !utf8.ValidString(b) || !utf8.ValidString(k) || len(k) > 1024 {
		return invalid("bucket and UTF-8 key are required (key <=1024 bytes)")
	}
	if s.Statistics == nil {
		return sUnavailable()
	}
	v, err := s.Statistics.Layout(c.r.Context(), b, k)
	if errors.Is(err, st.ErrBucketNotFound) {
		return errBucketNotFound
	}
	if errors.Is(err, st.ErrNotFound) {
		return notFound("object_not_found", "object does not exist")
	}
	if err != nil {
		return err
	}
	c.w.Header().Set("Cache-Control", "no-store")
	c.json(http.StatusOK, v)
	return nil
}
func sUnavailable() error {
	return &apiError{http.StatusServiceUnavailable, "unavailable", "storage measurement is not available"}
}
