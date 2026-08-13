// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package agent

// packageIdentity: only Windows has the MSIX package model. macOS app bundles
// and Linux Flatpak/Snap sandboxes have their own quirks, but they do not
// break child-process spawning the same way, and the spawn probes catch them
// regardless.
func packageIdentity() (bool, string) { return false, "" }
