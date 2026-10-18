package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/fsnotify/fsnotify"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// maxPendingChanges is how many changed paths a watcher collects before it
// gives up on listing them and asks for a full scan instead.
const maxPendingChanges = 10000

// maxDebounceFactor bounds how long a watcher holds changes back while
// events keep coming: at most this many debounce periods after the first.
const maxDebounceFactor = 10

// flushFunc hands a watcher's batch of changed paths (slash-separated,
// relative to the source; nil for "rescan everything") to the job. ran is
// false when the batch couldn't run yet (the job is busy) and should be
// offered again later; stop ends the watch.
type flushFunc func(changed []string) (ran, stop bool)

// sourceWatcher watches an on-change job's source tree (see watchSource).
type sourceWatcher struct {
	src config.OnChangeSource
	w   *fsnotify.Watcher
	log *slog.Logger

	pending map[string]struct{}
	full    bool      // rescan everything instead of pending
	first   time.Time // when the oldest unflushed change arrived
}

// watchSource watches every directory under src.Path (other than excluded
// ones) for changes, and hands them to flush in batches: once no event has
// arrived for src.Debounce, or maxDebounceFactor debounce periods after the
// batch's first event while events keep coming. Missed events (the OS
// queue overflowing) or too many changes turn the batch into a full scan.
// It returns once ctx is done or flush says stop, or with an error when
// src.Path can't be watched at all.
func watchSource(ctx context.Context, src config.OnChangeSource, log *slog.Logger, flush flushFunc) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("starting file watcher: %w", err)
	}
	defer func() { _ = w.Close() }()

	sw := &sourceWatcher{src: src, w: w, log: log, pending: map[string]struct{}{}}

	if err := w.Add(src.Path); err != nil {
		return fmt.Errorf("watching %s: %w", src.Path, err)
	}

	sw.addTree(src.Path)

	timer := time.NewTimer(time.Hour)
	timer.Stop()

	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}

			if sw.handle(ev) {
				timer.Reset(sw.delay(time.Now()))
			}

		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}

			sw.handleError(err)
			timer.Reset(sw.delay(time.Now()))

		case <-timer.C:
			if stop := sw.flush(flush); stop {
				return nil
			}

			if sw.hasPending() {
				timer.Reset(src.Debounce)
			}
		}
	}
}

// handle records ev, reporting whether it's a change to back up. A new
// directory is watched too, along with everything already in it.
func (sw *sourceWatcher) handle(ev fsnotify.Event) bool {
	if ev.Op == fsnotify.Chmod {
		return false
	}

	rel, err := filepath.Rel(sw.src.Path, ev.Name)
	if err != nil || rel == "." {
		return false
	}

	rel = filepath.ToSlash(rel)
	if excludedPath(sw.src, rel) {
		return false
	}

	if ev.Has(fsnotify.Create) {
		sw.addTree(ev.Name)
	}

	sw.add(rel)

	return true
}

// handleError turns the pending batch into a full scan after the watcher
// failed, e.g. missed events.
func (sw *sourceWatcher) handleError(err error) {
	if errors.Is(err, fsnotify.ErrEventOverflow) {
		sw.log.Warn("on-change: missed file events, rescanning everything")
	} else {
		sw.log.Warn("on-change: file watcher error, rescanning everything", "error", err)
	}

	sw.markFull()
}

// add records rel as changed.
func (sw *sourceWatcher) add(rel string) {
	if !sw.hasPending() {
		sw.first = time.Now()
	}

	if sw.full {
		return
	}

	sw.pending[rel] = struct{}{}

	if len(sw.pending) > maxPendingChanges {
		sw.markFull()
	}
}

// markFull turns the pending batch into a full scan.
func (sw *sourceWatcher) markFull() {
	if !sw.hasPending() {
		sw.first = time.Now()
	}

	sw.full = true
	clear(sw.pending)
}

func (sw *sourceWatcher) hasPending() bool {
	return sw.full || len(sw.pending) > 0
}

// delay is how long to wait, as of now, before flushing: src.Debounce, but
// no later than maxDebounceFactor of them after the batch's first change.
func (sw *sourceWatcher) delay(now time.Time) time.Duration {
	deadline := sw.first.Add(maxDebounceFactor * sw.src.Debounce)

	return max(min(sw.src.Debounce, deadline.Sub(now)), 0)
}

// flush hands the pending batch to flush, keeping it when it couldn't run
// yet, and reports whether to stop watching.
func (sw *sourceWatcher) flush(flush flushFunc) (stop bool) {
	if !sw.hasPending() {
		return false
	}

	var changed []string

	if !sw.full {
		changed = make([]string, 0, len(sw.pending))
		for rel := range sw.pending {
			changed = append(changed, rel)
		}

		slices.Sort(changed)
	}

	ran, stop := flush(changed)
	if ran {
		sw.full = false
		clear(sw.pending)
	}

	return stop
}

// addTree watches dir and every directory below it that isn't excluded. A
// directory that can't be watched (e.g. the OS limit on watches is reached)
// is logged and skipped; full scans still cover it.
func (sw *sourceWatcher) addTree(dir string) {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || !d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, as the scan does
		}

		if p != sw.src.Path {
			rel, err := filepath.Rel(sw.src.Path, p)
			if err != nil || excludedPath(sw.src, filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}

			if err := sw.w.Add(p); err != nil {
				sw.log.Warn("on-change: can't watch directory; it's still covered by full scans", "path", p, "error", err)
				return filepath.SkipDir
			}
		}

		return nil
	})
	if err != nil {
		sw.log.Warn("on-change: walking new directory", "path", dir, "error", err)
	}
}
