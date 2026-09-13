package store

import "os"

// mkdirAll creates a directory for private data: the database holds
// identities and messages, so only the service user may read it.
func mkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o700) //nolint:gosec // G703: the path comes from the operator's config, not from the network
}
