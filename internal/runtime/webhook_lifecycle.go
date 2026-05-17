package runtime

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

const adoptedForwarderPollInterval = 2 * time.Second

type forwarderExitClass string

const (
	forwarderExitTransient forwarderExitClass = "transient"
	forwarderExitTerminal  forwarderExitClass = "terminal"
)

type forwarderExitClassification struct {
	Class          forwarderExitClass
	MatchedPattern string
}

func classifyForwarderExit(stderrTail []string, exitErr error) forwarderExitClassification {
	text := strings.ToLower(strings.Join(stderrTail, "\n"))
	patterns := []string{
		"Hook already exists on this repository",
		"HTTP 401",
		"authentication required",
		"gh auth login",
		"HTTP 403",
		"Resource not accessible by integration",
		"HTTP 404",
	}
	for _, pattern := range patterns {
		if strings.Contains(text, strings.ToLower(pattern)) {
			return forwarderExitClassification{Class: forwarderExitTerminal, MatchedPattern: pattern}
		}
	}
	if strings.Contains(text, "validation failed") && strings.Contains(text, "hook") {
		return forwarderExitClassification{Class: forwarderExitTerminal, MatchedPattern: "Validation Failed"}
	}
	return forwarderExitClassification{Class: forwarderExitTransient}
}

func commandFingerprint(ghPath, repo string, events []string, endpoint string) (string, string) {
	canonicalEvents := canonicalWebhookEvents(events)
	parts := []string{strings.TrimSpace(ghPath), strings.TrimSpace(repo), strings.Join(canonicalEvents, ","), strings.TrimSpace(endpoint)}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:]), strings.Join(canonicalEvents, ",")
}

func canonicalWebhookEvents(events []string) []string {
	canonical := make([]string, 0, len(events))
	seen := map[string]struct{}{}
	for _, event := range events {
		event = strings.ToLower(strings.TrimSpace(event))
		if event == "" {
			continue
		}
		if _, ok := seen[event]; ok {
			continue
		}
		seen[event] = struct{}{}
		canonical = append(canonical, event)
	}
	sort.Strings(canonical)
	return canonical
}

func newDaemonID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("pid-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func webhookForwarderLockPath(cfgStorageDBPath string) string {
	dbPath := strings.TrimSpace(cfgStorageDBPath)
	if dbPath != "" {
		if absPath, err := filepath.Abs(dbPath); err == nil {
			dbPath = absPath
		}
	}
	dir := filepath.Dir(dbPath)
	if dir == "." || dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".looper")
		}
	}
	return filepath.Join(dir, "looperd.lock")
}

type daemonLock struct {
	path string
	file *os.File
}

func acquireDaemonLock(path, daemonID string, now time.Time) (*daemonLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path)
		_ = file.Close()
		return nil, fmt.Errorf("another looperd already holds %s (%s): %w", path, strings.TrimSpace(string(holder)), err)
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "pid=%d daemon_id=%s started=%s\n", os.Getpid(), daemonID, now.UTC().Format(time.RFC3339)); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &daemonLock{path: path, file: file}, nil
}

func (l *daemonLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

type processProbe interface {
	IsAlive(pid int) (bool, error)
	StartTime(pid int) (int64, error)
	Argv(pid int) ([]string, error)
	ExecutablePath(pid int) (string, error)
}

type defaultProcessProbe struct{}

func (defaultProcessProbe) IsAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func (defaultProcessProbe) StartTime(pid int) (int64, error) {
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return 0, err
		}
		fields := strings.Fields(string(stat))
		if len(fields) < 22 {
			return 0, fmt.Errorf("unexpected /proc stat shape")
		}
		start, err := strconv.ParseInt(fields[21], 10, 64)
		if err != nil {
			return 0, err
		}
		return start, nil
	}
	return psProcessStart(pid)
}

func (defaultProcessProbe) Argv(pid int) ([]string, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(string(data), "\x00")
		if trimmed == "" {
			return nil, nil
		}
		return strings.Split(trimmed, "\x00"), nil
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(strings.TrimSpace(string(out))), nil
}

func (defaultProcessProbe) ExecutablePath(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	}
	argv, err := (defaultProcessProbe{}).Argv(pid)
	if err != nil || len(argv) == 0 {
		return "", err
	}
	return argv[0], nil
}

func psProcessStart(pid int) (int64, error) {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return 0, fmt.Errorf("empty process start")
	}
	parsed, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", value, time.UTC)
	if err != nil {
		return 0, err
	}
	return parsed.UnixNano(), nil
}

type adoptedForwarderProcess struct {
	pid          int
	processStart int64
	probe        processProbe
	pollInterval time.Duration
}

func (p *adoptedForwarderProcess) Wait() error {
	interval := p.pollInterval
	if interval <= 0 {
		interval = adoptedForwarderPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		alive, err := p.probe.IsAlive(p.pid)
		if err != nil {
			continue
		}
		if !alive {
			return nil
		}
		start, err := p.probe.StartTime(p.pid)
		if err != nil {
			continue
		}
		if start != p.processStart {
			return fmt.Errorf("adopted process identity changed")
		}
	}
	return nil
}

func (p *adoptedForwarderProcess) Stop() error { return syscall.Kill(p.pid, syscall.SIGTERM) }
func (p *adoptedForwarderProcess) Kill() error { return syscall.Kill(p.pid, syscall.SIGKILL) }

func webhookForwarderRecordFromState(repo string, pid int, processStart int64, command []string, daemonID string, now time.Time) storage.WebhookForwarderRecord {
	ghPath, endpoint, events := commandIdentityParts(command)
	fingerprint, eventsCSV := commandFingerprint(ghPath, repo, events, endpoint)
	nanos := now.UTC().UnixNano()
	return storage.WebhookForwarderRecord{Repo: repo, PID: int64(pid), ProcessStart: processStart, Fingerprint: fingerprint, Endpoint: endpoint, Events: eventsCSV, GHPath: ghPath, DaemonID: daemonID, SpawnedAt: nanos, UpdatedAt: nanos}
}

func commandIdentityParts(command []string) (string, string, []string) {
	ghPath := ""
	endpoint := ""
	events := []string{}
	if len(command) > 0 {
		ghPath = command[0]
	}
	for i := 0; i < len(command); i++ {
		arg := command[i]
		if strings.HasPrefix(arg, "--url=") {
			endpoint = strings.TrimPrefix(arg, "--url=")
		} else if arg == "--url" && i+1 < len(command) {
			endpoint = command[i+1]
			i++
		}
		if strings.HasPrefix(arg, "--events=") {
			events = strings.Split(strings.TrimPrefix(arg, "--events="), ",")
		} else if arg == "--events" && i+1 < len(command) {
			events = strings.Split(command[i+1], ",")
			i++
		}
	}
	return ghPath, endpoint, events
}

func argvMatchesWebhookForward(argv []string, repo string, events []string, endpoint string) bool {
	if len(argv) < 3 || argv[1] != "webhook" || argv[2] != "forward" {
		return false
	}
	foundRepo := ""
	foundURL := ""
	foundEvents := []string{}
	for i := 3; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case strings.HasPrefix(arg, "--repo="):
			foundRepo = strings.TrimPrefix(arg, "--repo=")
		case arg == "--repo" && i+1 < len(argv):
			foundRepo = argv[i+1]
			i++
		case strings.HasPrefix(arg, "--url="):
			foundURL = strings.TrimPrefix(arg, "--url=")
		case arg == "--url" && i+1 < len(argv):
			foundURL = argv[i+1]
			i++
		case strings.HasPrefix(arg, "--events="):
			foundEvents = strings.Split(strings.TrimPrefix(arg, "--events="), ",")
		case arg == "--events" && i+1 < len(argv):
			foundEvents = strings.Split(argv[i+1], ",")
			i++
		}
	}
	return foundRepo == repo && foundURL == endpoint && strings.Join(canonicalWebhookEvents(foundEvents), ",") == strings.Join(canonicalWebhookEvents(events), ",")
}
