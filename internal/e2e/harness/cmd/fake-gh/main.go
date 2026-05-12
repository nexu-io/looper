package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	envFakeGHMode        = "LOOPER_E2E_FAKE_GH_MODE"
	envFakeGHArtifactDir = "LOOPER_E2E_FAKE_GH_ARTIFACT_DIR"
	envFakeGHSchemaPath  = "LOOPER_E2E_FAKE_GH_SCHEMA_PATH"
	envFakeGHStatePath   = "LOOPER_E2E_FAKE_GH_STATE_PATH"
	envFakeGHRecordPath  = "LOOPER_E2E_FAKE_GH_RECORD_PATH"
)

type schema struct {
	JSONFieldAllowlist map[string][]string `json:"jsonFieldAllowlist"`
}

type response struct {
	Stdout   json.RawMessage `json:"stdout,omitempty"`
	Stderr   string          `json:"stderr,omitempty"`
	ExitCode int             `json:"exitCode,omitempty"`
}

type state struct {
	Commands map[string]response        `json:"commands,omitempty"`
	Routes   map[string]json.RawMessage `json:"routes,omitempty"`
	GraphQL  map[string]json.RawMessage `json:"graphql,omitempty"`
}

type invocation struct {
	Timestamp string            `json:"timestamp"`
	CWD       string            `json:"cwd"`
	Argv      []string          `json:"argv"`
	Stdin     string            `json:"stdin"`
	Env       map[string]string `json:"env"`
	Mode      string            `json:"mode"`
}

func main() {
	mode := strings.TrimSpace(os.Getenv(envFakeGHMode))
	if mode == "" {
		mode = "strict"
	}
	artifactDir := strings.TrimSpace(os.Getenv(envFakeGHArtifactDir))
	if artifactDir == "" {
		artifactDir = "."
	}
	_ = os.MkdirAll(artifactDir, 0o755)
	stdin, _ := io.ReadAll(os.Stdin)
	_ = appendJSONL(filepath.Join(artifactDir, "invocations.jsonl"), invocation{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		CWD:       mustGetwd(),
		Argv:      os.Args[1:],
		Stdin:     string(stdin),
		Env:       collectEnv(),
		Mode:      mode,
	})
	schemaDoc, err := loadSchema(strings.TrimSpace(os.Getenv(envFakeGHSchemaPath)))
	if err != nil && mode == "strict" {
		fatalf(2, "load fake-gh schema: %v\n", err)
	}
	st, err := loadState(strings.TrimSpace(os.Getenv(envFakeGHStatePath)))
	if err != nil {
		fatalf(2, "load fake-gh state: %v\n", err)
	}
	if mode == "record" {
		_ = appendJSONL(strings.TrimSpace(os.Getenv(envFakeGHRecordPath)), map[string]any{"argv": os.Args[1:], "stdin": string(stdin)})
	}
	if err := dispatch(mode, schemaDoc, st, string(stdin)); err != nil {
		fatalf(1, "%s\n", err.Error())
	}
}

func dispatch(mode string, schemaDoc schema, st state, stdin string) error {
	key := commandKey(os.Args[1:])
	if resp, ok := st.Commands[key]; ok {
		return emitResponse(resp)
	}
	if strings.HasPrefix(key, "api") {
		return handleAPI(mode, st, stdin)
	}
	switch key {
	case "issue list", "pr list", "pr view":
		fields := requestedJSONFields(os.Args[1:])
		allowed := schemaDoc.JSONFieldAllowlist[key]
		if len(allowed) == 0 && mode == "strict" {
			return fmt.Errorf("missing fake-gh allowlist for %s", key)
		}
		if err := validateFields(key, fields, allowed); err != nil {
			return err
		}
		return emitDefaultJSON(key, fields)
	default:
		if mode == "strict" {
			return fmt.Errorf("unsupported fake-gh command: %s", strings.Join(os.Args[1:], " "))
		}
		_, _ = fmt.Fprintln(os.Stdout, "{}")
		return nil
	}
}

func handleAPI(mode string, st state, stdin string) error {
	args := os.Args[1:]
	if len(args) >= 2 && args[1] == "graphql" {
		operation := graphqlOperation(args, stdin)
		if payload, ok := st.GraphQL[operation]; ok {
			_, _ = os.Stdout.Write(payload)
			if len(payload) == 0 || payload[len(payload)-1] != '\n' {
				_, _ = fmt.Fprintln(os.Stdout)
			}
			return nil
		}
		_, _ = fmt.Fprintln(os.Stdout, `{"data":{}}`)
		return nil
	}
	route := firstNonFlag(args[1:])
	if payload, ok := st.Routes[route]; ok {
		_, _ = os.Stdout.Write(payload)
		if len(payload) == 0 || payload[len(payload)-1] != '\n' {
			_, _ = fmt.Fprintln(os.Stdout)
		}
		return nil
	}
	if mode == "strict" && route == "" {
		return fmt.Errorf("unsupported fake-gh api invocation: %s", strings.Join(args, " "))
	}
	_, _ = fmt.Fprintln(os.Stdout, `{"id":1,"number":1,"title":"fake issue"}`)
	_ = stdin
	return nil
}

func emitResponse(resp response) error {
	if resp.Stderr != "" {
		_, _ = os.Stderr.WriteString(resp.Stderr)
	}
	if len(resp.Stdout) > 0 {
		_, _ = os.Stdout.Write(resp.Stdout)
		if resp.Stdout[len(resp.Stdout)-1] != '\n' {
			_, _ = fmt.Fprintln(os.Stdout)
		}
	}
	if resp.ExitCode != 0 {
		os.Exit(resp.ExitCode)
	}
	return nil
}

func emitDefaultJSON(key string, fields []string) error {
	object := map[string]any{}
	for _, field := range fields {
		object[field] = defaultValue(field)
	}
	var payload any = object
	if strings.HasSuffix(key, "list") {
		payload = []map[string]any{object}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, string(encoded))
	return nil
}

func validateFields(command string, fields []string, allowed []string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allow[field] = struct{}{}
	}
	for _, field := range fields {
		if _, ok := allow[field]; ok {
			continue
		}
		available := append([]string{}, allowed...)
		sort.Strings(available)
		return fmt.Errorf("unknown JSON field: %q\nAvailable fields:\n  %s\n", field, strings.Join(available, "\n  "))
	}
	return nil
}

func requestedJSONFields(args []string) []string {
	for index, arg := range args {
		if arg == "--json" && index+1 < len(args) {
			return splitFields(args[index+1])
		}
		if strings.HasPrefix(arg, "--json=") {
			return splitFields(strings.TrimPrefix(arg, "--json="))
		}
	}
	return nil
}

func splitFields(raw string) []string {
	parts := strings.Split(raw, ",")
	fields := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			fields = append(fields, part)
		}
	}
	return fields
}

func commandKey(args []string) string {
	parts := make([]string, 0, 2)
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if takesValue(arg) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		parts = append(parts, arg)
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func firstNonFlag(args []string) string {
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if takesValue(arg) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func graphqlOperation(args []string, stdin string) string {
	reader := strings.NewReader(strings.Join(args, " ") + " " + stdin)
	scanner := bufio.NewScanner(reader)
	scanner.Split(bufio.ScanWords)
	for scanner.Scan() {
		token := scanner.Text()
		if strings.Contains(token, "resolveReviewThread") {
			return "resolveReviewThread"
		}
		if strings.Contains(token, "unresolveReviewThread") {
			return "unresolveReviewThread"
		}
	}
	return "default"
}

func takesValue(flag string) bool {
	if strings.Contains(flag, "=") {
		return false
	}
	switch flag {
	case "-X", "--method", "-f", "-F", "--field", "--raw-field", "-H", "--header", "--hostname", "--repo", "--json", "--jq", "--template", "--input":
		return true
	default:
		return false
	}
}

func defaultValue(field string) any {
	switch field {
	case "number":
		return 1
	case "title":
		return "fake title"
	case "state":
		return "OPEN"
	case "url":
		return "https://example.test/owner/repo/pull/1"
	case "id":
		return "FAKE_node_id"
	case "body":
		return ""
	case "headRefName":
		return "fake-branch"
	case "headRefOid":
		return "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	case "authorAssociation":
		return "NONE"
	default:
		return field
	}
}

func loadSchema(path string) (schema, error) {
	if path == "" {
		return schema{}, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return schema{}, err
	}
	var decoded schema
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return schema{}, err
	}
	if decoded.JSONFieldAllowlist == nil {
		decoded.JSONFieldAllowlist = map[string][]string{}
	}
	return decoded, nil
}

func loadState(path string) (state, error) {
	if path == "" {
		return state{}, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state{}, nil
		}
		return state{}, err
	}
	var decoded state
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return state{}, err
	}
	if decoded.Commands == nil {
		decoded.Commands = map[string]response{}
	}
	if decoded.Routes == nil {
		decoded.Routes = map[string]json.RawMessage{}
	}
	if decoded.GraphQL == nil {
		decoded.GraphQL = map[string]json.RawMessage{}
	}
	return decoded, nil
}

func collectEnv() map[string]string {
	keys := []string{envFakeGHMode, envFakeGHArtifactDir, envFakeGHSchemaPath, envFakeGHStatePath, envFakeGHRecordPath, "HOME"}
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			result[key] = value
		}
	}
	return result
}

func appendJSONL(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = file.Write(append(encoded, '\n'))
	return err
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func fatalf(code int, format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(code)
}
