//go:build !windows

package integration

import "os"

func atomicReplace(from, to string) error { return os.Rename(from, to) }
