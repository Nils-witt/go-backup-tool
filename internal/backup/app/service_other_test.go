//go:build !windows

package app

import (
	"bytes"
	"testing"
)

func TestMainIsRunOnNonWindows(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	// -h is handled entirely by parseFlags before any job runs, so this
	// exercises Main's plumbing without needing a config file.
	if got := Main([]string{"-h"}, &buf); got != 0 {
		t.Errorf("Main([-h]) = %d, want 0", got)
	}

	if buf.Len() == 0 {
		t.Error("Main([-h]) wrote no usage output")
	}
}
