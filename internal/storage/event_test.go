package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestEventIDFrozenIdentity(t *testing.T) {
	history, mutation := strings.Repeat("1", 32), strings.Repeat("2", 32)
	id, err := EventID(history, mutation, "Objects", "object_commit")
	if err != nil || id != "8ad1cfa9e409f3f3a1304885a8773fde9c800e38498c4dda3bee4e00af874710" {
		t.Fatalf("frozen event identity: %q %v", id, err)
	}
	if next, err := EventID(history, mutation, "SystemConfig", "entity_put"); err != nil || next == id {
		t.Fatalf("entity/action domain separation: %q %v", next, err)
	}
	for _, bad := range []struct{ history, mutation, entity, action string }{
		{history + "0", mutation, "Objects", "object_commit"},
		{history, strings.ToUpper(strings.Repeat("a", 32)), "Objects", "object_commit"},
		{history, mutation, "Objects", "entity_put"},
	} {
		if _, err := EventID(bad.history, bad.mutation, bad.entity, bad.action); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted invalid event identity: %+v %v", bad, err)
		}
	}
}
