package cliapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
)

// archiveAssetSuffix is appended to the per-target binary name to form the
// archived release artifact name, e.g. looper-darwin-arm64.tar.gz.
const archiveAssetSuffix = ".tar.gz"

// maxArchiveBinaryBytes caps the size of any binary extracted from a release
// archive. The current looper/looperd binaries weigh well below 100 MiB; the
// generous ceiling guards against malicious or corrupt archives without
// constraining legitimate growth.
const maxArchiveBinaryBytes = 256 * 1024 * 1024

// downloadAsset describes a single artifact that, once downloaded and
// verified, produces a single executable binary. The archive variant is
// preferred when available because it transfers significantly fewer bytes
// than the raw binary.
type downloadAsset struct {
	// PreferredURL is the URL fetched for the binary payload. When IsArchive
	// is true this is the archive download URL; otherwise it is the raw
	// binary URL.
	PreferredURL string
	// PreferredName is the artifact name used in progress output.
	PreferredName string
	// ChecksumURL is the URL of the .sha256 file that pairs with PreferredURL.
	ChecksumURL string
	// IsArchive indicates whether PreferredURL points at a .tar.gz archive.
	// When true the downloaded payload must be extracted before installation.
	IsArchive bool
	// BinaryName is the name of the binary inside the archive, used for both
	// extraction and as the fallback artifact name when no archive exists.
	BinaryName string
}

// findReleaseAssetSet locates the best download asset for the given binary
// target. It prefers the .tar.gz archive when the release publishes one and
// falls back to the raw binary so older releases keep working.
func findReleaseAssetSet(release githubReleasePayload, binaryName string) (downloadAsset, error) {
	archiveName := binaryName + archiveAssetSuffix
	archiveChecksum := archiveName + ".sha256"
	rawChecksum := binaryName + ".sha256"

	var (
		archiveAsset, archiveChecksumAsset githubReleaseAsset
		binaryAsset, binaryChecksumAsset   githubReleaseAsset
	)
	for _, asset := range release.Assets {
		switch asset.Name {
		case archiveName:
			archiveAsset = asset
		case archiveChecksum:
			archiveChecksumAsset = asset
		case binaryName:
			binaryAsset = asset
		case rawChecksum:
			binaryChecksumAsset = asset
		}
	}

	archiveURL := strings.TrimSpace(archiveAsset.BrowserDownloadURL)
	archiveChecksumURL := strings.TrimSpace(archiveChecksumAsset.BrowserDownloadURL)
	if archiveURL != "" && archiveChecksumURL != "" {
		return downloadAsset{
			PreferredURL:  archiveURL,
			PreferredName: archiveName,
			ChecksumURL:   archiveChecksumURL,
			IsArchive:     true,
			BinaryName:    binaryName,
		}, nil
	}

	binaryURL := strings.TrimSpace(binaryAsset.BrowserDownloadURL)
	binaryChecksumURL := strings.TrimSpace(binaryChecksumAsset.BrowserDownloadURL)
	missing := make([]string, 0, 2)
	if binaryURL == "" {
		missing = append(missing, binaryName)
	}
	if binaryChecksumURL == "" {
		missing = append(missing, rawChecksum)
	}
	if len(missing) > 0 {
		return downloadAsset{}, fmt.Errorf("release is missing required asset(s): %s", strings.Join(missing, ", "))
	}

	return downloadAsset{
		PreferredURL:  binaryURL,
		PreferredName: binaryName,
		ChecksumURL:   binaryChecksumURL,
		IsArchive:     false,
		BinaryName:    binaryName,
	}, nil
}

// fetchAndExtractBinary downloads the configured asset, verifies its
// checksum, and (for archives) extracts the named binary. The returned bytes
// are the final binary ready to be written to disk.
func (r *commandRuntime) fetchAndExtractBinary(ctx context.Context, asset downloadAsset, progress io.Writer) ([]byte, error) {
	payload, err := r.downloadBinary(ctx, asset.PreferredURL, asset.PreferredName, progress)
	if err != nil {
		return nil, err
	}
	checksumText, err := r.downloadChecksum(ctx, asset.ChecksumURL)
	if err != nil {
		return nil, err
	}
	expected, err := parseChecksum(checksumText)
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(payload)
	if hex.EncodeToString(actual[:]) != expected {
		return nil, fmt.Errorf("downloaded %s checksum mismatch: expected %s, received %s", asset.PreferredName, expected, hex.EncodeToString(actual[:]))
	}
	if !asset.IsArchive {
		return payload, nil
	}
	binary, err := extractBinaryFromTarGz(payload, asset.BinaryName)
	if err != nil {
		return nil, fmt.Errorf("extract %s from %s: %w", asset.BinaryName, asset.PreferredName, err)
	}
	return binary, nil
}

// extractBinaryFromTarGz reads a gzipped tar archive in memory and returns
// the bytes of the entry whose base name matches binaryName. Other entries
// are ignored. Symlinks and absolute paths are rejected to prevent
// path-traversal attacks via crafted archives.
func extractBinaryFromTarGz(archiveBytes []byte, binaryName string) ([]byte, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		return nil, fmt.Errorf("open gzip stream: %w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar header: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if strings.HasPrefix(header.Name, "/") || strings.Contains(header.Name, "..") {
			return nil, fmt.Errorf("archive entry has unsafe path: %q", header.Name)
		}
		if path.Base(header.Name) != binaryName {
			continue
		}
		if header.Size > maxArchiveBinaryBytes {
			return nil, fmt.Errorf("archive entry %q exceeds %d-byte limit", header.Name, maxArchiveBinaryBytes)
		}
		data, err := io.ReadAll(io.LimitReader(tarReader, maxArchiveBinaryBytes))
		if err != nil {
			return nil, fmt.Errorf("read tar entry %q: %w", header.Name, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("archive does not contain entry %q", binaryName)
}

// concurrentProgressMux serializes progress writes from multiple goroutines
// onto a single underlying writer. It also tells download progress emitters to
// use compact summary mode so concurrent artifact downloads produce a small,
// readable set of whole-line updates instead of noisy interleaved carriage-
// return animations.
type concurrentProgressMux struct {
	mu sync.Mutex
	w  io.Writer
}

func newConcurrentProgressMux(w io.Writer) *concurrentProgressMux {
	return &concurrentProgressMux{w: w}
}

func (m *concurrentProgressMux) writer() io.Writer {
	if m == nil || m.w == nil {
		return nil
	}
	return &concurrentLineWriter{parent: m}
}

type concurrentLineWriter struct {
	parent *concurrentProgressMux
}

func (c *concurrentLineWriter) downloadProgressMode() downloadProgressMode {
	return downloadProgressModeSummary
}

func (c *concurrentLineWriter) Write(p []byte) (int, error) {
	if c == nil || c.parent == nil || c.parent.w == nil {
		return len(p), nil
	}
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()
	rewritten := bytesReplaceCR(p)
	if _, err := c.parent.w.Write(rewritten); err != nil {
		return 0, err
	}
	return len(p), nil
}

// bytesReplaceCR rewrites a payload so that lone carriage returns become
// newlines, which is what concurrent line-oriented progress output needs.
func bytesReplaceCR(input []byte) []byte {
	if len(input) == 0 {
		return input
	}
	if !bytes.ContainsRune(input, '\r') {
		return input
	}
	out := make([]byte, 0, len(input))
	for _, b := range input {
		if b == '\r' {
			out = append(out, '\n')
			continue
		}
		out = append(out, b)
	}
	return out
}
