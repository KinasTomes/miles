package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/KinasTomes/miles/internal/faults"
)

type createFaultRequest struct {
	Type     string  `json:"type"`
	Scope    string  `json:"scope"`
	NodeID   string  `json:"node_id,omitempty"`
	Rate     float64 `json:"rate"`
	Duration int64   `json:"duration_seconds"`
}

func (s *server) listFaults(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	if s.Faults == nil {
		c.json(http.StatusOK, []faults.FaultRule{})
		return nil
	}
	rules, err := s.Faults.List(s.now())
	if err != nil {
		if errors.Is(err, faults.ErrDisabled) {
			return invalid("fault injection is disabled on this gateway")
		}
		return err
	}
	if rules == nil {
		rules = []faults.FaultRule{}
	}
	c.w.Header().Set("Cache-Control", "no-store")
	c.json(http.StatusOK, rules)
	return nil
}

func (s *server) createFault(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	if s.Faults == nil {
		return invalid("fault injection is disabled on this gateway")
	}
	var req createFaultRequest
	if err := json.NewDecoder(c.r.Body).Decode(&req); err != nil {
		return invalid("invalid request body: %v", err)
	}

	if s.NodeID != "" && req.NodeID != s.NodeID {
		return invalid("node_id %q does not match target gateway node identity %q; target gateway directly", req.NodeID, s.NodeID)
	}

	rule := faults.FaultRule{
		Type:     faults.FaultType(req.Type),
		Scope:    faults.FaultScope(req.Scope),
		NodeID:   req.NodeID,
		Rate:     req.Rate,
		Duration: req.Duration,
	}

	created, err := s.Faults.Add(s.now(), rule)
	if err != nil {
		if errors.Is(err, faults.ErrDisabled) {
			return invalid("fault injection is disabled on this gateway")
		}
		return invalid("%v", err)
	}

	c.json(http.StatusCreated, created)
	return nil
}

func (s *server) deleteFault(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	if s.Faults == nil {
		return invalid("fault injection is disabled on this gateway")
	}
	id := c.r.PathValue("id")
	if id == "" {
		return invalid("fault id is required")
	}

	err := s.Faults.Delete(id)
	if err != nil {
		if errors.Is(err, faults.ErrNotFound) {
			return notFound("fault_not_found", "fault rule not found")
		}
		if errors.Is(err, faults.ErrDisabled) {
			return invalid("fault injection is disabled on this gateway")
		}
		return err
	}

	c.w.WriteHeader(http.StatusNoContent)
	return nil
}
