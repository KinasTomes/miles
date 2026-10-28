package app

import (
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/storage"
	"testing"
)

func TestExplicitDevAndRuntimeUnavailable(t *testing.T) {
	c := config.DefaultStartup()
	runtime, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(runtime.Probe(context.Background()), ErrRuntimeUnavailable) || runtime.Metadata != nil {
		t.Fatal("runtime must not silently use fixtures")
	}
	c.Mode = "dev"
	dev, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	if dev.Metadata == nil || dev.Packs == nil || dev.Cache == nil {
		t.Fatal("shared ports not wired")
	}
	if err := dev.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := dev.Packs.DeletePack(context.Background(), storage.PackRef{ID: "dev-probe", Generation: "1"}); !errors.Is(err, coordination.ErrMaintenanceDisabled) {
		t.Fatal("MVP must block destructive lifecycle", err)
	}
	if err := dev.Packs.ChangeStorageClass(context.Background(), storage.PackRef{ID: "dev-probe", Generation: "1"}, storage.Glacier); !errors.Is(err, coordination.ErrMaintenanceDisabled) {
		t.Fatal("MVP must block tiering", err)
	}
	if err := dev.Probe(context.Background()); err != nil {
		t.Fatal("rejected maintenance changed origin", err)
	}
}
