package sqliteutil

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// ChmodFiles applies mode to a SQLite database and existing WAL/SHM sidecars.
func ChmodFiles(path string, mode os.FileMode) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := path + suffix
		if err := os.Chmod(p, mode); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("chmod %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

func FileURI(path, rawQuery string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: rawQuery}).String()
}
