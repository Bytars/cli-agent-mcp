//go:build windows

package task

import "os"

func syscallGetenv(k string) string { return os.Getenv(k) }
