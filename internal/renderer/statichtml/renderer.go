// Package statichtml renders deliberately constrained, network-isolated HTML
// prototypes. It never executes an agent-provided command or serves the worktree.
package statichtml

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"image/png"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	DefaultWidth  = 1200
	DefaultHeight = 800
)

var allowedElements = map[string]bool{
	"html": true, "head": true, "body": true, "title": true, "meta": true, "style": true,
	"main": true, "section": true, "aside": true, "header": true, "footer": true, "nav": true,
	"div": true, "span": true, "p": true, "br": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "ul": true, "ol": true, "li": true,
	"strong": true, "em": true, "small": true, "code": true, "button": true,
	"label": true,
}

var allowedAttributes = map[string]bool{
	"class": true, "id": true, "style": true, "role": true, "aria-label": true,
}

var forbiddenCSS = []string{
	"url(", "@import", "expression(", "behavior:", "-moz-binding", "javascript:",
	"file:", "http:", "https:", "data:",
}

type Options struct {
	BrowserPath string
	Width       int
	Height      int
	Timeout     time.Duration
}

type Manifest struct {
	BrowserPath string `json:"browserPath"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	PNGBytes    int64  `json:"pngBytes"`
	PNGSHA256   string `json:"pngSha256"`
	Completion  string `json:"completion"`
	GeneratedAt string `json:"generatedAt"`
}

type pngSnapshot struct {
	Size   int64
	Digest [sha256.Size]byte
}

type pngStability struct {
	previous *pngSnapshot
}

func (stability *pngStability) observe(snapshot pngSnapshot, complete bool) bool {
	if !complete {
		stability.previous = nil
		return false
	}
	if stability.previous != nil && *stability.previous == snapshot {
		return true
	}
	observed := snapshot
	stability.previous = &observed
	return false
}

// Sanitize parses and rejects anything outside the small static subset. Rejecting
// (rather than silently dropping) keeps the design question blocked on unsafe input.
func Sanitize(raw string) (string, error) {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("parse html: %w", err)
	}
	var visit func(*html.Node) error
	visit = func(node *html.Node) error {
		if node.Type == html.ElementNode {
			name := strings.ToLower(node.Data)
			if !allowedElements[name] {
				return fmt.Errorf("unsafe html element <%s>", name)
			}
			if name == "meta" {
				if err := validateMeta(node.Attr); err != nil {
					return err
				}
				goto visitChildren
			}
			for _, attr := range node.Attr {
				key := strings.ToLower(attr.Key)
				if strings.HasPrefix(key, "on") || !allowedAttributes[key] {
					return fmt.Errorf("unsafe attribute %s on <%s>", key, name)
				}
				if key == "style" {
					if err := validateCSS(attr.Val); err != nil {
						return err
					}
				}
			}
			if name == "style" {
				for child := node.FirstChild; child != nil; child = child.NextSibling {
					if child.Type != html.TextNode {
						return fmt.Errorf("style element must contain text only")
					}
					if err := validateCSS(child.Data); err != nil {
						return err
					}
				}
			}
		}
	visitChildren:
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(doc); err != nil {
		return "", err
	}
	var out strings.Builder
	if err := html.Render(&out, doc); err != nil {
		return "", fmt.Errorf("render sanitized html: %w", err)
	}
	return out.String(), nil
}

var safeViewportContent = regexp.MustCompile(`(?i)^[a-z0-9.=, _-]+$`)

func validateMeta(attrs []html.Attribute) error {
	if len(attrs) == 1 && strings.EqualFold(attrs[0].Key, "charset") && strings.EqualFold(strings.TrimSpace(attrs[0].Val), "utf-8") {
		return nil
	}
	values := map[string]string{}
	for _, attr := range attrs {
		key := strings.ToLower(strings.TrimSpace(attr.Key))
		if key != "name" && key != "content" {
			return fmt.Errorf("unsafe attribute %s on <meta>", key)
		}
		values[key] = strings.TrimSpace(attr.Val)
	}
	if len(values) == 2 && strings.EqualFold(values["name"], "viewport") && values["content"] != "" && safeViewportContent.MatchString(values["content"]) {
		return nil
	}
	return fmt.Errorf("unsafe <meta>; only utf-8 charset or a static viewport declaration is allowed")
}

func validateCSS(css string) error {
	lower := strings.ToLower(strings.ReplaceAll(css, " ", ""))
	for _, forbidden := range forbiddenCSS {
		if strings.Contains(lower, forbidden) {
			return fmt.Errorf("unsafe css construct %q", forbidden)
		}
	}
	// Color functions are common in static mockups and carry no fetch/execution
	// capability when restricted to numeric channels. Remove only those known-safe
	// calls before rejecting every other function/escape token (url, var, calc,
	// obfuscated identifiers, etc.).
	withoutSafeColors := safeCSSColorFunction.ReplaceAllString(lower, "")
	if strings.ContainsAny(withoutSafeColors, `\@()/<>`) {
		return fmt.Errorf("unsafe css token")
	}
	return nil
}

var safeCSSColorFunction = regexp.MustCompile(`(?i)(?:rgb|rgba|hsl|hsla)\([0-9.,%+\-]+\)`)

func DetectBrowser(override string) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		info, err := os.Stat(override)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("browser executable %q is unavailable", override)
		}
		return override, nil
	}
	candidates := []string{"google-chrome", "chromium", "chromium-browser"}
	if runtime.GOOS == "darwin" {
		candidates = append([]string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}, candidates...)
	}
	for _, candidate := range candidates {
		if filepath.IsAbs(candidate) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("Chrome/Chromium was not found")
}

// Render writes one PNG to outputPath. outputPath must be chosen by Looper under its
// runtime artifact root; the browser is only given a fresh temporary profile and a
// tokenized loopback URL containing sanitized bytes.
func Render(ctx context.Context, raw, outputPath string, options Options) (Manifest, error) {
	safe, err := Sanitize(raw)
	if err != nil {
		return Manifest{}, err
	}
	browser, err := DetectBrowser(options.BrowserPath)
	if err != nil {
		return Manifest{}, err
	}
	width, height := options.Width, options.Height
	if width <= 0 {
		width = DefaultWidth
	}
	if height <= 0 {
		height = DefaultHeight
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return Manifest{}, err
	}
	profile, err := os.MkdirTemp("", "looper-render-profile-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(profile)
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return Manifest{}, err
	}
	token := hex.EncodeToString(tokenBytes)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return Manifest{}, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+token, func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/"+token {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'none'; font-src 'none'; script-src 'none'; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'")
		_, _ = w.Write([]byte(safe))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	renderCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := "http://" + listener.Addr().String() + "/" + token
	args := controlledBrowserArgs(profile, outputPath, url, width, height)
	cmd := exec.Command(browser, args...)
	var commandOutput bytes.Buffer
	cmd.Stdout = &commandOutput
	cmd.Stderr = &commandOutput
	if err := cmd.Start(); err != nil {
		return Manifest{}, fmt.Errorf("start controlled Chrome: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var stability pngStability
	var stableSnapshot *pngSnapshot
	completion := "process_exit"
	for {
		select {
		case waitErr := <-done:
			if _, ok := completePNGSnapshot(outputPath); ok {
				goto screenshotReady
			}
			return Manifest{}, fmt.Errorf("controlled Chrome exited before screenshot: %v: %.400s", waitErr, commandOutput.String())
		case <-ticker.C:
			snapshot, ok := completePNGSnapshot(outputPath)
			if stability.observe(snapshot, ok) {
				// Some macOS Chrome builds keep updater/background processes alive after
				// writing the screenshot. Only kill after two identical observations of a
				// fully decoded PNG whose CRC-valid terminal chunk is IEND. A signature or
				// partially flushed file must never be treated as completion.
				completion = "stable_png"
				observed := snapshot
				stableSnapshot = &observed
				_ = cmd.Process.Kill()
				<-done
				goto screenshotReady
			}
		case <-renderCtx.Done():
			_ = cmd.Process.Kill()
			<-done
			return Manifest{}, fmt.Errorf("controlled Chrome render timed out: %.400s", commandOutput.String())
		}
	}

screenshotReady:
	snapshot, ok := completePNGSnapshot(outputPath)
	if !ok {
		return Manifest{}, fmt.Errorf("renderer output is not a complete PNG with terminal IEND")
	}
	if stableSnapshot != nil && snapshot != *stableSnapshot {
		return Manifest{}, fmt.Errorf("renderer PNG changed after stable completion was observed")
	}
	return Manifest{
		BrowserPath: browser,
		Width:       width,
		Height:      height,
		PNGBytes:    snapshot.Size,
		PNGSHA256:   hex.EncodeToString(snapshot.Digest[:]),
		Completion:  completion,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func controlledBrowserArgs(profile, outputPath, url string, width, height int) []string {
	return []string{
		"--headless=new", "--disable-gpu", "--disable-extensions", "--disable-background-networking",
		"--disable-component-update", "--disable-sync", "--no-first-run", "--no-default-browser-check",
		"--metrics-recording-only", "--safebrowsing-disable-auto-update", "--hide-scrollbars", "--disable-javascript",
		// Defense in depth beyond the strict sanitizer/CSP: every non-loopback HTTP(S)
		// request is sent to a closed local proxy, while the host resolver also fails
		// every hostname except the tokenized 127.0.0.1 origin.
		"--proxy-server=http://127.0.0.1:9",
		"--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1",
		"--user-data-dir=" + profile, fmt.Sprintf("--window-size=%d,%d", width, height),
		"--screenshot=" + outputPath, url,
	}
}

func completePNGSnapshot(path string) (pngSnapshot, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pngSnapshot{}, false
	}
	if !hasTerminalPNGEnd(data) {
		return pngSnapshot{}, false
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return pngSnapshot{}, false
	}
	return pngSnapshot{Size: int64(len(data)), Digest: sha256.Sum256(data)}, true
}

// hasTerminalPNGEnd validates every chunk boundary and CRC, and requires the
// zero-length IEND chunk to be the final bytes in the file. This deliberately
// rejects signature-only, partially flushed, corrupt, and trailing-data files.
func hasTerminalPNGEnd(data []byte) bool {
	const signature = "\x89PNG\r\n\x1a\n"
	if len(data) < len(signature)+12 || string(data[:len(signature)]) != signature {
		return false
	}
	for offset := uint64(len(signature)); ; {
		if offset+12 > uint64(len(data)) {
			return false
		}
		length := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		chunkEnd := offset + 12 + length
		if chunkEnd > uint64(len(data)) {
			return false
		}
		typeStart := offset + 4
		dataEnd := typeStart + 4 + length
		storedCRC := binary.BigEndian.Uint32(data[dataEnd : dataEnd+4])
		if crc32.ChecksumIEEE(data[typeStart:dataEnd]) != storedCRC {
			return false
		}
		chunkType := string(data[typeStart : typeStart+4])
		if chunkType == "IEND" {
			return length == 0 && chunkEnd == uint64(len(data))
		}
		offset = chunkEnd
	}
}
