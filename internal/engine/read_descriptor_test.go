package engine

import (
	"errors"
	"reflect"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestReadDescriptor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*storage.ObjectHead)
		invalid bool
	}{
		{"valid", func(*storage.ObjectHead) {}, false},
		{"pages", func(h *storage.ObjectHead) { h.PageCount++ }, true},
		{"chunks", func(h *storage.ObjectHead) { h.ChunkCount-- }, true},
		{"missing index", func(h *storage.ObjectHead) { h.PageIndex = nil }, true},
		{"wrong index", func(h *storage.ObjectHead) { h.PageIndex[1]++ }, true},
		{"outside requested range", func(h *storage.ObjectHead) { h.PageIndex[1] = ^uint64(0) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pages := readFixture()
			h.PageCount, h.ChunkCount, h.PageIndex = 2, 3, []uint64{0, 7}
			tc.mutate(&h)
			before := append([]uint64(nil), h.PageIndex...)
			p, err := PlanRead(h.Object, h, pages, &storage.ByteRange{Length: 1})
			if tc.invalid {
				if !errors.Is(err, ErrInvalidManifest) || !reflect.DeepEqual(p, ReadPlan{}) {
					t.Fatalf("partial/accepted plan: %+v %v", p, err)
				}
			} else if err != nil || !p.Validation.DescriptorVerified {
				t.Fatalf("%+v %v", p, err)
			}
			if !reflect.DeepEqual(before, h.PageIndex) {
				t.Fatal("mutated index")
			}
		})
	}
}

func TestReadDescriptorRejectsMissingTailDespiteMatchingObservedSize(t *testing.T) {
	h, pages := readFixture()
	h.PageCount, h.ChunkCount, h.PageIndex = 2, 3, []uint64{0, 7}
	h.Size = 7 // observed pages alone would look structurally complete
	p, err := PlanRead(h.Object, h, pages[:1], nil)
	if !errors.Is(err, ErrInvalidManifest) || !reflect.DeepEqual(p, ReadPlan{}) {
		t.Fatal(p, err)
	}
}
