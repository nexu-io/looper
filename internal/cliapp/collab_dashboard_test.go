package cliapp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollabDashboardIsReadOnlyAndDoesNotExportTaskBodies(t *testing.T) {
	config := writeLoopDiagnosticsFixture(t, "")
	db, err := openCollabReadOnly(filepath.Dir(config))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := collabDashboardHandler(db)
	for _, test := range []struct {
		method, path, host string
		code               int
	}{
		{"GET", "/", "127.0.0.1:18090", 200}, {"GET", "/app.js", "localhost:18090", 200},
		{"GET", "/api/overview", "127.0.0.1:18090", 200}, {"GET", "/api/tasks/8", "127.0.0.1:18090", 200},
		{"GET", "/api/memory", "127.0.0.1:18090", 200}, {"POST", "/api/memory", "localhost", 405},
		{"POST", "/api/overview", "127.0.0.1:18090", 405}, {"GET", "/api/overview", "attacker.example", 403},
		{"GET", "/config.json", "localhost", 404}, {"GET", "/design.md", "localhost", 404},
	} {
		req := httptest.NewRequest(test.method, "http://"+test.host+test.path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != test.code {
			t.Fatalf("%s %s: %d %s", test.method, test.path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "worktree is locked") || strings.Contains(w.Body.String(), "previous review") {
			t.Fatal("raw body exported")
		}
		if test.code == 200 && w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("missing CSP")
		}
	}
	if _, err := db.Exec(`UPDATE loops SET status='completed'`); err == nil {
		t.Fatal("dashboard connection allows writes")
	}
	result, err := collabOverview(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	tasks := result["tasks"].([]map[string]any)
	if len(tasks) != 1 || tasks[0]["runs"] != int64(1) || tasks[0]["agentMs"] != nil {
		t.Fatalf("wrong task aggregation: %v", tasks)
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), `"agentMs":null`) {
		t.Fatal("unknown usage became zero")
	}
}
