package storage

import "context"

// ObservationStore is a trusted, read-only admin projection. It grants no S3
// permissions and is NOT a cross-table snapshot or a GC eligibility proof.
type ObservationStore interface {
	ObservePage(context.Context, ObservationRequest) (ObservationPage, error)
}
type ObservationRequest struct {
	Kind, Cursor string
	Limit        int
}
type ObservationPage struct {
	Buckets []Bucket
	Heads   []ObjectHead
	Packs   []PackInfo
	Next    string
}

func (r ObservationRequest) Validate() error {
	if r.Limit < 1 || r.Limit > 256 || len(r.Cursor) > 4096 {
		return ErrInvalid
	}
	switch r.Kind {
	case "buckets", "heads", "packs":
		return nil
	}
	return ErrInvalid
}

type PhysicalObject struct {
	Key, VersionID, Category string
	Size                     int64
	Current                  bool
	Pack                     PackRef
	// Nil means no verified index: only canonical payload can be attributed;
	// unknown structural/dead overhead stays in the global pool.
	PayloadBytes *int64
}
type PhysicalInventory struct {
	Objects      []PhysicalObject
	Complete     bool
	VersionScope string
}
type PhysicalInventorySource interface {
	CollectInventory(context.Context, int) (PhysicalInventory, error)
}
