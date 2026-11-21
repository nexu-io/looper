package statichtml

import (
	"bytes"
	"context"
	"crypto/sha256"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func encodedTestPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 0x63, G: 0xfe, B: 0x13, A: 0xff})
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestSanitizeRejectsActiveAndNetworkContent(t *testing.T) {
	bad := []string{
		`<script>alert(1)</script>`,
		`<div onclick="fetch('https://evil')">x</div>`,
		`<iframe src="file:///etc/passwd"></iframe>`,
		`<style>.x{background:url(https://evil)}</style>`,
		`<div style="background: url(data:image/png,x)">x</div>`,
		`<style>.x{background:u\72l(https://evil)}</style>`,
		`<meta http-equiv="refresh" content="0;url=https://evil">`,
		`<meta name="viewport" content="width=device-width;url=https://evil">`,
	}
	for _, raw := range bad {
		if _, err := Sanitize(raw); err == nil {
			t.Fatalf("Sanitize(%q) unexpectedly accepted unsafe input", raw)
		}
	}
	if _, err := Sanitize(`<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><style>body{font-family:sans-serif;background:#fff}.card{padding:20px;box-shadow:0 8px 24px rgba(15,23,42,.08)}</style></head><body><aside>导航</aside><main><div class="card">错误 ID<br><button>导出</button></div></main></body></html>`); err != nil {
		t.Fatalf("safe prototype rejected: %v", err)
	}
}

func TestControlledBrowserArgsDisableScriptAndExternalNetwork(t *testing.T) {
	args := strings.Join(controlledBrowserArgs("/tmp/profile", "/tmp/out.png", "http://127.0.0.1:1234/token", 900, 600), " ")
	for _, want := range []string{"--disable-javascript", "--proxy-server=http://127.0.0.1:9", "MAP * ~NOTFOUND, EXCLUDE 127.0.0.1", "--user-data-dir=/tmp/profile", "--screenshot=/tmp/out.png", "http://127.0.0.1:1234/token"} {
		if !strings.Contains(args, want) {
			t.Fatalf("controlled args missing %q: %s", want, args)
		}
	}
}

func TestCompletePNGSnapshotRequiresDecodedTerminalIEND(t *testing.T) {
	complete := encodedTestPNG(t)
	path := filepath.Join(t.TempDir(), "option.png")
	if err := os.WriteFile(path, complete, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := completePNGSnapshot(path)
	if !ok || snapshot.Size != int64(len(complete)) {
		t.Fatalf("complete PNG rejected: snapshot=%#v ok=%v", snapshot, ok)
	}

	bad := [][]byte{
		complete[:8],
		complete[:len(complete)-1],
		append(append([]byte{}, complete...), 0),
	}
	corrupt := append([]byte{}, complete...)
	corrupt[len(corrupt)-1] ^= 0xff // corrupt IEND CRC
	bad = append(bad, corrupt)
	for index, data := range bad {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := completePNGSnapshot(path); ok {
			t.Fatalf("invalid PNG case %d was accepted", index)
		}
	}
}

func TestPNGStabilityRequiresTwoIdenticalCompleteObservations(t *testing.T) {
	first := pngSnapshot{Size: 10, Digest: [sha256.Size]byte{1}}
	changed := pngSnapshot{Size: 11, Digest: [sha256.Size]byte{2}}
	var stability pngStability
	if stability.observe(first, true) {
		t.Fatal("first complete observation must not be stable")
	}
	if stability.observe(changed, true) {
		t.Fatal("changed complete observation must restart stability")
	}
	if !stability.observe(changed, true) {
		t.Fatal("second identical complete observation must be stable")
	}
	if stability.observe(pngSnapshot{}, false) {
		t.Fatal("incomplete observation cannot be stable")
	}
	if stability.observe(changed, true) {
		t.Fatal("incomplete observation must reset stability")
	}
}

func TestRenderControlledChrome(t *testing.T) {
	browser, err := DetectBrowser("")
	if err != nil {
		t.Skip(err)
	}
	out := filepath.Join(t.TempDir(), "option.png")
	manifest, err := Render(context.Background(), `<style>body{margin:0;background:#fff;font-family:sans-serif}.card{margin:80px;padding:32px;border:1px solid #ddd}</style><main><div class="card"><h1>导出设计稿</h1><button>导出</button></div></main>`, out, Options{BrowserPath: browser, Width: 900, Height: 600, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Width != 900 || manifest.Height != 600 {
		t.Fatalf("manifest = %#v", manifest)
	}
	if manifest.PNGBytes <= 0 || len(manifest.PNGSHA256) == 0 {
		t.Fatalf("manifest missing PNG evidence: %#v", manifest)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Fatalf("png missing: %v", err)
	}
}
