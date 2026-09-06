package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	latestReleaseURL    = "https://api.github.com/repos/janiorvalle/hop/releases/latest"
	maximumArchiveSize  = 128 << 20
	maximumChecksumSize = 1 << 20
)

type releaseAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName string         `json:"tag_name"`
	HTMLURL string         `json:"html_url"`
	Assets  []releaseAsset `json:"assets"`
}

type upgradeOptions struct {
	currentVersion  string
	releaseURL      string
	executablePath  string
	operatingSystem string
	architecture    string
	client          *http.Client
	stdout          io.Writer
	validateBinary  func(context.Context, string, string) error
}

func upgradeHop(ctx context.Context, stdout io.Writer) error {
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("[UPGRADE_EXECUTABLE_UNKNOWN] Could not find the running hop binary: %w. Install the latest release again", err)
	}

	return runUpgrade(ctx, upgradeOptions{
		currentVersion:  version(),
		releaseURL:      latestReleaseURL,
		executablePath:  executablePath,
		operatingSystem: runtime.GOOS,
		architecture:    runtime.GOARCH,
		client:          &http.Client{Timeout: 45 * time.Second},
		stdout:          stdout,
		validateBinary:  validateDownloadedBinary,
	})
}

func runUpgrade(ctx context.Context, options upgradeOptions) error {
	currentVersion, err := releaseVersion(options.currentVersion)
	if err != nil {
		return fmt.Errorf("[UPGRADE_DEV_BUILD] This hop build cannot upgrade itself because its version is %q. Install a published release first, then run 'hop upgrade' again", options.currentVersion)
	}

	release, err := fetchLatestRelease(ctx, options.client, options.releaseURL)
	if err != nil {
		return err
	}
	latestVersion, err := releaseVersion(release.TagName)
	if err != nil {
		return fmt.Errorf("[UPGRADE_RELEASE_INVALID] The latest GitHub release has invalid tag %q. Keep the current binary and report the release at %s", release.TagName, releasePage(release))
	}

	comparison := semver.Compare("v"+currentVersion, "v"+latestVersion)
	if comparison >= 0 {
		if comparison == 0 {
			_, err = fmt.Fprintf(options.stdout, "hop %s is already up to date.\n", currentVersion)
		} else {
			_, err = fmt.Fprintf(options.stdout, "hop %s is newer than the latest release (%s); no upgrade needed.\n", currentVersion, latestVersion)
		}
		return err
	}

	archiveName := releaseArchiveName(latestVersion, options.operatingSystem, options.architecture)
	archiveAsset, found := findReleaseAsset(release.Assets, archiveName)
	if !found {
		return fmt.Errorf("[UPGRADE_ASSET_MISSING] Release %s has no %s artifact for %s/%s. Keep hop %s and download a supported build from %s", latestVersion, archiveName, options.operatingSystem, options.architecture, currentVersion, releasePage(release))
	}
	checksumAsset, found := findReleaseAsset(release.Assets, "checksums.txt")
	if !found {
		return fmt.Errorf("[UPGRADE_CHECKSUMS_MISSING] Release %s has no checksums.txt, so hop refused to install it. Keep hop %s and report the release at %s", latestVersion, currentVersion, releasePage(release))
	}

	archive, err := downloadReleaseFile(ctx, options.client, archiveAsset.DownloadURL, archiveName, maximumArchiveSize, releasePage(release))
	if err != nil {
		return err
	}
	checksums, err := downloadReleaseFile(ctx, options.client, checksumAsset.DownloadURL, "checksums.txt", maximumChecksumSize, releasePage(release))
	if err != nil {
		return err
	}
	if err := verifyReleaseChecksum(archiveName, archive, checksums, currentVersion); err != nil {
		return err
	}

	binaryName := "hop"
	if options.operatingSystem == "windows" {
		binaryName += ".exe"
	}
	binary, err := extractReleaseBinary(archiveName, archive, binaryName)
	if err != nil {
		return err
	}
	stagedPath, err := stageExecutable(options.executablePath, binary)
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(stagedPath) // The path disappears after a successful rename.
	}()

	if err := options.validateBinary(ctx, stagedPath, latestVersion); err != nil {
		return err
	}
	if err := replaceExecutable(options.executablePath, stagedPath, options.operatingSystem); err != nil {
		return err
	}

	_, err = fmt.Fprintf(options.stdout, "Upgraded hop from %s to %s.\n", currentVersion, latestVersion)
	return err
}

func releaseVersion(candidate string) (string, error) {
	trimmed := strings.TrimSpace(candidate)
	normalized := strings.TrimPrefix(trimmed, "v")
	if trimmed == "" || normalized == developmentVersion || strings.Contains(strings.ToLower(normalized), "dirty") || !semver.IsValid("v"+normalized) {
		return "", errors.New("not a published semantic version")
	}
	return normalized, nil
}

func fetchLatestRelease(ctx context.Context, client *http.Client, releaseURL string) (githubRelease, error) {
	payload, err := downloadReleaseFile(ctx, client, releaseURL, "latest release metadata", maximumChecksumSize, "https://github.com/janiorvalle/hop/releases")
	if err != nil {
		return githubRelease{}, err
	}

	var release githubRelease
	if err := json.Unmarshal(payload, &release); err != nil {
		return githubRelease{}, fmt.Errorf("[UPGRADE_RELEASE_INVALID] GitHub returned unreadable release metadata. Keep the current binary and retry 'hop upgrade' later: %w", err)
	}
	return release, nil
}

func downloadReleaseFile(ctx context.Context, client *http.Client, url, name string, maximumSize int64, releaseURL string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("[UPGRADE_NETWORK] Could not prepare the request for %s: %w. Keep the current binary and retry 'hop upgrade'", name, err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "hop-upgrade")

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("[UPGRADE_NETWORK] Could not download %s: %w. Check the network connection, keep the current binary, and retry 'hop upgrade'", name, err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("[UPGRADE_RATE_LIMITED] GitHub rate-limited the request for %s. Wait for the limit to reset, then run 'hop upgrade' again; the current binary was not changed", name)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[UPGRADE_DOWNLOAD_FAILED] Could not download %s: server returned %s. Keep the current binary, retry 'hop upgrade', or use %s", name, response.Status, releaseURL)
	}

	payload, err := io.ReadAll(io.LimitReader(response.Body, maximumSize+1))
	if err != nil {
		return nil, fmt.Errorf("[UPGRADE_NETWORK] The download for %s stopped early: %w. Keep the current binary and retry 'hop upgrade'", name, err)
	}
	if int64(len(payload)) > maximumSize {
		return nil, fmt.Errorf("[UPGRADE_DOWNLOAD_TOO_LARGE] The %s download exceeded %d bytes, so hop refused it. Keep the current binary and inspect the release at %s", name, maximumSize, releaseURL)
	}
	return payload, nil
}

func releaseArchiveName(releaseVersion, operatingSystem, architecture string) string {
	extension := ".tar.gz"
	if operatingSystem == "windows" {
		extension = ".zip"
	}
	return fmt.Sprintf("hop_%s_%s_%s%s", releaseVersion, operatingSystem, architecture, extension)
}

func findReleaseAsset(assets []releaseAsset, name string) (releaseAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name && asset.DownloadURL != "" {
			return asset, true
		}
	}
	return releaseAsset{}, false
}

func releasePage(release githubRelease) string {
	if release.HTMLURL != "" {
		return release.HTMLURL
	}
	return "https://github.com/janiorvalle/hop/releases"
}

func verifyReleaseChecksum(archiveName string, archive, checksumFile []byte, currentVersion string) error {
	expectedChecksum := ""
	for line := range strings.Lines(string(checksumFile)) {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != archiveName {
			continue
		}
		if len(fields[0]) == sha256.Size*2 {
			if _, err := hex.DecodeString(fields[0]); err == nil {
				expectedChecksum = strings.ToLower(fields[0])
			}
		}
		break
	}
	if expectedChecksum == "" {
		return fmt.Errorf("[UPGRADE_CHECKSUM_MISSING] checksums.txt has no valid SHA-256 entry for %s, so hop refused the release. Keep hop %s and report the broken release", archiveName, currentVersion)
	}

	actualChecksum := fmt.Sprintf("%x", sha256.Sum256(archive))
	if actualChecksum != expectedChecksum {
		return fmt.Errorf("[UPGRADE_CHECKSUM_MISMATCH] The SHA-256 checksum for %s did not match the release manifest. Hop %s was not changed; retry later or inspect the release before installing manually", archiveName, currentVersion)
	}
	return nil
}

func extractReleaseBinary(archiveName string, archive []byte, binaryName string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractBinaryFromZip(archiveName, archive, binaryName)
	}
	return extractBinaryFromTarGzip(archiveName, archive, binaryName)
}

func extractBinaryFromTarGzip(archiveName string, archive []byte, binaryName string) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, invalidArchiveError(archiveName, binaryName, err)
	}
	defer func() {
		_ = gzipReader.Close()
	}()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalidArchiveError(archiveName, binaryName, err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != binaryName {
			continue
		}
		binary, err := io.ReadAll(io.LimitReader(tarReader, maximumArchiveSize+1))
		if err != nil || len(binary) > maximumArchiveSize {
			return nil, invalidArchiveError(archiveName, binaryName, err)
		}
		return binary, nil
	}
	return nil, invalidArchiveError(archiveName, binaryName, errors.New("binary is missing"))
}

func extractBinaryFromZip(archiveName string, archive []byte, binaryName string) ([]byte, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, invalidArchiveError(archiveName, binaryName, err)
	}
	for _, file := range zipReader.File {
		if file.FileInfo().IsDir() || filepath.Base(file.Name) != binaryName {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return nil, invalidArchiveError(archiveName, binaryName, err)
		}
		binary, readErr := io.ReadAll(io.LimitReader(reader, maximumArchiveSize+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || len(binary) > maximumArchiveSize {
			return nil, invalidArchiveError(archiveName, binaryName, errors.Join(readErr, closeErr))
		}
		return binary, nil
	}
	return nil, invalidArchiveError(archiveName, binaryName, errors.New("binary is missing"))
}

func invalidArchiveError(archiveName, binaryName string, err error) error {
	return fmt.Errorf("[UPGRADE_ARCHIVE_INVALID] %s did not contain a readable %s binary: %w. Keep the current binary and report the release", archiveName, binaryName, err)
}

func stageExecutable(executablePath string, binary []byte) (stagedPath string, returnedError error) {
	stagedFile, err := os.CreateTemp(filepath.Dir(executablePath), ".hop-upgrade-*")
	if err != nil {
		return "", replacementError(executablePath, err)
	}
	stagedPath = stagedFile.Name()
	defer func() {
		if returnedError != nil {
			_ = os.Remove(stagedPath)
		}
	}()

	if err := stagedFile.Chmod(0o755); err != nil {
		_ = stagedFile.Close()
		return "", replacementError(executablePath, err)
	}
	if _, err := stagedFile.Write(binary); err != nil {
		_ = stagedFile.Close()
		return "", replacementError(executablePath, err)
	}
	if err := stagedFile.Sync(); err != nil {
		_ = stagedFile.Close()
		return "", replacementError(executablePath, err)
	}
	if err := stagedFile.Close(); err != nil {
		return "", replacementError(executablePath, err)
	}
	return stagedPath, nil
}

func validateDownloadedBinary(ctx context.Context, path, expectedVersion string) error {
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("[UPGRADE_BINARY_INVALID] The verified release binary could not start: %w. Keep the current binary and report the release", err)
	}
	if got, want := strings.TrimSpace(string(output)), "hop "+expectedVersion; got != want {
		return fmt.Errorf("[UPGRADE_BINARY_INVALID] The verified release binary reported %q instead of %q. Keep the current binary and report the release", got, want)
	}
	return nil
}

func replaceExecutable(executablePath, stagedPath, operatingSystem string) error {
	if operatingSystem != "windows" {
		if err := os.Rename(stagedPath, executablePath); err != nil {
			return replacementError(executablePath, err)
		}
		return nil
	}

	backupPath := executablePath + ".old"
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return replacementError(executablePath, err)
	}
	if err := os.Rename(executablePath, backupPath); err != nil {
		return replacementError(executablePath, err)
	}
	if err := os.Rename(stagedPath, executablePath); err != nil {
		if restoreErr := os.Rename(backupPath, executablePath); restoreErr != nil {
			return fmt.Errorf("[UPGRADE_ROLLBACK_FAILED] Could not install the new binary (%v) or restore the old one (%v). Move %s back to %s, then retry", err, restoreErr, backupPath, executablePath)
		}
		return replacementError(executablePath, err)
	}
	// Windows can keep the running image locked. A leftover .old file is safely
	// removed at the start of the next explicit upgrade.
	_ = os.Remove(backupPath)
	return nil
}

func replacementError(executablePath string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("[UPGRADE_NO_PERMISSION] Hop cannot write beside %s. Fix that file's ownership or reinstall hop in a directory you own, then run 'hop upgrade' again; the current binary was not changed", executablePath)
	}
	return fmt.Errorf("[UPGRADE_REPLACE_FAILED] Could not replace %s: %w. Keep the current binary and retry 'hop upgrade'", executablePath, err)
}
