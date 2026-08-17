package model

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var (
	ErrLegacyOIDCUserNotFound    = errors.New("legacy OIDC user not found")
	ErrLegacyOIDCUserNotEligible = errors.New("legacy OIDC user is not eligible")
)

// BindOIDCIdentity atomically assigns one OIDC subject to one active user.
func BindOIDCIdentity(userID int, subject string) (*User, error) {
	subject = strings.TrimSpace(subject)
	if userID == 0 || subject == "" {
		return nil, ErrExternalIdentityAlreadyClaimed
	}
	var user User
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrLegacyOIDCUserNotEligible
		}
		if user.OidcId != "" && user.OidcId != subject {
			return ErrExternalIdentityAlreadyClaimed
		}
		if err := ClaimExternalIdentityWithTx(tx, ExternalIdentityProviderOIDC, subject, user.Id); err != nil {
			return err
		}
		if err := tx.Model(&User{}).Where("id = ?", user.Id).Update("oidc_id", subject).Error; err != nil {
			return err
		}
		user.OidcId = subject
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := updateUserCache(user); err != nil {
		return nil, err
	}
	return &user, nil
}

// BindLegacyOIDCUser consumes only the Authentik-issued legacy_username claim.
// preferred_username and email are never used to select the migration target.
func BindLegacyOIDCUser(username, subject, verifiedEmail string) (*User, error) {
	username = strings.TrimSpace(username)
	subject = strings.TrimSpace(subject)
	verifiedEmail = NormalizeEmail(verifiedEmail)
	if username == "" || subject == "" || verifiedEmail == "" || len(verifiedEmail) > 50 ||
		strings.ContainsAny(verifiedEmail, "\r\n") {
		return nil, ErrLegacyOIDCUserNotEligible
	}

	var user User
	err := DB.Transaction(func(tx *gorm.DB) error {
		return withNormalizedEmailLock(tx, verifiedEmail, func(tx *gorm.DB) error {
			result := lockForUpdate(tx).Unscoped().Where("username = ?", username).First(&user)
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return ErrLegacyOIDCUserNotFound
			}
			if result.Error != nil {
				return result.Error
			}
			if user.DeletedAt.Valid || user.Status != common.UserStatusEnabled || user.Password == "" {
				return ErrLegacyOIDCUserNotEligible
			}
			if user.OidcId != "" && user.OidcId != subject {
				return ErrExternalIdentityAlreadyClaimed
			}
			if err := ensureEmailAvailableWithTx(tx, verifiedEmail, user.Id); err != nil {
				return err
			}
			if err := ClaimExternalIdentityWithTx(tx, ExternalIdentityProviderOIDC, subject, user.Id); err != nil {
				return err
			}
			if err := tx.Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{
				"email":   verifiedEmail,
				"oidc_id": subject,
			}).Error; err != nil {
				return err
			}
			user.Email = verifiedEmail
			user.OidcId = subject
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if err := updateUserCache(user); err != nil {
		return nil, err
	}
	return &user, nil
}
