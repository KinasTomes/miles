package app

import (
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/config"
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
	if err := dev.Packs.DeletePack(context.Background(), storage.PackRef{ID: "dev-probe", Generation: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := dev.Probe(context.Background()); err == nil {
		t.Fatal("backend loss was hidden")
	}
}
