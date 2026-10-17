package version

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleConfigNamesRemovalRelease keeps the docs in step with
// DeprecatedConfigRemoval: every config.example.yaml comment paragraph that
// marks something DEPRECATED, and every README line calling something
// deprecated, must name the release that removes it. Bumping the constant
// without updating the docs fails here.
func TestExampleConfigNamesRemovalRelease(t *testing.T) {
	t.Parallel()

	example := readRepoFile(t, "config.example.yaml")

	var paragraphs []string

	var current strings.Builder

	for line := range strings.SplitSeq(example, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") || trimmed == "#" {
			paragraphs = append(paragraphs, current.String())
			current.Reset()

			continue
		}

		current.WriteString(trimmed + "\n")
	}

	paragraphs = append(paragraphs, current.String())

	marked := 0

	for _, p := range paragraphs {
		if !strings.Contains(p, "DEPRECATED") {
			continue
		}

		marked++

		if !strings.Contains(p, DeprecatedConfigRemoval) {
			t.Errorf("config.example.yaml: paragraph marks something DEPRECATED without naming %s:\n%s", DeprecatedConfigRemoval, p)
		}
	}

	if marked == 0 {
		t.Fatal("config.example.yaml: no DEPRECATED paragraphs found; has the marker wording changed?")
	}

	for i, line := range strings.Split(readRepoFile(t, "README.md"), "\n") {
		if strings.Contains(strings.ToLower(line), "deprecated") && !strings.Contains(line, DeprecatedConfigRemoval) {
			t.Errorf("README.md:%d: mentions a deprecation without naming %s", i+1, DeprecatedConfigRemoval)
		}
	}
}

// readRepoFile reads name from the repository root, two levels above this
// package.
func readRepoFile(t *testing.T, name string) string {
	t.Helper()

	data, err := fs.ReadFile(os.DirFS(filepath.Join("..", "..")), name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}

	return string(data)
}
