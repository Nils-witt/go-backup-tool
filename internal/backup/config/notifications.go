package config

import (
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// resolvedNotifications is what resolveNotifications derives from the
// config file: the resolved smtp:/gpg settings every email notification
// uses. The notifications themselves are managed in the web UI.
type resolvedNotifications struct {
	smtp notify.SMTPSettings
	gpg  notify.GPGSettings
}

// resolveNotifications resolves fileCfg's top-level smtp: entry (see
// notify.ResolveSMTP) — split out of ParseFlags to keep its own cyclomatic
// complexity down. Encrypted notification emails use
// the top-level gpg-bin/gpg-homedir, the same gpg and keyring as backups.
func resolveNotifications(fileCfg *fileConfig) (resolvedNotifications, error) {
	smtp, err := notify.ResolveSMTP(fileCfg.SMTP)
	if err != nil {
		return resolvedNotifications{}, err
	}

	gpg := notify.GPGSettings{Bin: DefaultGPGBin}
	applyString(&gpg.Bin, fileCfg.GPGBin)
	applyString(&gpg.Homedir, fileCfg.GPGHomedir)

	return resolvedNotifications{smtp: smtp, gpg: gpg}, nil
}
