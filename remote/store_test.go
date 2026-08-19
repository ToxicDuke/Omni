package remote

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/pterodactyl/wings/internal/models"
)

func newTestStateStore(t *testing.T) (*gormStateStore, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.PanelEndpointState{}, &models.PanelEndpointSwitch{}, &models.ServerConfigurationCache{}, &models.PanelEventOutbox{}); err != nil {
		t.Fatal(err)
	}
	return &gormStateStore{db: db}, db
}

func TestOutboxPreservesHeadOfLineOrdering(t *testing.T) {
	store, _ := newTestStateStore(t)
	ctx := context.Background()
	if err := store.Enqueue(ctx, "first", "POST", "/first", []byte(`{}`), "offline"); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(ctx, "second", "POST", "/second", []byte(`{}`), "offline"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOutboxFailed(ctx, 1, "still offline", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	events, err := store.DueOutbox(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("expected newer event to remain blocked, got %d events", len(events))
	}
}

func TestPruneServerConfigurations(t *testing.T) {
	store, _ := newTestStateStore(t)
	ctx := context.Background()
	for _, uuid := range []string{"keep", "remove"} {
		if err := store.SaveServerConfiguration(ctx, uuid, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PruneServerConfigurations(ctx, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	entries, err := store.LoadAllServerConfigurations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].UUID != "keep" {
		t.Fatalf("unexpected cached configurations: %#v", entries)
	}
}
