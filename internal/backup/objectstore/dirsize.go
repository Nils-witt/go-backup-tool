package objectstore

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// DirSize returns the total size of every regular file under root, which a
// receiver's quota: is checked against (see the receiver package). A root
// that doesn't exist yet holds nothing; a file that disappears mid-walk
// (e.g. swept by retention) is skipped.
func DirSize(root string) (int64, error) {
	var total int64

	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
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
		return 0, fmt.Errorf("measuring %q: %w", root, err)
	}

	return total, nil
}
