package model

import (
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestBindLegacyOIDCUserUsesExplicitLegacyUsername(t *testing.T) {
	truncateTables(t)

	legacy := User{
		Username: "legacy-owner",
		Password: "legacy-password-hash",
		Email:    "",
		Status:   common.UserStatusEnabled,
		AffCode:  "legacy-owner",
	}
	require.NoError(t, DB.Create(&legacy).Error)

	bound, err := BindLegacyOIDCUser("legacy-owner", "authentik-subject", "NEW@Example.com")
	require.NoError(t, err)
	assert.Equal(t, legacy.Id, bound.Id)
	assert.Equal(t, "new@example.com", bound.Email)
	assert.Equal(t, "authentik-subject", bound.OidcId)

	var claim ExternalIdentityClaim
	require.NoError(t, DB.Where("provider = ? AND subject = ?", ExternalIdentityProviderOIDC, "authentik-subject").First(&claim).Error)
	assert.Equal(t, legacy.Id, claim.UserId)
}

func TestBindLegacyOIDCUserRejectsEmailCollisionAndExistingBinding(t *testing.T) {
	truncateTables(t)

	legacy := User{
		Username: "legacy-owner",
		Password: "legacy-password-hash",
		Status:   common.UserStatusEnabled,
		AffCode:  "legacy-owner",
	}
	other := User{
		Username: "email-owner",
		Password: "password-hash",
		Email:    "claimed@example.com",
		Status:   common.UserStatusEnabled,
		AffCode:  "email-owner",
	}
	require.NoError(t, DB.Create(&legacy).Error)
	require.NoError(t, DB.Create(&other).Error)

	_, err := BindLegacyOIDCUser("legacy-owner", "authentik-subject", "CLAIMED@example.com")
	assert.ErrorIs(t, err, ErrEmailAlreadyTaken)

	require.NoError(t, DB.Model(&legacy).Update("oidc_id", "existing-subject").Error)
	_, err = BindLegacyOIDCUser("legacy-owner", "different-subject", "new@example.com")
	assert.ErrorIs(t, err, ErrExternalIdentityAlreadyClaimed)
}

func TestInitializeExternalIdentityClaimsBackfillsOIDC(t *testing.T) {
	truncateTables(t)

	user := User{
		Username: "oidc-legacy",
		Password: "password-hash",
		OidcId:   "existing-oidc-subject",
		AffCode:  "oidc-legacy",
	}
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, InitializeExternalIdentityClaims())

	var claim ExternalIdentityClaim
	require.NoError(t, DB.Where("provider = ? AND subject = ?", ExternalIdentityProviderOIDC, user.OidcId).First(&claim).Error)
	assert.Equal(t, user.Id, claim.UserId)
}

func TestSessionOIDCBindingPreservesClaimAndRollback(t *testing.T) {
	truncateTables(t)
	setupUserSessionTest(t)
	createUserSessionTestUser(t, 9101, 1)
	createUserSessionTestUser(t, 9102, 1)
	session := newTestUserSession("oidc-binding-session", 9101, time.Now().Unix())
	require.NoError(t, CreateUserSession(session))
	identity := AuthSessionIdentity{UserID: 9101, SessionID: session.SID, UserAuthVersion: 1, SessionVersion: 1}
	rollback := errors.New("rollback flow consumption")
	err := DB.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, BindOIDCIdentityForSessionWithTx(tx, identity, "subject-rollback"))
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	var count int64
	require.NoError(t, DB.Model(&ExternalIdentityClaim{}).Where("subject = ?", "subject-rollback").Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		return BindOIDCIdentityForSessionWithTx(tx, identity, "subject-owner")
	}))
	_, err = BindOIDCIdentity(9102, "subject-owner")
	require.ErrorIs(t, err, ErrExternalIdentityAlreadyClaimed)
	_, err = RevokeUserSession(9101, session.SID, "test")
	require.NoError(t, err)
	err = DB.Transaction(func(tx *gorm.DB) error {
		return BindOIDCIdentityForSessionWithTx(tx, identity, "subject-after-revoke")
	})
	require.ErrorIs(t, err, ErrUserSessionInactive)
}
