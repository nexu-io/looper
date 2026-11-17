package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
)

// Background repository metadata uses worker policy; PR snapshots use reviewer
// policy. Existing role contexts retain their captured selection.
func bindProjectHostingOperation(ctx context.Context, cfg config.Config, projectID, repo, cwd, role string) (context.Context, error) {
	if _, _, bound := hostingidentity.Binding(ctx); bound {
		return ctx, nil
	}
	if projectID == "" {
		// A supplied checkout disambiguates repositories with the same slug on
		// different hosts before the repository-only fallback is considered.
		for _, project := range cfg.Projects {
			if cwd != "" && cwdBelongsToProject(project, cwd) {
				if projectID != "" {
					return nil, fmt.Errorf("hosting identity: checkout matches multiple projects")
				}
				projectID = project.ID
			}
		}
	}
	if projectID == "" {
		for _, project := range cfg.Projects {
			if repo != "" && strings.EqualFold(project.Repo, repo) {
				if projectID != "" {
					return nil, fmt.Errorf("hosting identity: repository matches multiple projects; project path is required")
				}
				projectID = project.ID
			}
		}
	}
	return hostingidentity.Bind(ctx, cfg, projectID, role)
}

// Probes share the scheduler's cancellation/drain lifetime. A bounded batch
// reports independent failures without delaying readiness or closing admission.
func (r *Runtime) probeHostingIdentities(parent context.Context, cfg config.Config) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	bindings := make(map[string][]string)
	var sessions []*hostingidentity.Session
	for _, project := range cfg.Projects {
		for _, role := range []string{"planner", "worker", "reviewer", "fixer", "coordinator"} {
			bound, err := hostingidentity.Bind(ctx, cfg, project.ID, role)
			if err != nil {
				if r.logger != nil {
					r.logger.Warn("hosting identity configuration failed", map[string]any{"projectId": project.ID, "role": role, "error": err.Error()})
				}
				continue
			}
			session, selected := hostingidentity.FromContext(bound)
			if !selected {
				continue
			}
			key := session.CacheKey()
			if len(bindings[key]) == 0 {
				sessions = append(sessions, session)
			}
			bindings[key] = append(bindings[key], project.ID+":"+role)
		}
	}
	semaphore := make(chan struct{}, 4)
	var pending sync.WaitGroup
	for _, session := range sessions {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			pending.Wait()
			return
		}
		pending.Add(1)
		go func(session *hostingidentity.Session) {
			defer pending.Done()
			defer func() { <-semaphore }()
			if _, err := session.Credentials(ctx); err != nil && r.logger != nil {
				r.logger.Warn("hosting identity authentication failed", map[string]any{"identity": session.Name(), "bindings": bindings[session.CacheKey()], "error": err.Error()})
			}
		}(session)
	}
	pending.Wait()
}
