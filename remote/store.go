package remote

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/pterodactyl/wings/internal/models"
)

type stateStore interface {
	LoadActiveEndpoint(context.Context) (string, error)
	SaveEndpointSwitch(context.Context, string, string, string) error
	RecentEndpointSwitches(context.Context, int) ([]models.PanelEndpointSwitch, error)
	SaveServerConfiguration(context.Context, string, []byte) error
	LoadServerConfiguration(context.Context, string) ([]byte, time.Time, error)
	LoadAllServerConfigurations(context.Context) ([]models.ServerConfigurationCache, error)
	PruneServerConfigurations(context.Context, []string) error
	Enqueue(context.Context, string, string, string, []byte, string) error
	DueOutbox(context.Context, int) ([]models.PanelEventOutbox, error)
	MarkOutboxDelivered(context.Context, uint) error
	MarkOutboxFailed(context.Context, uint, string, time.Time) error
	OutboxStats(context.Context) (int64, time.Time, error)
	CacheStats(context.Context) (int64, time.Time, error)
}

type gormStateStore struct{ db *gorm.DB }

// NewStateStore creates the persistence adapter used by the resilient client.
func NewStateStore(db *gorm.DB) stateStore { return &gormStateStore{db: db} }

func (s *gormStateStore) LoadActiveEndpoint(ctx context.Context) (string, error) {
	var state models.PanelEndpointState
	err := s.db.WithContext(ctx).First(&state, 1).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	return state.ActiveName, err
}

func (s *gormStateStore) SaveEndpointSwitch(ctx context.Context, from, to, reason string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state := models.PanelEndpointState{ID: 1, ActiveName: to, UpdatedAt: time.Now().UTC()}
		if err := tx.Save(&state).Error; err != nil {
			return err
		}
		return tx.Create(&models.PanelEndpointSwitch{From: from, To: to, Reason: reason, CreatedAt: time.Now().UTC()}).Error
	})
}

func (s *gormStateStore) RecentEndpointSwitches(ctx context.Context, limit int) ([]models.PanelEndpointSwitch, error) {
	var switches []models.PanelEndpointSwitch
	err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&switches).Error
	return switches, err
}

func (s *gormStateStore) SaveServerConfiguration(ctx context.Context, uuid string, payload []byte) error {
	entry := models.ServerConfigurationCache{UUID: uuid, Payload: payload, UpdatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Save(&entry).Error
}

func (s *gormStateStore) LoadServerConfiguration(ctx context.Context, uuid string) ([]byte, time.Time, error) {
	var entry models.ServerConfigurationCache
	err := s.db.WithContext(ctx).First(&entry, "uuid = ?", uuid).Error
	return entry.Payload, entry.UpdatedAt, err
}

func (s *gormStateStore) LoadAllServerConfigurations(ctx context.Context) ([]models.ServerConfigurationCache, error) {
	var entries []models.ServerConfigurationCache
	err := s.db.WithContext(ctx).Order("uuid ASC").Find(&entries).Error
	return entries, err
}

func (s *gormStateStore) PruneServerConfigurations(ctx context.Context, uuids []string) error {
	query := s.db.WithContext(ctx)
	if len(uuids) == 0 {
		return query.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&models.ServerConfigurationCache{}).Error
	}
	return query.Where("uuid NOT IN ?", uuids).Delete(&models.ServerConfigurationCache{}).Error
}

func (s *gormStateStore) Enqueue(ctx context.Context, kind, method, path string, payload []byte, lastError string) error {
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Create(&models.PanelEventOutbox{
		Kind: kind, Method: method, Path: path, Payload: payload, LastError: lastError,
		NextAttempt: now, CreatedAt: now, UpdatedAt: now,
	}).Error
}

func (s *gormStateStore) DueOutbox(ctx context.Context, limit int) ([]models.PanelEventOutbox, error) {
	var events []models.PanelEventOutbox
	err := s.db.WithContext(ctx).Where("next_attempt <= ?", time.Now().UTC()).Order("id ASC").Limit(limit).Find(&events).Error
	return events, err
}

func (s *gormStateStore) MarkOutboxDelivered(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Delete(&models.PanelEventOutbox{}, id).Error
}

func (s *gormStateStore) MarkOutboxFailed(ctx context.Context, id uint, message string, next time.Time) error {
	return s.db.WithContext(ctx).Model(&models.PanelEventOutbox{}).Where("id = ?", id).Updates(map[string]interface{}{
		"attempts": gorm.Expr("attempts + 1"), "last_error": message, "next_attempt": next.UTC(), "updated_at": time.Now().UTC(),
	}).Error
}

func (s *gormStateStore) OutboxStats(ctx context.Context) (int64, time.Time, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.PanelEventOutbox{}).Count(&count).Error; err != nil || count == 0 {
		return count, time.Time{}, err
	}
	var oldest models.PanelEventOutbox
	err := s.db.WithContext(ctx).Order("created_at ASC").First(&oldest).Error
	return count, oldest.CreatedAt, err
}

func (s *gormStateStore) CacheStats(ctx context.Context) (int64, time.Time, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.ServerConfigurationCache{}).Count(&count).Error; err != nil || count == 0 {
		return count, time.Time{}, err
	}
	var oldest models.ServerConfigurationCache
	err := s.db.WithContext(ctx).Order("updated_at ASC").First(&oldest).Error
	return count, oldest.UpdatedAt, err
}
