package storage

import "fmt"

const MaxManifestPages = 1024

// ValidateManifestDescriptor checks a fully prepared or persisted descriptor.
// Unlike CommitRequest's compatibility path, it does not derive omitted fields.
// Authorization, canonical dependencies and publication still belong to backend.
func (h ObjectHead) ValidateManifestDescriptor() error {
	if err := h.Object.Validate(); err != nil {
		return err
	}
	if h.Version != ModelVersion || !identifier.MatchString(h.Domain) || h.Size < 0 || h.Size > MaxObjectBytes || h.PageCount > MaxManifestPages {
		return ErrInvalid
	}
	if h.Deleted {
		if h.Size != 0 || h.ManifestID != "" || h.PageCount != 0 || h.ChunkCount != 0 || len(h.PageIndex) != 0 {
			return fmt.Errorf("%w: tombstone descriptor", ErrInvalid)
		}
		return nil
	}
	if !identifier.MatchString(h.ManifestID) || len(h.PageIndex) != int(h.PageCount) {
		return fmt.Errorf("%w: manifest descriptor", ErrInvalid)
	}
	if h.Size == 0 {
		if h.PageCount != 0 || h.ChunkCount != 0 {
			return ErrInvalid
		}
		return nil
	}
	if h.PageCount == 0 || h.ChunkCount < uint64(h.PageCount) || h.ChunkCount > uint64(h.Size) {
		return ErrInvalid
	}
	for i, offset := range h.PageIndex {
		if offset >= uint64(h.Size) || (i == 0 && offset != 0) || (i > 0 && offset <= h.PageIndex[i-1]) {
			return fmt.Errorf("%w: page index", ErrInvalid)
		}
	}
	return nil
}
