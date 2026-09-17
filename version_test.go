// SPDX-License-Identifier: MIT

package libvhdi_test

import (
	"runtime/debug"
	"testing"

	"github.com/aoiflux/libvhdi"
)

// Version must never again be a hand-maintained constant that can drift away
// from the published tag. These tests pin the properties that guarantee it.

func TestVersionIsDerivedFromBuildInfo(t *testing.T) {
	got := libvhdi.Version()
	if got == "" {
		t.Fatal("Version() returned an empty string; it must always name something, even if only \"unknown\"")
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		if got != "unknown" {
			t.Fatalf("Version() = %q with no build info available, want \"unknown\"", got)
		}
		return
	}

	// Under `go test` the module under test is the main module, so the
	// toolchain stamps it as the development pseudo-version. Anything else
	// means the lookup is not actually consulting build info.
	if info.Main.Path == libvhdi.ModulePath {
		if got != info.Main.Version {
			t.Fatalf("Version() = %q, want the main module's version %q", got, info.Main.Version)
		}
		return
	}

	for _, dep := range info.Deps {
		if dep != nil && dep.Path == libvhdi.ModulePath {
			return
		}
	}
	if got != "unknown" {
		t.Fatalf("Version() = %q but libvhdi appears in neither the main module nor Deps", got)
	}
}

func TestVersionDoesNotReportAStaleHardcodedRelease(t *testing.T) {
	// 0.6.0 was the value of the old constant. The newest tag at the time was
	// v0.2.0, so the constant was simply wrong, and no build of this library
	// should ever produce that string again.
	if got := libvhdi.Version(); got == "0.6.0" || got == "v0.6.0" {
		t.Fatalf("Version() = %q: the stale hardcoded release has come back", got)
	}
}

func TestModulePathMatchesTheImportPath(t *testing.T) {
	// If the module is ever renamed, Version() would silently start returning
	// "unknown" for every consumer, because the Deps lookup keys on this.
	const want = "github.com/aoiflux/libvhdi"
	if libvhdi.ModulePath != want {
		t.Fatalf("ModulePath = %q, want %q", libvhdi.ModulePath, want)
	}
}
