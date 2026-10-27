package storage

import "fmt"

func (c Chunk) Validate() error {
	if err := c.Key.Validate(); err != nil {
		return err
	}
	if c.Version != ModelVersion || c.RawBytes <= 0 || c.RawBytes > MaxObjectBytes || c.LocationRevision == 0 {
		return fmt.Errorf("%w: chunk version/size/revision", ErrInvalid)
	}
	return c.Location.Validate()
}
func (l ChunkLocation) Validate() error {
	if err := l.Pack.Validate(); err != nil {
		return err
	}
	if l.Range.Offset < 0 || l.Range.Length <= 0 || (l.Codec != "raw" && l.Codec != "zstd") {
		return fmt.Errorf("%w: chunk location", ErrInvalid)
	}
	return nil
}
func (p ManifestPage) Validate() error {
	if p.Version != ModelVersion || p.ManifestID == "" || !identifier.MatchString(p.Domain) {
		return fmt.Errorf("%w: manifest identity/version", ErrInvalid)
	}
	for _, ref := range p.Chunks {
		if err := ref.Key.Validate(); err != nil {
			return err
		}
		if ref.Key.Domain != p.Domain || ref.RawBytes <= 0 || ref.RawBytes > MaxObjectBytes || ref.LogicalOffset < 0 || ref.LogicalOffset > MaxObjectBytes-ref.RawBytes {
			return fmt.Errorf("%w: manifest chunk/domain/offset", ErrInvalid)
		}
	}
	return nil
}
func (k ObjectKey) Validate() error {
	if k.Bucket == "" || !identifier.MatchString(k.BucketID) || k.Key == "" {
		return fmt.Errorf("%w: object/bucket identity", ErrInvalid)
	}
	return nil
}
func (r CommitRequest) Validate() error {
	h := r.Head
	if err := h.Object.Validate(); err != nil {
		return err
	}
	if h.Version != ModelVersion || h.Revision != 0 || h.OpID == "" || h.Size < 0 || h.Size > MaxObjectBytes || !identifier.MatchString(h.Domain) {
		return fmt.Errorf("%w: head version/revision/identity/size", ErrInvalid)
	}
	if h.Deleted {
		if h.Size != 0 || h.ManifestID != "" || h.PageCount != 0 || h.ChunkCount != 0 || len(h.PageIndex) != 0 ||
			h.ETag != "" || h.ETagKind != "" || h.Checksum != (Checksum{}) || len(h.Metadata) != 0 ||
			r.Receipt != nil || r.Completion != nil {
			return fmt.Errorf("%w: tombstone payload", ErrInvalid)
		}
	} else if h.ManifestID == "" || h.ETag == "" {
		return fmt.Errorf("%w: manifest/etag", ErrInvalid)
	}
	if r.UserID == "" || r.CredentialID == "" || r.ReservationID == "" || r.ReservationRevision == 0 {
		return fmt.Errorf("%w: commit authorization/reservation", ErrInvalid)
	}
	e := r.Event
	if e.Validate() != nil || e.OpID != h.OpID || e.Entity != "Objects" || e.Object != h.Object || e.Revision != r.ExpectedRevision+1 {
		return fmt.Errorf("%w: authoritative outbox event", ErrInvalid)
	}
	if (r.Receipt == nil) != (r.Completion == nil) {
		return fmt.Errorf("%w: receipt/completion pair", ErrInvalid)
	}
	if r.Receipt != nil {
		p, u := r.Receipt, r.Completion
		if p.Version != ModelVersion || p.UploadID == "" || p.UploadID != u.UploadID || p.BucketID != h.Object.BucketID || p.Key != h.Object.Key || p.Initiator != r.UserID || p.OpID != h.OpID || p.ManifestID != h.ManifestID || p.ETag != h.ETag || p.Checksum != h.Checksum || !sha256Hex.MatchString(p.InputDigest) || p.ExpiresAt.IsZero() || u.State != "Completing" || u.Revision == 0 || u.Fence == 0 || u.BucketID != p.BucketID || u.Key != p.Key || u.Initiator != p.Initiator {
			return fmt.Errorf("%w: completion/receipt binding", ErrInvalid)
		}
	}
	return nil
}

// Validate checks the shared durable event envelope, not the referenced payload.
func (e OutboxEvent) Validate() error {
	if e.Version != ModelVersion || e.ID == "" || e.HistoryID == "" || e.OpID == "" || e.Revision == 0 || e.PayloadPointer == "" || !sha256Hex.MatchString(e.PayloadSHA256) {
		return fmt.Errorf("%w: authoritative outbox event", ErrInvalid)
	}
	id, err := EventID(e.HistoryID, e.OpID, e.Entity, e.Action)
	if err != nil || e.ID != id {
		return fmt.Errorf("%w: outbox event identity", ErrInvalid)
	}
	switch e.Entity {
	case "Objects":
		if e.EntityID != "" {
			return ErrInvalid
		}
		return e.Object.Validate()
	case "SystemConfig":
		if e.EntityID == "global-policy" && e.Object == (ObjectKey{}) {
			return nil
		}
	}
	return ErrInvalid
}
