package objectstore

import (
	"errors"
	"fmt"
	"io/fs"
)

// DirSize returns the total size of every regular file under base, which a
// receiver's quota: is checked against (see the receiver package), without
// following symlinks. A base that doesn't exist yet holds nothing; a file
// that disappears mid-walk (e.g. swept by retention) is skipped.
func DirSize(base string) (int64, error) {
	root, err := openRoot(base, false)
	if isNotExist(err) {
		return 0, nil
	}

	if err != nil {
		return 0, err
	}

	defer func() { _ = root.Close() }()

	var total int64

	err = fs.WalkDir(root.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		if !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		total += info.Size()

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measuring %q: %w", base, err)
	}

	return total, nil
}
