package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/tarball"
)

// JobKind distinguishes a job's type:.
type JobKind string

// The JobKind values a job's type: can resolve to.
const (
	// JobKindBackup (type: backup, or unset) runs the job's files:
	// commands and uploads their encrypted output as one backup.
	JobKindBackup JobKind = "backup"

	// JobKindOnChange (type: on-change) uploads every file under its
	// source.path that changed since its last upload, each encrypted on its
	// own, to directory receivers (see ReceiverKindDirectory).
	JobKindOnChange JobKind = "on-change"
)

// DefaultOnChangeDebounce is how long an on-change job's watcher waits
// after the last filesystem event before uploading, when source.debounce:
// is unset.
const DefaultOnChangeDebounce = 5 * time.Second

// VersionDirSuffix is the reserved suffix of the directory a directory
// receiver keeps one path's versions in (see objectstore.WriteFileVersion).
// An on-change job skips source paths with a segment ending in it, and a
// receiver rejects them.
const VersionDirSuffix = ".gbtv"

// FileJobSource is an on-change job's source: entry: the directory tree
// whose files it uploads.
type FileJobSource struct {
	// Path is the absolute directory to back up.
	Path string `yaml:"path" json:"path"`

	// Exclude are path.Match patterns, matched like a tar command's (see
	// tarball.Options.Exclude): against a file's slash-separated path
	// relative to Path, or its base name. A matching directory is skipped
	// with everything below it.
	Exclude []string `yaml:"exclude,omitempty" json:"exclude"`

	// Watch uploads changed files as they change, using filesystem events,
	// on top of the full scans interval: runs.
	Watch bool `yaml:"watch,omitempty" json:"watch"`

	// Debounce is how long the watcher waits after the last event before
	// uploading, e.g. "10s". Defaults to DefaultOnChangeDebounce.
	Debounce string `yaml:"debounce,omitempty" json:"debounce"`
}

// OnChangeSource is a FileJobSource, resolved.
type OnChangeSource struct {
	Path     string
	Exclude  []string
	Watch    bool
	Debounce time.Duration
}

// Excluded reports whether rel, a slash-separated path relative to
// s.Path, matches one of s's exclude patterns, the same way a tar
// command's excludes match.
func (s OnChangeSource) Excluded(rel string) bool {
	return tarball.Excluded(s.Exclude, rel)
}

// parseJobKind validates a FileJob's Type field.
func parseJobKind(t string) (JobKind, error) {
	switch strings.TrimSpace(t) {
	case "", string(JobKindBackup):
		return JobKindBackup, nil
	case string(JobKindOnChange):
		return JobKindOnChange, nil
	default:
		return "", fmt.Errorf("unknown type %q (want %q or %q)", t, JobKindBackup, JobKindOnChange)
	}
}

// resolveOnChangeSource validates fs into an OnChangeSource.
func resolveOnChangeSource(fs *FileJobSource) (OnChangeSource, error) {
	if fs == nil || strings.TrimSpace(fs.Path) == "" {
		return OnChangeSource{}, errors.New("source.path is required")
	}

	p := filepath.Clean(strings.TrimSpace(fs.Path))
	if !filepath.IsAbs(p) {
		return OnChangeSource{}, fmt.Errorf("source.path %q must be absolute", fs.Path)
	}

	for _, pattern := range fs.Exclude {
		if err := tarball.ValidatePattern(pattern); err != nil {
			return OnChangeSource{}, fmt.Errorf("source.exclude: %w", err)
		}
	}

	debounce := DefaultOnChangeDebounce

	if s := strings.TrimSpace(fs.Debounce); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return OnChangeSource{}, fmt.Errorf("parsing source.debounce %q: %w", fs.Debounce, err)
		}

		if d <= 0 {
			return OnChangeSource{}, fmt.Errorf("source.debounce must be positive, got %q", fs.Debounce)
		}

		debounce = d
	}

	return OnChangeSource{
		Path:     p,
		Exclude:  slices.Clone(fs.Exclude),
		Watch:    fs.Watch,
		Debounce: debounce,
	}, nil
}

// checkOnChangeJob is checkJob for an on-change job.
func checkOnChangeJob(cfg *Config) error {
	switch {
	case cfg.Source.Path == "":
		return errors.New("source.path is required for an on-change job")
	case len(cfg.fileRefs) > 0:
		return errors.New("files is not valid for an on-change job (it uploads source.path)")
	case len(cfg.Targets) == 0:
		return errors.New("at least one target is required (see targets: and servers:)")
	case len(cfg.Recipients) == 0:
		return errors.New("specify at least one recipient")
	case cfg.Interval < 0:
		return errors.New("interval must not be negative")
	case !cfg.StartTime.IsZero() && cfg.Interval <= 0:
		return errors.New("start-time requires interval")
	case !cfg.Source.Watch && cfg.Interval <= 0:
		return errors.New("an on-change job needs source.watch, an interval, or both")
	}

	for i, t := range cfg.Targets {
		if t.Kind != ServerKindRemote {
			return fmt.Errorf("targets[%d]: server %q is type %s; an on-change job uploads only to remote (directory) receivers", i, t.ServerName, t.Kind)
		}
	}

	return nil
}
