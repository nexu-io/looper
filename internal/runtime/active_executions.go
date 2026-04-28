package runtime

import "sync"

type activeExecution interface {
	Kill(string) error
}

type ActiveExecutionRegistry struct {
	mu         sync.Mutex
	executions map[string]activeExecution
}

func NewActiveExecutionRegistry() *ActiveExecutionRegistry {
	return &ActiveExecutionRegistry{executions: make(map[string]activeExecution)}
}

func (r *ActiveExecutionRegistry) Register(loopID, runID, executionID string, execution activeExecution) func() {
	if r == nil || execution == nil {
		return func() {}
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	r.executions[key] = execution
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.executions[key] == execution {
			delete(r.executions, key)
		}
		r.mu.Unlock()
	}
}

func (r *ActiveExecutionRegistry) Kill(loopID, runID, executionID, reason string) (bool, error) {
	if r == nil {
		return false, nil
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	execution := r.executions[key]
	r.mu.Unlock()
	if execution == nil {
		return false, nil
	}
	return true, execution.Kill(reason)
}

func activeExecutionKey(loopID, runID, executionID string) string {
	return loopID + "\x00" + runID + "\x00" + executionID
}
