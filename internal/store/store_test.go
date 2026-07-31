package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/model"
)

func TestObserveAndBind(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id := model.Identity{SourceType: "pve", SourceID: "site-a", ExternalID: "qemu:105"}
	if err := s.Observe(context.Background(), model.Snapshot{
		Identity: id, Name: "gpu", ResourceType: "qemu", CollectedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Binding(context.Background(), id); !errors.Is(err, ErrNotBound) {
		t.Fatalf("Binding() error = %v, want ErrNotBound", err)
	}

	want := model.Binding{Identity: id, KomariUUID: "uuid-1", Token: "secret"}
	if err := s.Bind(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Binding(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Binding() = %#v, want %#v", got, want)
	}
}
