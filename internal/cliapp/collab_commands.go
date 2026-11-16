package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/collab"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/executionhost"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/worker"
	"github.com/spf13/cobra"
)

func (r *commandRuntime) collabCommand() *cobra.Command {
	command := newCommand(commandSpec{use: "collab", short: "Run the independent Feishu collaboration service", helpWhenNoArgs: true,
		persistentFlags: []flagSpec{stringFlag("state-dir", "path", "Independent collaboration state directory")},
		helpSubcommands: []helpSubcommand{{name: "start", description: "Start once; stay available across tasks"}, {name: "status", description: "Show service and task progress"}, {name: "run", description: "Explicitly dispatch a task to a configured repository"}, {name: "logs", description: "Show collaboration host logs"}, {name: "stop", description: "Drain executions and stop the service"}},
		subcommands: []*cobra.Command{
			newCommand(commandSpec{use: "start", short: "Start once; stay available across tasks", args: cobra.NoArgs, runE: r.collabStart, localFlags: []flagSpec{boolFlag("foreground", "Run in the current terminal")}}),
			newCommand(commandSpec{use: "status", short: "Show service and task progress", args: cobra.NoArgs, runE: r.collabStatus}),
			newCommand(commandSpec{use: "run", short: "Explicitly dispatch a task", args: cobra.NoArgs, runE: r.collabRun, localFlags: []flagSpec{stringFlag("project", "id", "Configured local project ID"), stringFlag("repo", "owner/repo", "Explicit target code repository"), stringFlag("module", "name", "Optional module collaboration scope"), stringFlag("title", "text", "Human-readable task title"), stringFlag("prompt-file", "path|-", "Task prompt file; - reads stdin"), stringFlag("issue", "number", "Source issue or Plane sequence number"), stringFlag("issue-url", "url", "Source task link")}}),
			newCommand(commandSpec{use: "logs", short: "Show collaboration host logs", args: cobra.NoArgs, runE: r.collabLogs}),
			newCommand(commandSpec{use: "stop", short: "Drain executions and stop the service", args: cobra.NoArgs, runE: r.collabStop}),
		},
	})
	return command
}

func (r *commandRuntime) collabConfig(cmd *cobra.Command) (config.Config, string, error) {
	loaded, err := r.loadConfig()
	if err != nil {
		return config.Config{}, "", err
	}
	dir, err := filepath.Abs(collab.StateDir(loaded.Config, getStringFlag(cmd, "state-dir")))
	return loaded.Config, dir, err
}

func (r *commandRuntime) collabStart(cmd *cobra.Command, _ []string) error {
	cfg, dir, err := r.collabConfig(cmd)
	if err != nil {
		return err
	}
	if getBoolFlag(cmd, "foreground") {
		home, err := r.homeDir()
		if err != nil {
			return err
		}
		return collab.Serve(cmd.Context(), collab.Options{Config: cfg, StateDir: dir, NetworkStatePath: networkclient.DefaultStatePath(home), Logger: collabLogger{out: r.app.stderr()}})
	}
	if result, err := collab.Request(cmd.Context(), dir, "GET", "/status", nil); err == nil {
		return writeCollabOutput(cmd, result)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(collab.LogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	executable := r.app.deps.ExecutablePath
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return err
		}
	}
	args := append([]string{}, ExtractConfigArgs(r.argv)...)
	args = append(args, "collab", "start", "--foreground", "--state-dir", dir)
	child := exec.Command(executable, args...)
	child.Stdout, child.Stderr = log, log
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case err := <-done:
			return fmt.Errorf("collab exited before readiness (%v); see %s", err, collab.LogPath(dir))
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-deadline.C:
			return fmt.Errorf("collab is still starting; inspect %s", collab.LogPath(dir))
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(cmd.Context(), 250*time.Millisecond)
			result, err := collab.Request(ctx, dir, "GET", "/status", nil)
			cancel()
			if err == nil {
				return writeCollabOutput(cmd, result)
			}
		}
	}
}

func (r *commandRuntime) collabStatus(cmd *cobra.Command, _ []string) error {
	return r.collabRequest(cmd, "GET", "/status", nil)
}
func (r *commandRuntime) collabStop(cmd *cobra.Command, _ []string) error {
	_, dir, err := r.collabConfig(cmd)
	if err != nil {
		return err
	}
	if _, err := collab.Request(cmd.Context(), dir, "POST", "/stop", nil); err != nil {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		held, err := executionhost.ServiceLockHeld(filepath.Join(dir, "service.lock"))
		if err != nil {
			return err
		}
		if !held {
			return writeJSON(cmd.OutOrStdout(), map[string]string{"status": "stopped"})
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-timeout.C:
			return fmt.Errorf("collab is still draining; see %s", collab.LogPath(dir))
		case <-ticker.C:
		}
	}
}
func (r *commandRuntime) collabRequest(cmd *cobra.Command, method, path string, input any) error {
	_, dir, err := r.collabConfig(cmd)
	if err != nil {
		return err
	}
	result, err := collab.Request(cmd.Context(), dir, method, path, input)
	if err != nil {
		return err
	}
	return writeCollabOutput(cmd, result)
}

func (r *commandRuntime) collabRun(cmd *cobra.Command, _ []string) error {
	input := worker.EnqueueInput{ProjectID: getStringFlag(cmd, "project"), Repo: getStringFlag(cmd, "repo"), Module: getStringFlag(cmd, "module"), Title: getStringFlag(cmd, "title"), IssueURL: getStringFlag(cmd, "issue-url")}
	if text := getStringFlag(cmd, "issue"); text != "" {
		number, err := strconv.ParseInt(text, 10, 64)
		if err != nil || number <= 0 {
			return fmt.Errorf("--issue must be a positive integer")
		}
		input.IssueNumber = number
	}
	if path := getStringFlag(cmd, "prompt-file"); path != "" {
		var data []byte
		var err error
		if path == "-" {
			data, err = io.ReadAll(io.LimitReader(collabStdin(r.app), 2<<20))
		} else {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			return err
		}
		input.Prompt = string(data)
	}
	return r.collabRequest(cmd, "POST", "/run", input)
}

func (r *commandRuntime) collabLogs(cmd *cobra.Command, _ []string) error {
	_, dir, err := r.collabConfig(cmd)
	if err != nil {
		return err
	}
	file, err := os.Open(collab.LogPath(dir))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 64<<10 {
		if _, err := file.Seek(-(64 << 10), io.SeekEnd); err != nil {
			return err
		}
	}
	_, err = io.Copy(cmd.OutOrStdout(), file)
	return err
}

type collabLogger struct{ out io.Writer }

func (l collabLogger) Debug(message string, fields map[string]any) { l.Info(message, fields) }
func (l collabLogger) Info(message string, fields map[string]any) {
	fmt.Fprintf(l.out, "%s %s %v\n", time.Now().Format(time.RFC3339), message, fields)
}
func (l collabLogger) Warn(message string, fields map[string]any)  { l.Info(message, fields) }
func (l collabLogger) Error(message string, fields map[string]any) { l.Info(message, fields) }

func collabStdin(app *App) io.Reader {
	if app.deps.Stdin != nil {
		return app.deps.Stdin
	}
	return os.Stdin
}

func writeCollabOutput(cmd *cobra.Command, result json.RawMessage) error {
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), result)
	}
	var status collab.Status
	if err := json.Unmarshal(result, &status); err != nil {
		return err
	}
	if status.Service != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "%s · %s · PID %d · %d 个执行中\n", status.Service, status.State, status.PID, status.ActiveExecutions)
		networkState := "尚未绑定设备"
		if status.Network.Configured {
			networkState = "设备已绑定，连接不可达"
			if status.Network.CloudReachable {
				networkState = "设备在线"
			}
			if status.Network.IdentityDrift {
				networkState = "设备身份与当前代码托管账号不一致"
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "loopernet · %s · %s\n", networkState, status.Network.NodeName)
		for _, task := range status.Tasks {
			fmt.Fprintf(cmd.OutOrStdout(), "#%d  %s  %s  %s\n", task.Seq, task.Status, task.Repo, task.Title)
		}
		return nil
	}
	var task collab.Task
	if err := json.Unmarshal(result, &task); err != nil {
		return err
	}
	if task.ID != "" {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "已接单 #%d · %s · %s\n", task.Seq, task.Repo, task.Title)
		return err
	}
	return writeJSON(cmd.OutOrStdout(), result)
}
