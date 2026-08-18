//go:build !windows

package jsonutil

import "os"

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
