package remote

import (
	"context"
	"testing"
	"time"

	"github.com/pterodactyl/wings/internal/models"
)

type memoryStateStore struct {
	active   string
	switches []models.PanelEndpointSwitch
	configs  map[string][]byte
	outbox   []models.PanelEventOutbox
}

func (s *memoryStateStore) LoadActiveEndpoint(context.Context) (string, error) { return s.active, nil }
func (s *memoryStateStore) SaveEndpointSwitch(_ context.Context, from, to, reason string) error {
	s.active = to
	s.switches = append(s.switches, models.PanelEndpointSwitch{From: from, To: to, Reason: reason})
	return nil
}
func (s *memoryStateStore) RecentEndpointSwitches(context.Context, int) ([]models.PanelEndpointSwitch, error) {
	return append([]models.PanelEndpointSwitch(nil), s.switches...), nil
}
func (s *memoryStateStore) SaveServerConfiguration(_ context.Context, uuid string, payload []byte) error {
	if s.configs == nil {
		s.configs = map[string][]byte{}
	}
	s.configs[uuid] = append([]byte(nil), payload...)
	return nil
}
func (s *memoryStateStore) LoadServerConfiguration(_ context.Context, uuid string) ([]byte, time.Time, error) {
	return append([]byte(nil), s.configs[uuid]...), time.Now(), nil
}
func (s *memoryStateStore) LoadAllServerConfigurations(context.Context) ([]models.ServerConfigurationCache, error) {
	entries := make([]models.ServerConfigurationCache, 0, len(s.configs))
	for uuid, payload := range s.configs {
		entries = append(entries, models.ServerConfigurationCache{UUID: uuid, Payload: payload})
	}
	return entries, nil
}
func (s *memoryStateStore) PruneServerConfigurations(_ context.Context, uuids []string) error {
	keep := make(map[string]struct{}, len(uuids))
	for _, uuid := range uuids {
		keep[uuid] = struct{}{}
	}
	for uuid := range s.configs {
		if _, ok := keep[uuid]; !ok {
			delete(s.configs, uuid)
		}
	}
	return nil
}
func (s *memoryStateStore) Enqueue(_ context.Context, kind, method, path string, payload []byte, lastError string) error {
	s.outbox = append(s.outbox, models.PanelEventOutbox{ID: uint(len(s.outbox) + 1), Kind: kind, Method: method, Path: path, Payload: payload, LastError: lastError})
	return nil
}
func (s *memoryStateStore) DueOutbox(context.Context, int) ([]models.PanelEventOutbox, error) {
	return append([]models.PanelEventOutbox(nil), s.outbox...), nil
}
func (s *memoryStateStore) MarkOutboxDelivered(_ context.Context, id uint) error {
	for i := range s.outbox {
		if s.outbox[i].ID == id {
			s.outbox = append(s.outbox[:i], s.outbox[i+1:]...)
			break
		}
	}
	return nil
}
func (s *memoryStateStore) MarkOutboxFailed(context.Context, uint, string, time.Time) error {
	return nil
}
func (s *memoryStateStore) OutboxStats(context.Context) (int64, time.Time, error) {
	return int64(len(s.outbox)), time.Time{}, nil
}
func (s *memoryStateStore) CacheStats(context.Context) (int64, time.Time, error) {
	return int64(len(s.configs)), time.Time{}, nil
}

func TestRestoreActiveEndpoint(t *testing.T) {
	store := &memoryStateStore{active: "secondary"}
	c := NewWithEndpoints([]Endpoint{{Name: "primary", URL: "https://one.example.com"}, {Name: "secondary", URL: "https://two.example.com"}}, WithStateStore(store)).(*client)
	c.restoreActiveEndpoint(context.Background())
	if got := c.activeEndpoint().Name; got != "secondary" {
		t.Fatalf("expected restored secondary endpoint, got %q", got)
	}
}

func TestHealthFailureAndPreferredFailback(t *testing.T) {
	store := &memoryStateStore{}
	c := NewWithEndpoints(
		[]Endpoint{{Name: "primary", URL: "https://one.example.com"}, {Name: "secondary", URL: "https://two.example.com"}},
		WithStateStore(store), WithFailureThreshold(2), WithRecoveryThreshold(2), WithSwitchCooldown(0),
	).(*client)

	c.recordHealthResult(0, false)
	c.recordHealthResult(0, false)
	if got := c.activeEndpoint().Name; got != "secondary" {
		t.Fatalf("expected failover to secondary, got %q", got)
	}
	c.recordHealthResult(0, true)
	c.recordHealthResult(0, true)
	if got := c.activeEndpoint().Name; got != "primary" {
		t.Fatalf("expected failback to primary, got %q", got)
	}
	if len(store.switches) != 2 {
		t.Fatalf("expected two persisted switches, got %d", len(store.switches))
	}
}
