package recovery

import (
	"fmt"

	s "github.com/KinasTomes/miles/internal/storage"
)

// These schemas exercise bounded history/epoch/catalog binding only. An
// Inventory is NOT a seal, completion marker or supported recovery target.
type Catalog struct {
	Genesis     Reference `json:"genesis"`
	History     string    `json:"history"`
	Incarnation string    `json:"incarnation"`
	Version     int       `json:"r2_version"`
}
type Member struct {
	Descriptor Reference `json:"descriptor"`
	ID         string    `json:"id"`
}
type Inventory struct {
	Epoch   uint64   `json:"epoch"`
	Events  []Member `json:"events"`
	History string   `json:"history"`
	Version int      `json:"r2_version"`
	State   string   `json:"state"`
}

func (r Reference) Validate() error {
	if r.Length <= 0 || r.Length > MaxMetadataBytes || !isHex(r.SHA256, 64) {
		return s.ErrInvalid
	}
	return nil
}
func (c Catalog) Validate() error {
	if c.Version != 1 || !isHex(c.History, 32) || !isHex(c.Incarnation, 32) || c.Genesis.Validate() != nil {
		return s.ErrInvalid
	}
	return nil
}

// ValidateExact requires the authoritative Closed epoch's stable, completely
// enumerated IDs. A count, scan timeout or digest alone is not exact membership.
// The caller supplies the independently obtained closure proof, never a GSI.
func (v Inventory) ValidateExact(history string, epoch uint64, expected []string) error {
	if v.Version != 1 || v.State != "Closed" || !isHex(history, 32) || v.History != history || v.Epoch != epoch || epoch > maxInteger || v.Events == nil || len(v.Events) > MaxFanout || len(v.Events) != len(expected) {
		return s.ErrInvalid
	}
	previous := ""
	for i, m := range v.Events {
		if !isHex(m.ID, 64) || m.ID <= previous || m.ID != expected[i] || m.Descriptor.Validate() != nil {
			return fmt.Errorf("%w: exact epoch membership", s.ErrInvalid)
		}
		previous = m.ID
	}
	return nil
}
