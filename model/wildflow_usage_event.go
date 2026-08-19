package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrWildFlowUsageEventConflict = errors.New("WildFlow usage event identity conflict")
	ErrWildFlowUsageEventUnknown  = errors.New("WildFlow usage event does not match an operation")
)

type WildFlowUsageEvent struct {
	EventID         string    `json:"-" gorm:"type:varchar(128);primaryKey"`
	AggregateType   string    `json:"-" gorm:"type:varchar(32)"`
	AggregateID     string    `json:"-" gorm:"type:varchar(128);index"`
	EventType       string    `json:"-" gorm:"type:varchar(64)"`
	PayloadDigest   string    `json:"-" gorm:"type:char(64)"`
	OperationID     string    `json:"-" gorm:"type:varchar(64);index"`
	JobID           string    `json:"-" gorm:"type:varchar(128);index"`
	AttemptID       string    `json:"-" gorm:"type:varchar(128)"`
	ModelVersionRef string    `json:"-" gorm:"type:varchar(256)"`
	ChannelType     string    `json:"-" gorm:"type:varchar(32)"`
	Kind            string    `json:"-" gorm:"type:varchar(32)"`
	Quantity        int64     `json:"-"`
	Unit            string    `json:"-" gorm:"type:varchar(32)"`
	StartedAt       time.Time `json:"-"`
	EndedAt         time.Time `json:"-"`
	EvidenceRef     string    `json:"-" gorm:"type:varchar(256)"`
	CreatedTime     int64     `json:"-" gorm:"bigint"`
}

func (event *WildFlowUsageEvent) BeforeCreate(_ *gorm.DB) error {
	if event.CreatedTime == 0 {
		event.CreatedTime = time.Now().Unix()
	}
	return nil
}

func RecordWildFlowUsageEvent(event *WildFlowUsageEvent) (bool, error) {
	if DB == nil || event == nil {
		return false, errors.New("WildFlow usage event database is unavailable")
	}
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing WildFlowUsageEvent
		err := tx.Where("event_id = ?", event.EventID).First(&existing).Error
		if err == nil {
			if existing.PayloadDigest != event.PayloadDigest {
				return ErrWildFlowUsageEventConflict
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var operation WildFlowOperation
		if err := tx.Where("operation_id = ?", event.OperationID).First(&operation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrWildFlowUsageEventUnknown
			}
			return err
		}
		if operation.JobID != event.JobID || operation.ModelVersionRef != event.ModelVersionRef {
			return ErrWildFlowUsageEventConflict
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(event)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var raced WildFlowUsageEvent
			if lookupErr := tx.Where("event_id = ?", event.EventID).First(&raced).Error; lookupErr != nil {
				return lookupErr
			}
			if raced.PayloadDigest != event.PayloadDigest {
				return ErrWildFlowUsageEventConflict
			}
			replayed = true
		}
		return nil
	})
	return replayed, err
}
