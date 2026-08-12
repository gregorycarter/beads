package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/mod/semver"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/git"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/utils"
)

// guardLegacyUpgradeWorkspace rejects the reviewed pre-Dolt and legacy-Dolt
// workspace shapes before command setup can track a version, migrate metadata,
// or construct a store. It intentionally classifies only metadata, regular
// SQLite files, and the bounded local version witness; storage internals stay
// behind the driver boundary.
func guardLegacyUpgradeWorkspace(beadsDir string) error {
	if beadsDir == "" {
		return nil
	}
	cfg, err := configfile.LoadForDiscovery(beadsDir)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if isHistoricalSQLiteWorkspace(beadsDir, cfg) {
		return legacyUpgradeRefusal("historical SQLite workspace")
	}
	// Validate read-only discovery metadata before any caller can use Load,
	// which migrates legacy config.json to metadata.json. Removed and unknown
	// backends must fail without changing the only pointer to their data.
	if err := validateConfiguredBackend(cfg); err != nil {
		return err
	}
	serverMode := cfg != nil && strings.EqualFold(cfg.DoltMode, configfile.DoltModeServer)
	if embeddeddolt.HasRepository(beadsDir) && !serverMode {
		return nil
	}
	version, present := legacyUpgradeVersionWitness(beadsDir)
	if serverMode && present && legacyServerVersion(version) {
		return legacyUpgradeRefusal(fmt.Sprintf("legacy Dolt server workspace from bd %s", version))
	}
	hasLocalDoltRoot := hasLegacyDoltRoot(beadsDir)
	if !hasLocalDoltRoot {
		return nil
	}
	if serverMode {
		if currentVersionWitness(version) {
			return nil
		}
		// A witness that is on disk but does not parse answers "this bd could
		// not read a version file", not "this workspace predates the 1.0 era".
		// Refusing on it turns every unrecognized version format into a total
		// outage: bd rejects the workspace outright, including the diagnostics
		// that would explain why. Warn and proceed instead; the confirmed
		// verdicts above and below stay fail-closed.
		if present && unreadableVersionWitness(version) {
			warnUnreadableVersionWitness(beadsDir, version)
			return nil
		}
		return legacyUpgradeRefusal("legacy Dolt server workspace")
	}
	if cfg == nil || cfg.DoltMode == "" ||
		strings.EqualFold(cfg.DoltMode, configfile.DoltModeEmbedded) {
		if doltserver.IsSharedServerMode() && !(present && legacyServerVersion(version)) {
			return nil
		}
		reason := "legacy Dolt workspace"
		if _, validVersion := legacyVersionMinor(version); present && validVersion {
			reason = fmt.Sprintf("legacy Dolt workspace from bd %s", version)
		}
		return legacyUpgradeRefusal(reason)
	}
	return nil
}

func isHistoricalSQLiteWorkspace(beadsDir string, cfg *configfile.Config) bool {
	if beadsDir == "" {
		return false
	}
	if embeddeddolt.HasRepository(beadsDir) {
		return false
	}
	if cfg != nil {
		return configuredHistoricalSQLiteWorkspace(beadsDir, cfg)
	}
	return discoveredHistoricalSQLiteWorkspace(beadsDir)
}

// configuredHistoricalSQLiteWorkspace classifies a metadata-backed workspace
// whose config names a bare on-disk SQLite database. It selects the database
// name from the config exactly as legacy bd did and confirms the file exists
// beneath beadsDir; a non-SQLite backend, any Dolt configuration, or a database
// path that is not a plain basename all disqualify it.
func configuredHistoricalSQLiteWorkspace(beadsDir string, cfg *configfile.Config) bool {
	if cfg.Backend != "" && cfg.Backend != configfile.BackendSQLite {
		return false
	}
	if cfg.DoltMode != "" || cfg.DoltDatabase != "" {
		return false
	}
	databaseName := cfg.SQLitePath
	if databaseName == "" {
		databaseName = cfg.Database
	}
	if databaseName == "" {
		databaseName = "beads.db"
	}
	if filepath.Base(databaseName) != databaseName {
		return false
	}
	return isNonEmptyRegularFile(filepath.Join(beadsDir, databaseName))
}

// discoveredHistoricalSQLiteWorkspace classifies a metadata-less workspace by
// its on-disk layout: exactly one non-empty regular .db file carrying a real
// SQLite header, and no entries beyond that database, its WAL/SHM sidecars, an
// optional issues.jsonl export, and an embeddeddolt directory.
func discoveredHistoricalSQLiteWorkspace(beadsDir string) bool {
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return false
	}
	databaseName, ok := soleLegacySQLiteDatabase(beadsDir, entries)
	if !ok {
		return false
	}
	if !hasSQLiteHeader(filepath.Join(beadsDir, databaseName)) {
		return false
	}
	return onlyLegacySQLiteEntries(beadsDir, entries, databaseName)
}

// soleLegacySQLiteDatabase returns the single .db filename directly beneath a
// metadata-less workspace. It fails closed if any entry is a symlink, if a .db
// file is empty or not a regular file, or if more than one .db file is present.
func soleLegacySQLiteDatabase(beadsDir string, entries []os.DirEntry) (string, bool) {
	databaseName := ""
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return "", false
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		if !isNonEmptyRegularFile(filepath.Join(beadsDir, name)) || databaseName != "" {
			return "", false
		}
		databaseName = name
	}
	return databaseName, databaseName != ""
}

// onlyLegacySQLiteEntries reports whether every entry beneath a metadata-less
// workspace belongs to the recognized legacy SQLite shape: the database file,
// its WAL/SHM sidecars, an optional issues.jsonl export, and an embeddeddolt
// directory. Any other entry, or an expected entry that is not a regular file,
// disqualifies the workspace.
func onlyLegacySQLiteEntries(beadsDir string, entries []os.DirEntry, databaseName string) bool {
	for _, entry := range entries {
		name := entry.Name()
		if name == "embeddeddolt" && entry.IsDir() {
			continue
		}
		if name == databaseName || name == "issues.jsonl" || name == databaseName+"-wal" || name == databaseName+"-shm" {
			if !isRegularFile(filepath.Join(beadsDir, name)) {
				return false
			}
			continue
		}
		return false
	}
	return true
}

// guardUndiscoveredLegacyWorkspace covers metadata-less SQLite workspaces such
// as v0.9.1's sole .beads/vc.db. Normal workspace discovery intentionally
// excludes vc.db, so this bounded fallback runs only after that discovery found
// no current workspace.
func guardUndiscoveredLegacyWorkspace() error {
	if explicit := os.Getenv("BEADS_DIR"); explicit != "" {
		beadsDir := beads.FollowRedirect(utils.CanonicalizePath(explicit))
		return guardLegacyUpgradeWorkspace(beadsDir)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	dir := utils.CanonicalizePath(cwd)
	boundary := utils.CanonicalizePath(git.GetRepoRoot())
	for {
		if err := guardLegacyUpgradeWorkspace(filepath.Join(dir, ".beads")); err != nil {
			return err
		}
		if boundary != "" && utils.PathsEqual(dir, boundary) {
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func hasLegacyDoltRoot(beadsDir string) bool {
	info, err := os.Lstat(filepath.Join(beadsDir, "dolt"))
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func isNonEmptyRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func hasSQLiteHeader(path string) bool {
	if !isNonEmptyRegularFile(path) {
		return false
	}
	file, err := os.Open(path) // #nosec G304 -- bounded header read beneath selected workspace
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, len("SQLite format 3\x00"))
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return string(header) == "SQLite format 3\x00"
}

func isRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// maxVersionWitnessBytes bounds the witness read. It is far above any version
// string bd can stamp -- a Go pseudo-version carrying both a prerelease and
// build metadata runs to roughly 70 bytes -- while keeping the read bounded.
// The previous 64-byte bound was itself tight enough to reject a legitimate
// version, which the guard would then have read as no witness at all.
const maxVersionWitnessBytes = 256

// legacyUpgradeVersionWitness reads the recorded version marker. The second
// result reports whether the workspace carries a witness at all, which is a
// separate question from whether its contents parse: no witness means no bd new
// enough to track versions (0.29.0 and later) ever wrote here, while a witness
// this bd cannot read means only that it does not recognize the format. A
// blank witness carries no claim either way and counts as no witness.
//
// The version is empty when the marker exists but could not be read within its
// size bound; that is an unreadable witness, not an absent one.
func legacyUpgradeVersionWitness(beadsDir string) (string, bool) {
	path := filepath.Join(beadsDir, localVersionFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return "", false
	}
	if info.Size() > maxVersionWitnessBytes {
		return "", true
	}
	data, err := os.ReadFile(path) // #nosec G304 -- bounded file beneath selected workspace
	if err != nil || int64(len(data)) != info.Size() {
		return "", true
	}
	version := strings.TrimSpace(string(data))
	return version, version != ""
}

func legacyServerVersion(version string) bool {
	minor, ok := legacyVersionMinor(version)
	return ok && minor >= 55 && minor <= 62
}

// currentVersionWitness identifies a post-1.0 version marker. A local Dolt root
// in server mode is ambiguous without one, so that shape must be refused rather
// than opened as a historical schema.
func currentVersionWitness(version string) bool {
	major, _, ok := versionWitnessMajorMinor(version)
	return ok && major >= 1
}

// unreadableVersionWitness reports whether a witness carries something this bd
// cannot read as a version at all. It is deliberately the complement of a real
// semver parse rather than a blocklist: any format bd, its release pipeline, or
// a downstream packager invents later lands here as unknown instead of being
// misreported as a pre-1.0 workspace.
func unreadableVersionWitness(version string) bool {
	_, _, ok := versionWitnessMajorMinor(version)
	return !ok
}

func legacyVersionMinor(version string) (int, bool) {
	major, minor, ok := versionWitnessMajorMinor(version)
	if !ok || major != 0 {
		return 0, false
	}
	return minor, true
}

// versionWitnessMajorMinor parses the recorded marker as semver and returns its
// major and minor components.
//
// The witness holds whatever bd stamped into main.Version, and that is a full
// semver string, not three integers: .goreleaser.yml and release.yml both stamp
// the release tag verbatim (the repository already carries v1.1.0-rc.1 and
// v1.1.0-rc.2), and module-pinned builds stamp a Go pseudo-version such as
// v1.1.1-0.20260805093327-bf97b73749ac. Splitting on "." and demanding exactly
// three parts rejected every one of those, so bd could not read back what bd
// had written. Parsing with the same library the module system uses keeps the
// reader's accepted set equal to the writer's emitted set.
func versionWitnessMajorMinor(version string) (int, int, bool) {
	canonical := "v" + strings.TrimPrefix(strings.TrimSpace(version), "v")
	if !semver.IsValid(canonical) {
		return 0, 0, false
	}
	// MajorMinor yields "vX.Y", or "vX" when the witness omitted the minor.
	majorMinor := strings.TrimPrefix(semver.MajorMinor(canonical), "v")
	majorText, minorText, hasMinor := strings.Cut(majorMinor, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil {
		return 0, 0, false
	}
	if !hasMinor {
		return major, 0, true
	}
	minor, err := strconv.Atoi(minorText)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// warnUnreadableVersionWitness reports an unrecognized marker once per
// workspace. Proceeding on an unreadable witness is a judgement call, so it has
// to be visible; the guard also runs against every ancestor directory during
// undiscovered-workspace probing, so the same workspace must not warn twice.
func warnUnreadableVersionWitness(beadsDir, version string) {
	unreadableWitnessMu.Lock()
	warned := unreadableWitnessWarned[beadsDir]
	if !warned {
		if unreadableWitnessWarned == nil {
			unreadableWitnessWarned = map[string]bool{}
		}
		unreadableWitnessWarned[beadsDir] = true
	}
	unreadableWitnessMu.Unlock()
	if warned {
		return
	}

	detail := fmt.Sprintf("holds %q, which is not a recognized version", version)
	if version == "" {
		detail = "could not be read within its size bound"
	}
	fmt.Fprintf(os.Stderr,
		"warning: %s in %s %s; treating the workspace as current. "+
			"Run 'bd doctor' if this is unexpected.\n",
		localVersionFile, beadsDir, detail)
}

var (
	unreadableWitnessMu     sync.Mutex
	unreadableWitnessWarned map[string]bool
)

func legacyUpgradeRefusal(reason string) error {
	return legacyUpgradeRefusalError{reason: reason}
}

type legacyUpgradeRefusalError struct{ reason string }

func (err legacyUpgradeRefusalError) Error() string {
	return fmt.Sprintf("%s detected; explicit migration is required before this bd version can open or modify the workspace. Preserve .beads unchanged and follow docs/getting-started/upgrading.md#cross-era-upgrades", err.reason)
}

func isLegacyUpgradeRefusal(err error) bool {
	_, ok := err.(legacyUpgradeRefusalError)
	return ok
}
