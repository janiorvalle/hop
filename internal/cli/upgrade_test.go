package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpgradeInstallsVerifiedPlatformRelease(t *testing.T) {
	t.Parallel()

	executablePath := writeDisposableExecutable(t, []byte("old binary"))
	archiveName := releaseArchiveName("1.1.0", "linux", "amd64")
	newBinary := []byte("new binary")
	archive := makeReleaseArchive(t, archiveName, "hop", newBinary)
	server := newReleaseServer(t, "v1.1.0", archiveName, archive, validChecksums(archiveName, archive), true)

	var stdout bytes.Buffer
	err := runUpgrade(context.Background(), upgradeOptions{
		currentVersion:  "1.0.0",
		releaseURL:      server.URL + "/latest",
		executablePath:  executablePath,
		operatingSystem: "linux",
		architecture:    "amd64",
		client:          server.Client(),
		stdout:          &stdout,
		validateBinary:  acceptTestBinary,
	})
	if err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if got, err := os.ReadFile(executablePath); err != nil || !bytes.Equal(got, newBinary) {
		t.Fatalf("installed binary = %q, %v; want %q", got, err, newBinary)
	}
	if got, want := stdout.String(), "Upgraded hop from 1.0.0 to 1.1.0.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestUpgradeReportsAlreadyCurrentWithoutDownloadingAssets(t *testing.T) {
	t.Parallel()

	server := newReleaseServer(t, "v1.0.0", "unused", nil, nil, false)
	var stdout bytes.Buffer
	err := runUpgrade(context.Background(), upgradeOptions{
		currentVersion:  "v1.0.0",
		releaseURL:      server.URL + "/latest",
		executablePath:  filepath.Join(t.TempDir(), "hop"),
		operatingSystem: "linux",
		architecture:    "amd64",
		client:          server.Client(),
		stdout:          &stdout,
		validateBinary:  acceptTestBinary,
	})
	if err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if got, want := stdout.String(), "hop 1.0.0 is already up to date.\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestUpgradeRefusesChecksumMismatchAndKeepsCurrentBinary(t *testing.T) {
	t.Parallel()

	oldBinary := []byte("known working binary")
	executablePath := writeDisposableExecutable(t, oldBinary)
	archiveName := releaseArchiveName("1.1.0", "linux", "amd64")
	archive := makeReleaseArchive(t, archiveName, "hop", []byte("tampered binary"))
	badChecksums := []byte(strings.Repeat("0", sha256.Size*2) + "  " + archiveName + "\n")
	server := newReleaseServer(t, "v1.1.0", archiveName, archive, badChecksums, true)

	err := runUpgrade(context.Background(), testUpgradeOptions(server, executablePath))
	if err == nil || !strings.Contains(err.Error(), "[UPGRADE_CHECKSUM_MISMATCH]") {
		t.Fatalf("runUpgrade() error = %v, want checksum mismatch instructions", err)
	}
	if got, readErr := os.ReadFile(executablePath); readErr != nil || !bytes.Equal(got, oldBinary) {
		t.Fatalf("current binary changed to %q, %v; want %q", got, readErr, oldBinary)
	}
}

func TestUpgradeExplainsMissingPlatformAsset(t *testing.T) {
	t.Parallel()

	server := newReleaseServer(t, "v1.1.0", "unused", nil, nil, false)
	options := testUpgradeOptions(server, filepath.Join(t.TempDir(), "hop"))
	options.architecture = "riscv64"

	err := runUpgrade(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "[UPGRADE_ASSET_MISSING]") || !strings.Contains(err.Error(), "linux/riscv64") {
		t.Fatalf("runUpgrade() error = %v, want platform-specific next step", err)
	}
}

func TestUpgradeExplainsRateLimitWithoutChangingBinary(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	executablePath := writeDisposableExecutable(t, []byte("current"))
	options := testUpgradeOptions(server, executablePath)
	options.releaseURL = server.URL

	err := runUpgrade(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "[UPGRADE_RATE_LIMITED]") || !strings.Contains(err.Error(), "hop upgrade") {
		t.Fatalf("runUpgrade() error = %v, want retry instructions", err)
	}
}

func TestUpgradeRefusesDevelopmentAndDirtyBuildsBeforeNetworkRequest(t *testing.T) {
	t.Parallel()

	for _, currentVersion := range []string{"dev", "1.0.0+dirty", ""} {
		currentVersion := currentVersion
		t.Run(currentVersion, func(t *testing.T) {
			t.Parallel()
			err := runUpgrade(context.Background(), upgradeOptions{currentVersion: currentVersion})
			if err == nil || !strings.Contains(err.Error(), "[UPGRADE_DEV_BUILD]") || !strings.Contains(err.Error(), "published release") {
				t.Fatalf("runUpgrade() error = %v, want release-install guidance", err)
			}
		})
	}
}

func TestUpgradeExtractsWindowsZip(t *testing.T) {
	t.Parallel()

	archiveName := releaseArchiveName("1.1.0", "windows", "arm64")
	want := []byte("windows binary")
	archive := makeReleaseArchive(t, archiveName, "hop.exe", want)
	got, err := extractReleaseBinary(archiveName, archive, "hop.exe")
	if err != nil {
		t.Fatalf("extractReleaseBinary() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("extractReleaseBinary() = %q, want %q", got, want)
	}
}

func TestUpgradeLocallyServedReleaseReplacesExecutableCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("receipt compiles disposable release binaries")
	}

	temporaryDirectory := t.TempDir()
	oldBinaryPath := filepath.Join(temporaryDirectory, executableName("hop-copy"))
	newBinaryPath := filepath.Join(temporaryDirectory, executableName("hop-new"))
	buildReleaseBinary(t, oldBinaryPath, "1.0.0")
	buildReleaseBinary(t, newBinaryPath, "1.1.0")
	newBinary, err := os.ReadFile(newBinaryPath)
	if err != nil {
		t.Fatal(err)
	}

	archiveName := releaseArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH)
	archive := makeReleaseArchive(t, archiveName, executableName("hop"), newBinary)
	server := newReleaseServer(t, "v1.1.0", archiveName, archive, validChecksums(archiveName, archive), true)
	var stdout bytes.Buffer
	err = runUpgrade(context.Background(), upgradeOptions{
		currentVersion:  "1.0.0",
		releaseURL:      server.URL + "/latest",
		executablePath:  oldBinaryPath,
		operatingSystem: runtime.GOOS,
		architecture:    runtime.GOARCH,
		client:          server.Client(),
		stdout:          &stdout,
		validateBinary:  validateDownloadedBinary,
	})
	if err != nil {
		t.Fatalf("runUpgrade() against local release error = %v", err)
	}

	output, err := exec.Command(oldBinaryPath, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("upgraded executable failed: %v: %s", err, output)
	}
	if got, want := string(output), "hop 1.1.0\n"; got != want {
		t.Fatalf("upgraded executable output = %q, want %q", got, want)
	}
}

func testUpgradeOptions(server *httptest.Server, executablePath string) upgradeOptions {
	return upgradeOptions{
		currentVersion:  "1.0.0",
		releaseURL:      server.URL + "/latest",
		executablePath:  executablePath,
		operatingSystem: "linux",
		architecture:    "amd64",
		client:          server.Client(),
		stdout:          io.Discard,
		validateBinary:  acceptTestBinary,
	}
}

func acceptTestBinary(context.Context, string, string) error {
	return nil
}

func writeDisposableExecutable(t *testing.T, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), executableName("hop"))
	if err := os.WriteFile(path, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newReleaseServer(t *testing.T, version, archiveName string, archive, checksums []byte, includeArchive bool) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			assets := []releaseAsset{{Name: "checksums.txt", DownloadURL: server.URL + "/checksums.txt"}}
			if includeArchive {
				assets = append(assets, releaseAsset{Name: archiveName, DownloadURL: server.URL + "/" + archiveName})
			}
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(githubRelease{
				TagName: version,
				HTMLURL: server.URL + "/release",
				Assets:  assets,
			}); err != nil {
				t.Errorf("encode release: %v", err)
			}
		case "/" + archiveName:
			_, _ = response.Write(archive)
		case "/checksums.txt":
			_, _ = response.Write(checksums)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func validChecksums(archiveName string, archive []byte) []byte {
	return []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), archiveName))
}

func makeReleaseArchive(t *testing.T, archiveName, binaryName string, binary []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	if strings.HasSuffix(archiveName, ".zip") {
		writer := zip.NewWriter(&archive)
		file, err := writer.Create(binaryName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(binary); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return archive.Bytes()
	}

	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: binaryName, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func buildReleaseBinary(t *testing.T, outputPath, releaseVersion string) {
	t.Helper()
	command := exec.Command(
		"go", "build",
		"-buildvcs=false",
		"-o", outputPath,
		"-ldflags", "-X github.com/janiorvalle/hop/internal/cli.Version="+releaseVersion,
		"../..",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build disposable release binary: %v: %s", err, output)
	}
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
