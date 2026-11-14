package recovery

import (
	"context"
	"errors"
	"reflect"
)

// VerifyReplay walks the sealed chain and rebuilds twice from the SAME genesis,
// comparing canonical roots and rebuilt quota/head/refcount records. Starting
// the second build at its target checkpoint would skip replay entirely.
// This does not install or mutate the running metadata namespace.
func VerifyReplay(ctx context.Context, objects ImmutableStore, catalog Catalog, target uint64) (AttachmentReference, AttachmentReference, int, error) {
	var zero AttachmentReference
	if _, err := VerifyCut(ctx, objects, catalog, target); err != nil {
		return zero, zero, 0, err
	}
	w := Checkpointer{Objects: objects, Catalog: catalog}
	base := AttachmentReference{Key: genesisKey(catalog), Reference: catalog.Genesis}
	a, err := w.Build(ctx, base, target)
	if err != nil {
		return zero, zero, 0, err
	}
	b, err := w.Build(ctx, base, target)
	if err != nil {
		return a, zero, 0, err
	}
	if a != b {
		return a, b, 0, errors.New("checkpoint replay changed canonical root")
	}
	first, err := w.Rebuild(ctx, a)
	if err != nil {
		return a, b, 0, err
	}
	second, err := w.Rebuild(ctx, b)
	if err != nil {
		return a, b, 0, err
	}
	if len(first.Records) == 0 || !reflect.DeepEqual(first, second) {
		return a, b, 0, errors.New("replayed namespace/quota differs")
	}
	return a, b, len(first.Records), nil
}
