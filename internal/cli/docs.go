package cli

import (
	"fmt"
	"io"
	"strings"

	docsusers "github.com/benjaco/devflow/docs_users"
)

type docsBundle string

const (
	docsBundleSetup       docsBundle = "setup"
	docsBundleDevelopment docsBundle = "development"
)

var docsBundleFiles = map[docsBundle][]string{
	docsBundleSetup:       {"setup.md", "adapter-guide.md"},
	docsBundleDevelopment: {"development.md", "agent-integration.md"},
}

func writeUserDocs(w io.Writer, bundle docsBundle) error {
	paths, ok := docsBundleFiles[bundle]
	if !ok {
		return fmt.Errorf("unknown docs bundle %q", bundle)
	}
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
