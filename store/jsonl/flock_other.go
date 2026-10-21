//go:build !unix

package jsonl

import "os"

// lock is a no-op where flock is unavailable; the store's mutex still serializes writers in this process, and seq checks catch another writer.
func lock(*os.File, bool) error { return nil }

func unlock(*os.File) {}
