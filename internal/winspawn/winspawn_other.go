// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package winspawn

import "os/exec"

// Harden no hace nada en plataformas sin ventanas de consola. El grupo de
// procesos lo fija `internal/task`, que es quien maneja la cancelación.
func Harden(cmd *exec.Cmd) *exec.Cmd { return cmd }
