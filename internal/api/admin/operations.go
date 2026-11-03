package admin

import (
	"errors"
	"net/http"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/storage"
)

func (s *server) operational(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	if s.Operations == nil {
		return sUnavailable()
	}
	return nil
}

func jobID(c *call) (string, error) {
	id := c.r.PathValue("id")
	if !jobs.ValidID(id) {
		return "", invalid("invalid job/node identifier")
	}
	return id, nil
}

func (s *server) listJobs(c *call) error {
	if err := s.operational(c); err != nil {
		return err
	}
	p, q, err := s.page(c, "")
	if err != nil {
		return err
	}
	v, err := s.Operations.ListJobs(c.r.Context(), jobs.PageRequest{Limit: p.Limit, Cursor: p.After})
	if err != nil {
		return err
	}
	items := make([]jobs.Detail, 0, len(v.Jobs))
	for _, j := range v.Jobs {
		items = append(items, j.View())
	}
	return s.list(c, "", q, items, v.Next)
}

func (s *server) createJob(c *call) error {
	if err := s.operational(c); err != nil {
		return err
	}
	o, err := decodeObject(c.w, c.r, "type", "dry_run", "params", "policy_revision")
	if err != nil {
		return err
	}
	t, err := o.str("type", "", true, nil)
	if err != nil || !jobs.Type(t).Valid() {
		return invalid("unsupported job type")
	}
	dry, err := o.boolean("dry_run", false)
	if err != nil {
		return err
	}
	revision, err := o.integer("policy_revision", 0, 0, MaxRevision)
	if err != nil {
		return err
	}
	params := o["params"]
	if len(params) > 8192 {
		return invalid("params exceed 8 KiB")
	}
	if len(params) > 0 {
		if _, err = parseObject(params); err != nil {
			return err
		}
	}
	j, err := s.Operations.Create(c.r.Context(), jobs.Create{Type: jobs.Type(t), DryRun: dry, Params: params, PolicyRevision: revision})
	if err != nil {
		if errors.Is(err, jobs.ErrUnsupported) {
			return &apiError{http.StatusNotImplemented, "job_type_unavailable", "job executor is not installed"}
		}
		// Preserve the generated identity even if a lost ACK made create uncertain.
		if j.ID != "" {
			c.w.Header().Set("Location", "/admin/v1/jobs/"+j.ID)
		}
		return err
	}
	c.w.Header().Set("Location", "/admin/v1/jobs/"+j.ID)
	c.json(http.StatusAccepted, j.View())
	return nil
}

func (s *server) getJob(c *call) error    { return s.job(c, false) }
func (s *server) cancelJob(c *call) error { return s.job(c, true) }
func (s *server) job(c *call, cancel bool) error {
	if err := s.operational(c); err != nil {
		return err
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	id, err := jobID(c)
	if err != nil {
		return err
	}
	var j jobs.Job
	if cancel {
		j, err = s.Operations.Cancel(c.r.Context(), id)
	} else {
		j, err = s.Operations.GetJob(c.r.Context(), id)
	}
	if errors.Is(err, storage.ErrNotFound) {
		return notFound("job_not_found", "job does not exist")
	}
	if err != nil {
		return err
	}
	c.json(http.StatusOK, j.View())
	return nil
}

func (s *server) listNodes(c *call) error {
	if err := s.operational(c); err != nil {
		return err
	}
	p, q, err := s.page(c, "")
	if err != nil {
		return err
	}
	v, err := s.Operations.ListNodes(c.r.Context(), jobs.PageRequest{Limit: p.Limit, Cursor: p.After})
	if err != nil {
		return err
	}
	return s.list(c, "", q, v.Nodes, v.Next)
}

func (s *server) drainNode(c *call) error {
	if err := s.operational(c); err != nil {
		return err
	}
	id, err := jobID(c)
	if err != nil {
		return err
	}
	if _, err = decodeObject(c.w, c.r); err != nil {
		return err
	}
	j, err := s.Operations.Drain(c.r.Context(), id)
	if j.ID != "" {
		c.w.Header().Set("Location", "/admin/v1/jobs/"+j.ID)
	}
	if errors.Is(err, storage.ErrNotFound) {
		return notFound("node_not_found", "node does not exist")
	}
	if err != nil {
		return err
	}
	c.w.Header().Set("Location", "/admin/v1/jobs/"+j.ID)
	c.json(http.StatusAccepted, j.View())
	return nil
}
