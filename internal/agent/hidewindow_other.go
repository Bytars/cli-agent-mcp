// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package agent

import "os/exec"

// systemRoot has no meaning off Windows.
func systemRoot() string { return "" }

// hardenSpawn is a no-op on platforms without console windows. The process
// group is set by the task package, which owns cancellation.
func hardenSpawn(cmd *exec.Cmd) *exec.Cmd { return cmd }
