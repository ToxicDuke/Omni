package models

import "time"

// PanelEndpointState stores the endpoint selected by Omni across restarts.
type PanelEndpointState struct {
	ID         uint      `gorm:"primaryKey"`
	ActiveName string    `gorm:"not null"`
	UpdatedAt  time.Time `gorm:"not null"`
}

// PanelEndpointSwitch is an append-only audit record for endpoint changes.
type PanelEndpointSwitch struct {
	ID        uint      `gorm:"primaryKey"`
	From      string    `gorm:"column:from_endpoint"`
	To        string    `gorm:"column:to_endpoint;not null"`
	Reason    string    `gorm:"not null"`
	CreatedAt time.Time `gorm:"not null;index"`
}

// ServerConfigurationCache stores the last configuration successfully read
// from the logical Panel control plane.
type ServerConfigurationCache struct {
	UUID      string    `gorm:"primaryKey;size:36"`
	Payload   []byte    `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null;index"`
}

// PanelEventOutbox stores typed Panel events until they are acknowledged.
type PanelEventOutbox struct {
	ID          uint      `gorm:"primaryKey"`
	Kind        string    `gorm:"not null;index"`
	Method      string    `gorm:"not null"`
	Path        string    `gorm:"not null"`
	Payload     []byte    `gorm:"not null"`
	Attempts    int       `gorm:"not null"`
	LastError   string    `gorm:"not null"`
	NextAttempt time.Time `gorm:"not null;index"`
	CreatedAt   time.Time `gorm:"not null;index"`
	UpdatedAt   time.Time `gorm:"not null"`
}
