package cliapp

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/term"
)

type downloadProgressFactory interface {
	io.Writer
	newTracker(name string, total int64) downloadProgressTracker
	close()
}

type downloadProgressTracker interface {
	wrap(io.ReadCloser) io.ReadCloser
	finish(success bool)
}

func ensureDownloadProgressFactory(w io.Writer) (downloadProgressFactory, bool) {
	if w == nil {
		return nil, false
	}
	if factory, ok := w.(downloadProgressFactory); ok {
		return factory, false
	}
	if writerIsTerminal(w) {
		return newMPBDownloadProgressFactory(w), true
	}
	return newSummaryDownloadProgressFactory(w), true
}

func newConcurrentProgressMux(w io.Writer) downloadProgressFactory {
	factory, _ := ensureDownloadProgressFactory(w)
	return factory
}

type fdWriter interface {
	Fd() uintptr
}

func writerIsTerminal(w io.Writer) bool {
	file, ok := w.(fdWriter)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

type mpbDownloadProgressFactory struct {
	progress *mpb.Progress
}

func newMPBDownloadProgressFactory(w io.Writer) *mpbDownloadProgressFactory {
	return &mpbDownloadProgressFactory{
		progress: mpb.New(
			mpb.WithOutput(w),
			mpb.WithAutoRefresh(),
			mpb.WithRefreshRate(80*time.Millisecond),
			mpb.WithWidth(downloadProgressBarWidth),
		),
	}
}

func (f *mpbDownloadProgressFactory) Write(p []byte) (int, error) {
	if f == nil || f.progress == nil {
		return len(p), nil
	}
	return f.progress.Write(p)
}

func (f *mpbDownloadProgressFactory) newTracker(name string, total int64) downloadProgressTracker {
	label := progressDisplayName(name)
	if total > 0 {
		bar := f.progress.New(0,
			mpb.BarStyle().Lbound("[").Filler("#").Tip("#").Padding("-").Rbound("]"),
			mpb.PrependDecorators(
				decor.Name(label, decor.WCSyncSpaceR),
			),
			mpb.AppendDecorators(
				decor.Percentage(decor.WCSyncSpace),
				decor.CountersKibiByte("% d / % d", decor.WCSyncSpace),
			),
		)
		bar.SetTotal(total, false)
		return &mpbDownloadProgressTracker{bar: bar, total: total, totalKnown: true}
	}
	bar := f.progress.AddSpinner(0,
		mpb.PrependDecorators(
			decor.Name(label, decor.WCSyncSpaceR),
		),
		mpb.AppendDecorators(
			decor.CurrentKibiByte("% d downloaded"),
		),
	)
	return &mpbDownloadProgressTracker{bar: bar}
}

func (f *mpbDownloadProgressFactory) close() {
	if f == nil || f.progress == nil {
		return
	}
	f.progress.Wait()
}

type mpbDownloadProgressTracker struct {
	bar        *mpb.Bar
	reader     io.ReadCloser
	total      int64
	totalKnown bool
}

func (t *mpbDownloadProgressTracker) wrap(r io.ReadCloser) io.ReadCloser {
	if t == nil || t.bar == nil {
		return r
	}
	t.reader = t.bar.ProxyReader(r)
	return t.reader
}

func (t *mpbDownloadProgressTracker) finish(success bool) {
	if t == nil || t.bar == nil {
		return
	}
	if !success {
		if t.totalKnown {
			current := t.bar.Current()
			if current >= t.total {
				t.bar.SetTotal(current+1, false)
			}
		}
		if !t.bar.Aborted() && !t.bar.Completed() {
			t.bar.Abort(false)
		}
		return
	}
	if !t.totalKnown {
		t.bar.SetTotal(-1, true)
		return
	}
	t.bar.SetTotal(t.total, true)
}

type summaryDownloadProgressFactory struct {
	mu sync.Mutex
	w  io.Writer
}

func newSummaryDownloadProgressFactory(w io.Writer) *summaryDownloadProgressFactory {
	return &summaryDownloadProgressFactory{w: w}
}

func (f *summaryDownloadProgressFactory) Write(p []byte) (int, error) {
	if f == nil || f.w == nil {
		return len(p), nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.w.Write(bytesReplaceCR(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (f *summaryDownloadProgressFactory) newTracker(name string, total int64) downloadProgressTracker {
	return newSummaryDownloadProgress(f, name, total)
}

func (f *summaryDownloadProgressFactory) close() {}

type summaryDownloadProgress struct {
	w        io.Writer
	name     string
	total    int64
	read     int64
	last     time.Time
	finished bool
}

func newSummaryDownloadProgress(w io.Writer, name string, total int64) *summaryDownloadProgress {
	p := &summaryDownloadProgress{w: w, name: progressDisplayName(name), total: total}
	p.print(true)
	return p
}

func (p *summaryDownloadProgress) wrap(r io.ReadCloser) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{Reader: io.TeeReader(r, p), Closer: r}
}

func (p *summaryDownloadProgress) Write(data []byte) (int, error) {
	p.read += int64(len(data))
	return len(data), nil
}

func (p *summaryDownloadProgress) finish(success bool) {
	if p.finished {
		return
	}
	p.finished = true
	if !success {
		return
	}
	p.print(false)
}

func (p *summaryDownloadProgress) print(initial bool) {
	p.last = time.Now()
	var line string
	if initial {
		line = fmt.Sprintf("Downloading %s…", p.name)
	} else if p.total > 0 {
		line = fmt.Sprintf("Downloaded %s (%s)", p.name, formatDownloadBytes(p.total))
	} else if p.read > 0 {
		line = fmt.Sprintf("Downloaded %s (%s)", p.name, formatDownloadBytes(p.read))
	} else {
		line = fmt.Sprintf("Downloaded %s", p.name)
	}
	_, _ = fmt.Fprintln(p.w, line)
}

func progressDisplayName(name string) string {
	trimmed := strings.TrimSpace(name)
	trimmed = strings.TrimSuffix(trimmed, archiveAssetSuffix)
	switch {
	case strings.HasPrefix(trimmed, "looper-"):
		return "looper"
	case strings.HasPrefix(trimmed, "looperd-"):
		return "looperd"
	default:
		return trimmed
	}
}

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

const downloadProgressBarWidth = 20

func formatDownloadBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	divisor := int64(unit)
	unitIndex := 0
	for n := value / unit; n >= unit && unitIndex < 3; n /= unit {
		divisor *= unit
		unitIndex++
	}
	number := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", float64(value)/float64(divisor)), "0"), ".")
	return fmt.Sprintf("%s %ciB", number, "KMGT"[unitIndex])
}
