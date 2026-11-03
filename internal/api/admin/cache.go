package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/storage"
)

type CacheStatistics interface {
	CacheStats(context.Context, string, string) (jobs.CacheReport, error)
}

func cacheScope(scope, node string) error {
	if scope == "cluster" && node == "" || scope == "node" && jobs.ValidID(node) {
		return nil
	}
	return invalid("scope must be node with node_id, or cluster without node_id")
}
func (s *server) cacheStats(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	q, err := query(c.r, "scope", "node_id")
	if err != nil {
		return err
	}
	scope, node := q.Get("scope"), q.Get("node_id")
	if err = cacheScope(scope, node); err != nil {
		return err
	}
	if s.CacheStatistics == nil {
		return sUnavailable()
	}
	out, err := s.CacheStatistics.CacheStats(c.r.Context(), scope, node)
	if errors.Is(err, storage.ErrNotFound) {
		return notFound("node_not_found", "node does not exist")
	}
	if err != nil {
		return err
	}
	c.json(http.StatusOK, out)
	return nil
}
func (s *server) purgeCache(c *call) error {
	if err := s.operational(c); err != nil {
		return err
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	o, err := decodeObject(c.w, c.r, "scope", "node_id")
	if err != nil {
		return err
	}
	scope, err := o.str("scope", "", true, nil)
	if err != nil {
		return err
	}
	node, err := o.str("node_id", "", false, nil)
	if err != nil {
		return err
	}
	if err = cacheScope(scope, node); err != nil {
		return err
	}
	params, _ := json.Marshal(map[string]string{"scope": scope, "node_id": node})
	ctx, err := s.operationalMutation(c)
	if err != nil {
		return err
	}
	j, err := s.Operations.Create(ctx, jobs.Create{Type: "cache_purge", Params: params})
	if j.ID != "" {
		c.w.Header().Set("Location", "/admin/v1/jobs/"+j.ID)
	}
	if errors.Is(err, jobs.ErrNodeNotFound) {
		return notFound("node_not_found", "node does not exist")
	}
	if errors.Is(err, jobs.ErrTooManyTargets) {
		return invalid("cluster supports at most 64 active targets")
	}
	if errors.Is(err, jobs.ErrUnsupported) {
		return &apiError{http.StatusNotImplemented, "job_type_unavailable", "cache purge executor is not installed"}
	}
	if err != nil {
		return err
	}
	c.json(http.StatusAccepted, j.View())
	return nil
}
