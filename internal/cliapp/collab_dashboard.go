package cliapp

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/storage"
	"github.com/spf13/cobra"
)

//go:embed collab_dashboard/*
var collabDashboardAssets embed.FS

func (r *commandRuntime) collabDashboard(cmd *cobra.Command, _ []string) error {
	_, dir, err := r.collabConfig(cmd)
	if err != nil {
		return err
	}
	port := getStringFlag(cmd, "port")
	if port == "" {
		port = "18090"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	db, err := openCollabReadOnly(dir)
	if err != nil {
		return err
	}
	defer db.Close()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: collabDashboardHandler(db), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-cmd.Context().Done():
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = server.Shutdown(ctx)
		case <-done:
		}
	}()
	fmt.Fprintf(cmd.OutOrStdout(), "Looper read-only dashboard: http://127.0.0.1:%s\n", port)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func collabDashboardHandler(db *sql.DB) http.Handler {
	assets, _ := fs.Sub(collabDashboardAssets, "collab_dashboard")
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "loopback host required", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		var result any
		var err error
		switch {
		case r.URL.Path == "/api/memory":
			result, err = collabMemoryOverview(r.Context(), db)
		case r.URL.Path == "/api/overview":
			result, err = collabOverview(r.Context(), db)
		case strings.HasPrefix(r.URL.Path, "/api/tasks/"):
			selector := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
			if selector == "" || strings.Contains(selector, "/") {
				http.NotFound(w, r)
				return
			}
			result, err = collabTraceReport(r.Context(), storage.NewRepositories(db), selector)
		case r.URL.Path == "/" || r.URL.Path == "/app.js" || r.URL.Path == "/style.css":
			files.ServeHTTP(w, r)
			return
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "local task data unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(result)
	})
}

func collabOverview(ctx context.Context, db *sql.DB) (map[string]any, error) {
	// Aggregate before joining: multiple turns and events must not multiply task counts.
	rows, err := db.QueryContext(ctx, `WITH recent AS (SELECT * FROM loops WHERE type='worker' ORDER BY created_at DESC,seq DESC LIMIT 500),
 rr AS (SELECT loop_id,count(*) n FROM runs WHERE loop_id IN (SELECT id FROM recent) GROUP BY loop_id),
 aa AS (SELECT loop_id,count(*) n,group_concat(DISTINCT vendor) vendors,sum(CASE WHEN ended_at IS NOT NULL AND julianday(ended_at)>=julianday(started_at) THEN (julianday(ended_at)-julianday(started_at))*86400000 END) ms FROM agent_executions WHERE loop_id IN (SELECT id FROM recent) GROUP BY loop_id),
 ee AS (SELECT loop_id,count(*) n,max(event_type='task.received') tracked FROM event_logs WHERE loop_id IN (SELECT id FROM recent) GROUP BY loop_id)
 SELECT l.seq,COALESCE(json_extract(l.metadata_json,'$.worker.title'),'Task #'||l.seq),l.status,l.created_at,l.updated_at,COALESCE(rr.n,0),COALESCE(aa.n,0),COALESCE(ee.n,0),COALESCE(aa.vendors,''),aa.ms,COALESCE(ee.tracked,0) FROM recent l LEFT JOIN rr ON rr.loop_id=l.id LEFT JOIN aa ON aa.loop_id=l.id LEFT JOIN ee ON ee.loop_id=l.id ORDER BY l.created_at DESC,l.seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []map[string]any{}
	for rows.Next() {
		var seq, runs, agents, events, tracked int64
		var title, status, created, updated, vendors string
		var ms sql.NullFloat64
		if err := rows.Scan(&seq, &title, &status, &created, &updated, &runs, &agents, &events, &vendors, &ms, &tracked); err != nil {
			return nil, err
		}
		coverage := "partial_legacy"
		if tracked != 0 {
			coverage = "tracking_enabled"
		}
		var duration any
		if ms.Valid {
			duration = int64(ms.Float64 + 0.5)
		}
		names := []string{}
		if vendors != "" {
			names = strings.Split(vendors, ",")
		}
		tasks = append(tasks, map[string]any{"seq": seq, "title": title, "status": status, "createdAt": created, "updatedAt": updated, "runs": runs, "agents": agents, "events": events, "vendors": names, "agentMs": duration, "coverage": coverage})
	}
	return map[string]any{"schemaVersion": 1, "asOf": time.Now().UTC().Format(time.RFC3339), "tasks": tasks}, rows.Err()
}
