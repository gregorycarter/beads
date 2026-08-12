package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCurrentVersionWitnessAcceptsEveryShapeTheWriterEmits pins the round-trip
// contract at the parse level: every version string bd's own build and release
// pipeline can stamp into main.Version must classify as a post-1.0 witness.
//
// The regression this covers took five production cities down. The witness
// parser split on "." and demanded exactly three parts, so a Go pseudo-version
// (v1.1.1-0.20260805093327-bf97b73749ac -> four parts) and a release-candidate
// tag (1.1.0-rc.1 -> four parts) both failed to parse and were reported as
// legacy pre-1.0 workspaces. Every bd invocation against those workspaces was
// refused outright.
func TestCurrentVersionWitnessAcceptsEveryShapeTheWriterEmits(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		// The exact string recovered from the production workspaces.
		{name: "go pseudo-version", version: "v1.1.1-0.20260805093327-bf97b73749ac", want: true},
		// The hand-repair value used to unblock the fleet.
		{name: "bare release", version: "1.1.0", want: true},
		// goreleaser and release.yml both stamp the tag with the v stripped,
		// but downstream packagers keep it.
		{name: "v-prefixed release", version: "v1.2.1", want: true},
		// v1.1.0-rc.1 and v1.1.0-rc.2 are real tags in this repository, so
		// release.yml has already built binaries stamped exactly this way.
		{name: "prerelease", version: "1.1.0-rc.1", want: true},
		{name: "v-prefixed prerelease", version: "v1.1.0-rc.2", want: true},
		{name: "build metadata", version: "1.2.1+build.20260805", want: true},
		{name: "prerelease and build metadata", version: "v1.2.1-rc.1+linux.amd64", want: true},
		// Two-component versions are still post-1.0.
		{name: "major minor only", version: "v1.2", want: true},

		// Pre-1.0 must stay classified as legacy in every shape.
		{name: "legacy release", version: "0.62.0", want: false},
		{name: "legacy pseudo-version", version: "v0.62.0-0.20251201093327-bf97b73749ac", want: false},
		{name: "legacy prerelease", version: "0.55.4-rc.1", want: false},

		// Not versions at all.
		{name: "empty", version: "", want: false},
		{name: "garbage", version: "not-a-version", want: false},
		{name: "path-like garbage", version: "/usr/local/bin/bd", want: false},
		{name: "leading junk", version: "bd version 1.2.1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := currentVersionWitness(tt.version); got != tt.want {
				t.Fatalf("currentVersionWitness(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

// TestLegacyVersionMinorAcceptsPrereleaseAndPseudoVersions covers the other
// direction of the same dot-counting bug. legacyVersionMinor drives the
// external-legacy-server refusal, so a pre-1.0 workspace whose witness carried
// a prerelease or pseudo-version suffix was silently admitted -- the guard
// failed open on exactly the shape it exists to catch.
func TestLegacyVersionMinorAcceptsPrereleaseAndPseudoVersions(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		wantMinor int
		wantOK    bool
	}{
		{name: "plain legacy", version: "0.62.0", wantMinor: 62, wantOK: true},
		{name: "v-prefixed legacy", version: "v0.55.4", wantMinor: 55, wantOK: true},
		{name: "legacy prerelease", version: "0.62.0-rc.1", wantMinor: 62, wantOK: true},
		{name: "legacy pseudo-version", version: "v0.60.0-0.20251201093327-bf97b73749ac", wantMinor: 60, wantOK: true},
		{name: "legacy build metadata", version: "0.58.1+deb12u1", wantMinor: 58, wantOK: true},
		{name: "post-1.0 is not legacy", version: "1.1.0", wantOK: false},
		{name: "pseudo-version is not legacy", version: "v1.1.1-0.20260805093327-bf97b73749ac", wantOK: false},
		{name: "garbage", version: "not-a-version", wantOK: false},
		{name: "empty", version: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			minor, ok := legacyVersionMinor(tt.version)
			if ok != tt.wantOK {
				t.Fatalf("legacyVersionMinor(%q) ok = %v, want %v", tt.version, ok, tt.wantOK)
			}
			if ok && minor != tt.wantMinor {
				t.Fatalf("legacyVersionMinor(%q) minor = %d, want %d", tt.version, minor, tt.wantMinor)
			}
		})
	}
}

// TestLegacyServerVersionWindowSurvivesSuffixes proves the 0.55-0.62 refusal
// window still closes on decorated legacy witnesses and does not swallow
// neighbouring versions.
func TestLegacyServerVersionWindowSurvivesSuffixes(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{version: "0.54.9", want: false},
		{version: "0.55.0", want: true},
		{version: "0.62.21", want: true},
		{version: "0.63.0", want: false},
		{version: "v0.62.0-rc.1", want: true},
		{version: "v0.55.0-0.20251201093327-bf97b73749ac", want: true},
		{version: "v1.1.1-0.20260805093327-bf97b73749ac", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := legacyServerVersion(tt.version); got != tt.want {
				t.Fatalf("legacyServerVersion(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

// TestLocalVersionWitnessRoundTrip is the contract the outage violated:
// whatever bd writes into .beads/.local_version, bd must be able to read back
// and classify. It drives the real writer rather than a literal so the two
// sides cannot drift apart again.
func TestLocalVersionWitnessRoundTrip(t *testing.T) {
	// Every shape main.Version can hold: the compiled-in default, the tag
	// forms release.yml and .goreleaser.yml stamp, and the pseudo-version
	// downstream module-pinned builds stamp.
	written := []string{
		Version,
		"1.2.1",
		"v1.2.1",
		"1.1.0-rc.1",
		"v1.1.0-rc.2",
		"v1.1.1-0.20260805093327-bf97b73749ac",
		"1.2.1+build.20260805",
		"v1.2.1-rc.1.0.20260805093327-bf97b73749ac+build.20260805.linux.amd64",
	}

	for _, version := range written {
		t.Run(version, func(t *testing.T) {
			beadsDir := t.TempDir()
			path := filepath.Join(beadsDir, localVersionFile)
			if err := writeLocalVersion(path, version); err != nil {
				t.Fatalf("writeLocalVersion(%q) = %v", version, err)
			}

			// The advisory reader and the guard's witness reader must agree.
			if got := readLocalVersion(path); got != version {
				t.Fatalf("readLocalVersion() = %q, want %q", got, version)
			}
			witness, present := legacyUpgradeVersionWitness(beadsDir)
			if !present {
				t.Fatalf("legacyUpgradeVersionWitness() reported no witness for %q", version)
			}
			if witness != version {
				t.Fatalf("legacyUpgradeVersionWitness() = %q, want %q", witness, version)
			}
			if !currentVersionWitness(witness) {
				t.Fatalf("bd wrote %q but currentVersionWitness() refuses to read it back", version)
			}

			// End to end: the workspace shape that took the fleet down.
			writeServerModeDoltWorkspace(t, beadsDir)
			if err := guardLegacyUpgradeWorkspace(beadsDir); err != nil {
				t.Fatalf("guard refused a workspace stamped by bd itself (%q): %v", version, err)
			}
		})
	}
}

// TestLegacyUpgradeGuardTreatsUnreadableWitnessAsUnknown pins the failure
// policy. A witness bd cannot parse answers "I could not read a version file",
// not "this is a legacy workspace". The guard warns and proceeds instead of
// refusing every command, which is what turned a parse bug into a fleet outage.
func TestLegacyUpgradeGuardTreatsUnreadableWitnessAsUnknown(t *testing.T) {
	unreadable := []string{
		"not-a-version",
		"/usr/local/bin/bd",
		"bd version 1.2.1",
		"1.2.1.4",
	}

	for _, version := range unreadable {
		t.Run(version, func(t *testing.T) {
			beadsDir := t.TempDir()
			writeServerModeDoltWorkspace(t, beadsDir)
			if err := os.WriteFile(filepath.Join(beadsDir, localVersionFile), []byte(version+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := guardLegacyUpgradeWorkspace(beadsDir); err != nil {
				t.Fatalf("unreadable witness %q was refused as legacy: %v", version, err)
			}
		})
	}
}

// TestLegacyUpgradeGuardStillRefusesConfirmedLegacy proves the other direction:
// loosening the unreadable case must not loosen the confirmed case. A witness
// that parses as pre-1.0 is a confirmed cross-era workspace and stays refused,
// and so does a workspace with no witness at all -- .local_version has existed
// since 0.29.0, so its absence is itself evidence of a pre-0.29 vintage.
func TestLegacyUpgradeGuardStillRefusesConfirmedLegacy(t *testing.T) {
	tests := []struct {
		name    string
		version string
		write   bool
	}{
		{name: "confirmed legacy release", version: "0.62.0", write: true},
		{name: "confirmed legacy prerelease", version: "0.62.0-rc.1", write: true},
		{name: "confirmed legacy pseudo-version", version: "v0.60.0-0.20251201093327-bf97b73749ac", write: true},
		{name: "confirmed pre-1.0 outside the server window", version: "0.49.6", write: true},
		{name: "absent witness", write: false},
		{name: "empty witness", version: "", write: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beadsDir := t.TempDir()
			writeServerModeDoltWorkspace(t, beadsDir)
			if tt.write {
				if err := os.WriteFile(filepath.Join(beadsDir, localVersionFile), []byte(tt.version+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := guardLegacyUpgradeWorkspace(beadsDir); !isLegacyUpgradeRefusal(err) {
				t.Fatalf("guardLegacyUpgradeWorkspace() = %v, want cross-era migration refusal", err)
			}
		})
	}
}

// TestLegacyUpgradeGuardAdmitsWitnessTooLargeToRead covers the bounded read
// itself. A marker larger than the read bound yields no version, which the
// guard must treat as unreadable rather than as absent -- the old 64-byte bound
// was tight enough to clip a legitimate pseudo-version carrying build metadata.
func TestLegacyUpgradeGuardAdmitsWitnessTooLargeToRead(t *testing.T) {
	beadsDir := t.TempDir()
	writeServerModeDoltWorkspace(t, beadsDir)
	oversized := "v1.2.1+" + strings.Repeat("a", maxVersionWitnessBytes)
	if err := os.WriteFile(filepath.Join(beadsDir, localVersionFile), []byte(oversized+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	version, present := legacyUpgradeVersionWitness(beadsDir)
	if !present {
		t.Fatal("an oversized marker was reported as no witness at all")
	}
	if version != "" {
		t.Fatalf("legacyUpgradeVersionWitness() = %q, want empty for an unread marker", version)
	}
	if err := guardLegacyUpgradeWorkspace(beadsDir); err != nil {
		t.Fatalf("oversized witness was refused as legacy: %v", err)
	}
}

// TestUnreadableVersionWitnessWarns proves the fail-open path stays visible.
// Proceeding on an unreadable marker is a judgement call, so it must reach
// stderr, and it must not repeat for the same workspace across the guard's
// ancestor walk.
func TestUnreadableVersionWitnessWarns(t *testing.T) {
	beadsDir := t.TempDir()
	writeServerModeDoltWorkspace(t, beadsDir)
	if err := os.WriteFile(filepath.Join(beadsDir, localVersionFile), []byte("not-a-version\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := captureStderr(t, func() {
		if err := guardLegacyUpgradeWorkspace(beadsDir); err != nil {
			t.Fatalf("guardLegacyUpgradeWorkspace() = %v, want nil", err)
		}
	})
	if !strings.Contains(first, "not-a-version") || !strings.Contains(first, localVersionFile) {
		t.Fatalf("guard proceeded silently on an unreadable witness; stderr = %q", first)
	}

	second := captureStderr(t, func() {
		if err := guardLegacyUpgradeWorkspace(beadsDir); err != nil {
			t.Fatalf("guardLegacyUpgradeWorkspace() = %v, want nil", err)
		}
	})
	if second != "" {
		t.Fatalf("warning repeated for the same workspace; stderr = %q", second)
	}
}

// writeServerModeDoltWorkspace lays down the exact shape the production cities
// had: explicit Dolt server metadata plus a local .beads/dolt root. That is the
// only shape whose verdict the version witness decides.
func writeServerModeDoltWorkspace(t *testing.T, beadsDir string) {
	t.Helper()
	metadata := []byte(`{"backend":"dolt","dolt_mode":"server"}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(beadsDir, "dolt"), 0o700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
}
