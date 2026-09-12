package cliapp

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/forge"
	"github.com/spf13/cobra"
)

func (r *commandRuntime) hostCommand() *cobra.Command {
	root := &cobra.Command{Use: "host", Short: "Read hosting context through the current execution capability", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error { return fmt.Errorf("a supported hosting command is required") }}
	identity := &cobra.Command{Use: "whoami", Short: "Show the bound hosting account", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return r.hostRead(cmd, forge.HostRequest{Op: "identity"})
	}}
	api := &cobra.Command{Use: "api <repository-relative-path>", Short: "Read a supported repository API endpoint", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		method, _ := cmd.Flags().GetString("method")
		paginate, _ := cmd.Flags().GetBool("paginate")
		diff, _ := cmd.Flags().GetBool("diff")
		return r.hostRead(cmd, forge.HostRequest{Op: "api.read", Path: args[0], Method: method, Paginate: paginate, Diff: diff})
	}}
	api.Flags().String("method", "GET", "Request method (GET only)")
	api.Flags().Bool("paginate", false, "Return the complete bounded collection")
	api.Flags().Bool("diff", false, "Read a pull request patch")
	threads := &cobra.Command{Use: "threads <pr-number>", Short: "Read all GitHub review threads", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		n, err := hostPRNumber(args[0])
		if err != nil {
			return err
		}
		return r.hostRead(cmd, forge.HostRequest{Op: "threads.list", PRNumber: n})
	}}
	thread := &cobra.Command{Use: "thread <pr-number> <thread-id>", Short: "Read one GitHub review thread", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		n, err := hostPRNumber(args[0])
		if err != nil {
			return err
		}
		return r.hostRead(cmd, forge.HostRequest{Op: "thread.read", PRNumber: n, ThreadID: args[1]})
	}}
	git := &cobra.Command{Use: "git", Short: "Read refs from the bound hosting repository", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error { return fmt.Errorf("only host git fetch is supported") }}
	git.AddCommand(&cobra.Command{Use: "fetch <ref>", Short: "Fetch one repository ref into the prepared checkout", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return r.hostRead(cmd, forge.HostRequest{Op: "git.fetch", Ref: args[0]})
	}})
	root.AddCommand(identity, api, threads, thread, git)
	return root
}

func hostPRNumber(raw string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("pull request number must be positive")
	}
	return n, nil
}

func (r *commandRuntime) hostRead(cmd *cobra.Command, request forge.HostRequest) error {
	// Global config/tool selectors are meaningless for a bound capability and
	// must not appear to alter its authority. No local config is ever loaded.
	for parent := cmd; parent != nil; parent = parent.Parent() {
		for _, name := range []string{"config", "db-path", "host", "port", "gh-path", "git-path", "looper-path", "looperd-path", "worktree-root", "working-directory"} {
			if flag := parent.Flags().Lookup(name); flag != nil && flag.Changed {
				return fmt.Errorf("hosting capability does not accept --%s", name)
			}
		}
	}
	output, err := forge.ProxyHost(cmd.Context(), request)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(cmd.OutOrStdout(), output); err != nil {
		return err
	}
	if output != "" && !strings.HasSuffix(output, "\n") {
		_, err = fmt.Fprintln(cmd.OutOrStdout())
	}
	return err
}
