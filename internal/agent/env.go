package agent

import "os"

// getenv exists so the Windows-only helpers can read the environment without
// each build-tagged file importing os separately.
func getenv(k string) string { return os.Getenv(k) }
