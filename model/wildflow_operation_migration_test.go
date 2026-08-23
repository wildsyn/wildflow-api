package model

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type legacyWildFlowOperation struct {
	ID                   int64  `gorm:"primaryKey"`
	OperationID          string `gorm:"type:varchar(64);uniqueIndex"`
	UserID               int    `gorm:"uniqueIndex:idx_wildflow_operation_user_key,priority:1;index"`
	IdempotencyKeyDigest string `gorm:"type:char(64);uniqueIndex:idx_wildflow_operation_user_key,priority:2"`
	RequestDigest        string `gorm:"type:char(64)"`
	State                string `gorm:"type:varchar(32);index"`
}

func (legacyWildFlowOperation) TableName() string {
	return "wild_flow_operations"
}

func TestWildFlowContractMigrationExpandsLegacySQLiteRowsInPlace(t *testing.T) {
	database, err := gorm.Open(sqlite.Open("file:wildflow-contract-migration-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, sqlErr := database.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, database.AutoMigrate(&legacyWildFlowOperation{}))
	require.NoError(t, database.Create(&legacyWildFlowOperation{
		OperationID: "op-legacy", UserID: 7, IdempotencyKeyDigest: "legacy-key",
		RequestDigest: "legacy-request", State: "succeeded",
	}).Error)

	require.NoError(t, database.AutoMigrate(
		&WildFlowOperation{}, &WildFlowUsageEvent{}, &WildFlowBillingLogEntry{},
	))
	for _, column := range []string{
		"billing_usage_event_id", "result_json", "result_validated_time", "result_retention_seconds", "result_expires_at",
		"submission_phase", "submission_owner", "submission_lease_token", "submission_lease_expires_at", "submission_retry_until", "submission_attempt",
	} {
		assert.True(t, database.Migrator().HasColumn(&WildFlowOperation{}, column), column)
	}
	assert.True(t, database.Migrator().HasTable(&WildFlowBillingLogEntry{}))
	for _, column := range []string{
		"projection_state", "projection_attempts", "projection_last_error", "projection_claim_token",
		"projection_lease_expires_at", "projected_time",
	} {
		assert.True(t, database.Migrator().HasColumn(&WildFlowBillingLogEntry{}, column), column)
	}

	var operation WildFlowOperation
	require.NoError(t, database.Where("operation_id = ?", "op-legacy").First(&operation).Error)
	assert.Equal(t, "succeeded", operation.State)
	assert.Empty(t, operation.ResultJSON)
	assert.Zero(t, operation.ResultExpiresAt)
}
