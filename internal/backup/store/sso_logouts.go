package store

import (
	"context"
	"fmt"
	"time"
)

// ssoLogoutModel is sso_logouts: every OIDC back-channel logout the web UI
// has accepted from its provider (see internal/backup/webui/backchannel.go)
// and not yet forgotten. Access tokens are verified statelessly, so a
// logout can only take effect by remembering which session (SessionID, the
// "sid" claim) or user (Subject, the "sub" claim) it ended until every
// access token issued before it has expired. Either of SessionID and
// Subject may be empty, but not both.
type ssoLogoutModel struct {
	ID          uint      `gorm:"column:id;primaryKey;autoIncrement"`
	Issuer      string    `gorm:"column:issuer;not null"`
	Subject     string    `gorm:"column:subject;not null;default:''"`
	SessionID   string    `gorm:"column:session_id;not null;default:''"`
	LoggedOutAt time.Time `gorm:"column:logged_out_at;not null"`
	ExpiresAt   time.Time `gorm:"column:expires_at;not null;index"`
}

func (ssoLogoutModel) TableName() string { return "sso_logouts" }

// SSOLogout is one accepted OIDC back-channel logout (see SaveSSOLogout).
// It applies until ExpiresAt, after which every access token it could
// still reject has expired anyway.
type SSOLogout struct {
	Issuer      string
	Subject     string
	SessionID   string
	LoggedOutAt time.Time
	ExpiresAt   time.Time
}

// SaveSSOLogout records an accepted back-channel logout.
func (s *gormStore) SaveSSOLogout(ctx context.Context, l SSOLogout) error {
	m := ssoLogoutModel{
		Issuer: l.Issuer, Subject: l.Subject, SessionID: l.SessionID,
		LoggedOutAt: l.LoggedOutAt.UTC(), ExpiresAt: l.ExpiresAt.UTC(),
	}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("recording sso logout: %w", err)
	}

	return nil
}

// ListSSOLogouts returns every back-channel logout from issuer that is
// still in effect at now, oldest first.
func (s *gormStore) ListSSOLogouts(ctx context.Context, issuer string, now time.Time) ([]SSOLogout, error) {
	var rows []ssoLogoutModel

	err := s.read.WithContext(ctx).
		Where("issuer = ? AND expires_at > ?", issuer, now.UTC()).
		Order("logged_out_at").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading sso logouts: %w", err)
	}

	logouts := make([]SSOLogout, len(rows))
	for i, m := range rows {
		logouts[i] = SSOLogout{
			Issuer: m.Issuer, Subject: m.Subject, SessionID: m.SessionID,
			LoggedOutAt: m.LoggedOutAt, ExpiresAt: m.ExpiresAt,
		}
	}

	return logouts, nil
}

// DeleteExpiredSSOLogouts removes every back-channel logout no longer in
// effect at now.
func (s *gormStore) DeleteExpiredSSOLogouts(ctx context.Context, now time.Time) error {
	if err := s.db.WithContext(ctx).Where("expires_at <= ?", now.UTC()).Delete(&ssoLogoutModel{}).Error; err != nil {
		return fmt.Errorf("deleting expired sso logouts: %w", err)
	}

	return nil
}
