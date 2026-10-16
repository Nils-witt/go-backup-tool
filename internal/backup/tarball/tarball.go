// Package tarball streams a tar archive of a directory tree (or a single
// file) on this machine to an io.Writer, optionally gzip-compressed — the
// source behind a type: tar command (see config.CommandKindTar), written
// in-process so no tar binary is needed on any platform.
//
// It is a leaf package: it imports nothing from the rest of the program.
package tarball

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// Options configures one archive.
type Options struct {
	// Root is the file or directory to archive. Entries are named after its
	// base name, the way "tar -C parent base" names them. When Root itself
	// is a symlink, its target is archived under Root's own base name.
	Root string

	// Exclude are path.Match patterns. An entry is left out (a directory
	// with everything below it) when a pattern matches either its path
	// relative to Root, slash-separated, or its base name. Root itself is
	// never excluded.
	Exclude []string

	// Gzip compresses the archive.
	Gzip bool

	// OnSkip, when set, is told about every entry left out because tar
	// can't usefully hold it (sockets, devices, named pipes). Excluded
	// entries aren't reported.
	OnSkip func(name, reason string)
}

// ValidatePattern reports whether p is a well-formed Exclude pattern.
func ValidatePattern(p string) error {
	if _, err := path.Match(p, ""); err != nil {
		return fmt.Errorf("exclude pattern %q: %w", p, err)
	}

	return nil
}

// Write archives o.Root to w. It fails on the first entry it can't read
// (a backup missing files silently is worse than one that fails), and when
// a file shrinks while it's being read. Canceling ctx stops it before the
// next entry.
func Write(ctx context.Context, w io.Writer, o Options) (err error) {
	for _, p := range o.Exclude {
		if err := ValidatePattern(p); err != nil {
			return err
		}
	}

	root, err := filepath.EvalSymlinks(o.Root)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", o.Root, err)
	}

	out := w

	if o.Gzip {
		zw := gzip.NewWriter(w)
		defer func() { err = errors.Join(err, zw.Close()) }()

		out = zw
	}

	tw := tar.NewWriter(out)

	a := archiver{opts: o, root: root, base: filepath.Base(filepath.Clean(o.Root)), tw: tw}

	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		return a.add(p, d)
	}); err != nil {
		return err
	}

	// tar's Close writes the end-of-archive marker; it must precede gzip's.
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finishing archive: %w", err)
	}

	return nil
}

// archiver holds one Write call's state for its WalkDir callback.
type archiver struct {
	opts Options
	root string // o.Root with symlinks resolved: what's walked
	base string // entry name prefix: o.Root's own base name
	tw   *tar.Writer
}

// add writes the entry at p (found by walking a.root) to the archive.
func (a *archiver) add(p string, d fs.DirEntry) error {
	rel, err := filepath.Rel(a.root, p)
	if err != nil {
		return err
	}

	rel = filepath.ToSlash(rel)

	if rel != "." && a.excluded(rel) {
		if d.IsDir() {
			return fs.SkipDir
		}

		return nil
	}

	name := path.Join(a.base, rel)

	info, err := d.Info()
	if err != nil {
		return err
	}

	hdr, err := a.header(p, name, info)
	if err != nil || hdr == nil {
		return err
	}

	if err := a.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	return copyFile(a.tw, p, name, hdr.Size)
}

// header builds the tar header for the entry at p, archived as name. It
// returns a nil header, after reporting it to OnSkip, for a file type tar
// can't usefully hold.
func (a *archiver) header(p, name string, info fs.FileInfo) (*tar.Header, error) {
	link := ""

	switch mode := info.Mode(); {
	case mode.IsRegular(), mode.IsDir():
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return nil, err
		}

		link = target
	default:
		if a.opts.OnSkip != nil {
			a.opts.OnSkip(name, "unsupported file type "+mode.Type().String())
		}

		return nil, nil
	}

	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	hdr.Name = name
	if info.IsDir() {
		hdr.Name += "/"
	}

	return hdr, nil
}

// excluded reports whether rel, a slash-separated path relative to the
// root, matches one of the exclude patterns (already validated by Write).
func (a *archiver) excluded(rel string) bool {
	for _, p := range a.opts.Exclude {
		if ok, _ := path.Match(p, rel); ok {
			return true
		}

		if ok, _ := path.Match(p, path.Base(rel)); ok {
			return true
		}
	}

	return false
}

// copyFile writes exactly size bytes of the file at p into tw. A file
// that has grown since its header was written is cut at size; one that has
// shrunk is an error, since its archived content would be incomplete.
func copyFile(tw *tar.Writer, p, name string, size int64) error {
	f, err := os.Open(p) //nolint:gosec // p comes from walking the operator-configured archive root
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	if _, err := io.CopyN(tw, f, size); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: file shrank while being archived", name)
		}

		return fmt.Errorf("%s: %w", name, err)
	}

	return nil
}
