// Package feishubridge runs the actual upstream bridge, with a small local
// dispatch adapter. It does not reimplement cards, streaming or conversations.
package feishubridge

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const Revision = "a9ff5d2d028522b41d7e8b1a4837e83be5f2f6d3"
const archiveSHA256 = "8332dbf721d7242dc4d02cb034d585bcbe83af77bde8cf03175c4b4c86f32251"

//go:embed assets/*
var assets embed.FS

// Install caches a pinned upstream source distribution and builds its real
// runtime. Neither node_modules nor the source tree is added to Looper's Git tree.
func Install(ctx context.Context, dir string, log io.Writer) (string, error) {
	entry := filepath.Join(dir, "dist-looper", "looper-entry.js")
	hash := sha256.New()
	_, _ = io.WriteString(hash, Revision)
	files, err := assets.ReadDir("assets")
	if err != nil {
		return "", err
	}
	for _, file := range files {
		data, err := assets.ReadFile("assets/" + file.Name())
		if err != nil {
			return "", err
		}
		_, _ = hash.Write(data)
	}
	buildID := hex.EncodeToString(hash.Sum(nil))
	if saved, err := os.ReadFile(filepath.Join(dir, "looper-build-id")); err == nil && string(saved) == buildID {
		if _, err := os.Stat(entry); err == nil {
			return entry, nil
		}
	}
	if _, err := exec.LookPath("node"); err != nil {
		return "", fmt.Errorf("feishu-codex-bridge needs Node.js 20 or newer: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	fmt.Fprintln(log, "Preparing pinned feishu-codex-bridge runtime", Revision)
	client := http.Client{Timeout: 2 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://codeload.github.com/modelzen/feishu-codex-bridge/tar.gz/"+Revision, nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download bridge: HTTP %d", res.StatusCode)
	}
	archive, err := os.CreateTemp(dir, "upstream-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(archive, digest), io.LimitReader(res.Body, 64<<20)); err != nil {
		return "", err
	}
	if hex.EncodeToString(digest.Sum(nil)) != archiveSHA256 {
		return "", fmt.Errorf("upstream bridge archive does not match pinned SHA256")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err := unpack(archive, dir); err != nil {
		return "", err
	}
	patch, _ := assets.ReadFile("assets/upstream.patch")
	patchCmd := exec.CommandContext(ctx, "git", "apply", "-")
	patchCmd.Dir, patchCmd.Stdin = dir, strings.NewReader(string(patch))
	patchCmd.Stdout, patchCmd.Stderr = log, log
	if err := patchCmd.Run(); err != nil {
		return "", fmt.Errorf("apply bridge dispatch seam: %w", err)
	}
	for target, asset := range map[string]string{
		"src/bot/external.ts": "external.ts", "src/looper-entry.ts": "looper-entry.ts", "tsup.looper.config.ts": "tsup.looper.config.ts",
	} {
		data, _ := assets.ReadFile("assets/" + asset)
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(target)), data, 0600); err != nil {
			return "", err
		}
	}
	for _, args := range [][]string{
		{"ci", "--ignore-scripts", "--no-audit", "--no-fund"},
		{"exec", "--", "tsup", "--config", "tsup.looper.config.ts"},
	} {
		cmd := exec.CommandContext(ctx, "npm", args...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("build bridge runtime: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "looper-build-id"), []byte(buildID), 0600); err != nil {
		return "", err
	}
	return entry, nil
}

func unpack(input io.Reader, dir string) error {
	gzipReader, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		_, name, ok := strings.Cut(header.Name, "/")
		if !ok || name == "" {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("invalid upstream archive path")
		}
		// Keep the complete runtime and its license, not upstream screenshots.
		if !strings.HasPrefix(name, "src/") && name != "package.json" && name != "package-lock.json" && name != "tsconfig.json" && name != "LICENSE" && name != "README.md" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		} else if header.Typeflag == tar.TypeReg {
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
}
