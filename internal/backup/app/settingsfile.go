package app

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/settingsfile"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// settingsFileUser is recorded as created_by/updated_by on settings changed
// by -import-settings.
const settingsFileUser = "settings file"

// runSettingsFile handles -export-settings/-import-settings: it loads the
// web UI-managed settings from stateDB through their managers, writes or
// applies the settings file, and returns the process exit code. It needs
// the state db, so a run without one fails.
func runSettingsFile(ctx context.Context, rc *config.RunConfig, stateDB store.Store, log *slog.Logger) int {
	if rc.ExportSettings != "" && rc.ImportSettings != "" {
		log.Error("-export-settings and -import-settings can't be combined")
		return 2
	}

	if stateDB == nil {
		log.Error("settings file: the state db couldn't be opened (see above)")
		return 1
	}

	notifications, _, settingsManager := newSettings(ctx, rc, stateDB, log)
	_, _, receiverManager, trustManager := newReceivers(ctx, rc, stateDB, notifications, log)
	m := settingsfile.Managers{Trust: trustManager, Settings: settingsManager, Receivers: receiverManager}

	if rc.ExportSettings != "" {
		if err := settingsfile.ExportFile(ctx, m, rc.ExportSettings); err != nil {
			log.Error("exporting settings", "err", err)
			return 1
		}

		log.Info("settings exported", "path", rc.ExportSettings)

		return 0
	}

	res, err := settingsfile.ImportFile(ctx, m, settingsFileUser, rc.ImportSettings)
	logImportResult(log, res)

	if err != nil {
		log.Error("importing settings", "path", rc.ImportSettings, "err", err)
		return 1
	}

	log.Info("settings imported", "path", rc.ImportSettings)

	return 0
}

// logImportResult logs how many entries of each section an import created
// and updated, including when it stopped early.
func logImportResult(log *slog.Logger, res settingsfile.Result) {
	sections := slices.Sorted(maps.Keys(res.Created))
	for _, s := range slices.Sorted(maps.Keys(res.Updated)) {
		if !slices.Contains(sections, s) {
			sections = append(sections, s)
		}
	}

	for _, s := range sections {
		log.Info("settings import", "section", s, "created", res.Created[s], "updated", res.Updated[s])
	}
}
