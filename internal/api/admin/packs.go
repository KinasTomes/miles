package admin

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
	"math/big"
	"net/http"
	"regexp"
	"strings"
)

type PackInventory interface {
	Collect(context.Context) ([]lifecycle.Pack, map[s.PackRef][]s.Chunk, error)
}

var liveRatioPattern = regexp.MustCompile(`^(0(\.[0-9]{1,18})?|1(\.0{1,18})?)$`)

func packID(c *call) (string, error) {
	id := c.r.PathValue("id")
	if (s.PackRef{ID: id, Generation: "validate"}).Validate() != nil {
		return "", invalid("invalid pack identifier")
	}
	return id, nil
}
func (s *server) listPacks(c *call) error { return s.packs(c, false) }
func (s *server) showPack(c *call) error  { return s.packs(c, true) }
func (s *server) packs(c *call, show bool) error {
	if e := c.admin(); e != nil {
		return e
	}
	filters := []string{"state", "storage_class", "live_ratio_lt"}
	if show {
		filters = nil
	}
	page, q, e := s.page(c, "", filters...)
	if e != nil {
		return e
	}
	id := ""
	if show {
		id, e = packID(c)
		if e != nil {
			return e
		}
	}
	state := q.Get("state")
	class := q.Get("storage_class")
	ratio := q.Get("live_ratio_lt")
	if state != "" && !strings.Contains("/Uploading/Warm/Cold/Restoring/Restored/Compacting/Transitioning/Retiring/Retired/Deleting/Deleted/", "/"+state+"/") {
		return invalid("invalid pack state")
	}
	if class != "" && sclass(class).Validate() != nil {
		return invalid("invalid storage_class")
	}
	var threshold *big.Rat
	if ratio != "" {
		if !liveRatioPattern.MatchString(ratio) {
			return invalid("live_ratio_lt must be decimal in 0..1")
		}
		threshold, _ = new(big.Rat).SetString(ratio)
	}
	if s.PackInventory == nil {
		return sUnavailable()
	}
	records, _, e := s.PackInventory.Collect(c.r.Context())
	if e != nil {
		return e
	}
	items := []lifecycle.Pack{}
	found := false
	next := ""
	for _, p := range records {
		if show && p.PackID != id {
			continue
		}
		found = true
		key := p.PackID + "\x00" + p.Generation
		if key <= page.After || state != "" && p.State != state || class != "" && string(p.StorageClass) != class {
			continue
		}
		if threshold != nil && new(big.Rat).SetFrac64(p.LiveBytes, p.Bytes).Cmp(threshold) >= 0 {
			continue
		}
		if len(items) == page.Limit {
			last := items[len(items)-1]
			next = last.PackID + "\x00" + last.Generation
			break
		}
		items = append(items, p)
	}
	if show && !found {
		return notFound("pack_not_found", "pack does not exist")
	}
	c.w.Header().Set("Cache-Control", "no-store")
	return s.list(c, "", q, items, next)
}
func sclass(v string) s.StorageClass        { return s.StorageClass(v) }
func (s *server) restorePack(c *call) error { return s.packJob(c, "pack_restore") }
func (s *server) promotePack(c *call) error { return s.packJob(c, "pack_promote") }
func (s *server) packJob(c *call, kind jobs.Type) error {
	if e := s.operational(c); e != nil {
		return e
	}
	if _, e := query(c.r); e != nil {
		return e
	}
	id, e := packID(c)
	if e != nil {
		return e
	}
	ref := sref(id, c.r.PathValue("generation"))
	if ref.Validate() != nil {
		return invalid("invalid generation")
	}
	allowed := []string{}
	if kind == "pack_restore" {
		allowed = []string{"days", "tier"}
	}
	o, e := decodeObject(c.w, c.r, allowed...)
	if e != nil {
		return e
	}
	p := lifecycle.Params{PackID: ref.ID, Generation: ref.Generation}
	if kind == "pack_restore" {
		days, e := o.integer("days", 1, 1, 30)
		if e != nil {
			return e
		}
		p.Days = int(days)
		p.Tier, e = o.str("tier", "Standard", false, nil)
		if e != nil {
			return e
		}
	}
	if s.PackInventory == nil {
		return sUnavailable()
	}
	rows, _, e := s.PackInventory.Collect(c.r.Context())
	if e != nil {
		return e
	}
	found := false
	for _, row := range rows {
		if row.PackID == ref.ID && row.Generation == ref.Generation {
			found = true
		}
	}
	if !found {
		return notFound("pack_not_found", "pack generation does not exist")
	}
	if e = p.Validate(kind); e != nil {
		return e
	}
	ctx, e := s.operationalMutation(c)
	if e != nil {
		return e
	}
	params, _ := json.Marshal(p)
	j, e := s.Operations.Create(ctx, jobs.Create{Type: kind, Params: params})
	if j.ID != "" {
		c.w.Header().Set("Location", "/admin/v1/jobs/"+j.ID)
	}
	if errors.Is(e, jobs.ErrUnsupported) {
		return &apiError{http.StatusNotImplemented, "job_type_unavailable", "lifecycle executor is not installed"}
	}
	if e != nil {
		return e
	}
	c.json(http.StatusAccepted, j.View())
	return nil
}
func sref(id, g string) s.PackRef { return s.PackRef{ID: id, Generation: g} }
