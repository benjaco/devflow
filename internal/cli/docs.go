package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	docsusers "github.com/benjaco/devflow/docs_users"
)

func writeUserDocs(w io.Writer) error {
	entries, err := docsusers.Files.ReadDir(".")
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		paths = append(paths, entry.Name())
	}
	sort.Strings(paths)
	for i, path := range paths {
		data, err := docsusers.Files.ReadFile(path)
		if err != nil {
			return err
		}
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "<!-- docs_users/%s -->\n\n", path); err != nil {
			return err
		}
		text := strings.TrimRight(string(data), "\n")
		if _, err := fmt.Fprintln(w, text); err != nil {
			return err
		}
	}
	return nil
}
