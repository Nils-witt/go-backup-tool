package config

import (
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// resolvedNotifications is what resolveNotifications derives from the
// config file: the resolved smtp:/gpg settings every email notification
// uses, and its (deprecated) notifications: entries, validated.
type resolvedNotifications struct {
	smtp notify.SMTPSettings
	gpg  notify.GPGSettings
	byID map[string]notify.Notification
}

// resolveNotifications resolves fileCfg's top-level smtp:/notifications:
// entries (see notify.ResolveSMTP/notify.Build) — split out of ParseFlags to
// keep its own cyclomatic complexity down. Encrypted notification emails use
// the top-level gpg-bin/gpg-homedir, the same gpg and keyring as backups.
func resolveNotifications(fileCfg *fileConfig) (resolvedNotifications, error) {
	smtp, err := notify.ResolveSMTP(fileCfg.SMTP)
	if err != nil {
		return resolvedNotifications{}, err
	}

	gpg := notify.GPGSettings{Bin: DefaultGPGBin}
	applyString(&gpg.Bin, fileCfg.GPGBin)
	applyString(&gpg.Homedir, fileCfg.GPGHomedir)

	byID, err := notify.Build(fileCfg.Notifications, smtp, gpg)
	if err != nil {
		return resolvedNotifications{}, err
	}

	return resolvedNotifications{smtp: smtp, gpg: gpg, byID: byID}, nil
}
