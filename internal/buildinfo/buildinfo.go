// SPDX-License-Identifier: MIT

// Package buildinfo reports the libvhdi module's own version.
//
// It lives in internal so that both the root package and the report package can
// use it without either importing the other. The root package re-exports it as
// Version; report records it in every document's header.
package buildinfo

import "runtime/debug"

// ModulePath is the library's Go module path, and the key its version is looked
// up under in a consuming binary's build information.
const ModulePath = "github.com/aoiflux/libvhdi"

// Version reports the libvhdi module version recorded in the calling binary's
// build information.
//
// It returns "(devel)" when libvhdi is the main module, which is the case while
// running its own tests, and "unknown" when no build information is available.
// Both are honest answers: a report generator has to be able to record that it
// could not determine its own version rather than fail or invent one.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	if info.Main.Path == ModulePath && info.Main.Version != "" {
		return info.Main.Version
	}

	for _, dep := range info.Deps {
		if dep == nil || dep.Path != ModulePath {
			continue
		}
		// A replace directive points at a different module, whose own version
		// is the one actually compiled in. Report that rather than the version
		// that was asked for and then substituted.
		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version
		}
		if dep.Version != "" {
			return dep.Version
		}
	}

	return "unknown"
}
