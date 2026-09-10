// Package tasklog defines the retained task-output format shared by producers
// and readers. Event payloads retain their separate stream and line fields.
package tasklog

import "strings"

const ErrorPrefix = "E: "

// FormatLine formats a logical output line without adding its final newline.
func FormatLine(stream, line string) string {
	if stream != "stderr" {
		return line
	}
	// Adapter callbacks may emit multiple physical lines in one event.
	return ErrorPrefix + strings.ReplaceAll(line, "\n", "\n"+ErrorPrefix)
}
