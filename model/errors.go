package model

import "errors"

// Common errors
var (
	ErrDatabase = errors.New("database error")
)

// User auth errors
var (
	ErrInvalidCredentials   = errors.New("invalid credentials")
	ErrUserEmptyCredentials = errors.New("empty credentials")
	ErrEmailAlreadyTaken    = errors.New("email already taken")
	ErrEmailNotFound        = errors.New("email not found")
	ErrEmailAmbiguous       = errors.New("email matches multiple users")
)

// Token auth errors
var (
	ErrTokenNotProvided = errors.New("token not provided")
	ErrTokenInvalid     = errors.New("token invalid")
	// ErrTokenNotFound reports that the key does not match any token. It is
	// intentionally a separate sentinel so relay callers can collapse it back
	// into the generic ErrTokenInvalid message and avoid revealing whether a
	// key exists; detailed status errors below are only returned for keys the
	// caller already demonstrated possession of.
	ErrTokenNotFound = errors.New("token not found")
	// Detailed rejection reasons for keys whose existence has been proven.
	// Callers may surface these as stable machine codes and human messages.
	ErrTokenExpired        = errors.New("token expired")
	ErrTokenDisabled       = errors.New("token disabled")
	ErrTokenQuotaExhausted = errors.New("token quota exhausted")
	ErrTokenStatusChanged  = errors.New("token status changed concurrently")
)

// Redemption errors
var ErrRedeemFailed = errors.New("redeem.failed")

// 2FA errors
var ErrTwoFANotEnabled = errors.New("2fa not enabled")
var ErrTwoFAAlreadyEnabled = errors.New("2fa already enabled")
