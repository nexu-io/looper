package config

import "os/exec"

type LookPathFunc func(string) (string, error)

type ToolDetectionResult struct {
	Paths     ToolPathsConfig
	Detection map[string]ToolDetectionStatus
}

func DetectToolPaths(configured ToolPathsConfig, lookPath LookPathFunc) ToolDetectionResult {
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	paths := ToolPathsConfig{
		GitPath:       cloneStringPtr(configured.GitPath),
		GHPath:        cloneStringPtr(configured.GHPath),
		LooperPath:    cloneStringPtr(configured.LooperPath),
		OsascriptPath: cloneStringPtr(configured.OsascriptPath),
	}

	detection := map[string]ToolDetectionStatus{
		"gitPath":       toolDetectionStatusFor(paths.GitPath),
		"ghPath":        toolDetectionStatusFor(paths.GHPath),
		"osascriptPath": toolDetectionStatusFor(paths.OsascriptPath),
	}
	if paths.LooperPath != nil {
		detection["looperPath"] = ToolDetectionStatusConfigured
	}

	candidates := []struct {
		key        string
		executable string
		target     **string
	}{
		{key: "gitPath", executable: "git", target: &paths.GitPath},
		{key: "ghPath", executable: "gh", target: &paths.GHPath},
		{key: "looperPath", executable: "looper", target: &paths.LooperPath},
		{key: "osascriptPath", executable: "osascript", target: &paths.OsascriptPath},
	}

	for _, candidate := range candidates {
		if *candidate.target != nil {
			continue
		}

		resolvedPath, err := lookPath(candidate.executable)
		if err != nil || resolvedPath == "" {
			continue
		}

		*candidate.target = stringPtr(resolvedPath)
		detection[candidate.key] = ToolDetectionStatusDetected
	}

	return ToolDetectionResult{Paths: paths, Detection: detection}
}

func toolDetectionStatusFor(path *string) ToolDetectionStatus {
	if path == nil || *path == "" {
		return ToolDetectionStatusMissing
	}

	return ToolDetectionStatusConfigured
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}

	return stringPtr(*value)
}
