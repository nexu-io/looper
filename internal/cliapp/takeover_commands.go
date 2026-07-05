package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// takeoverResumeResponse mirrors the daemon's POST /loops/{seq}/takeover reply.
type takeoverResumeResponse struct {
	LoopID        string `json:"loopId"`
	Vendor        string `json:"vendor"`
	SessionID     string `json:"sessionId"`
	WorktreePath  string `json:"worktreePath"`
	Supported     bool   `json:"supported"`
	ResumeCommand string `json:"resumeCommand"`
	Message       string `json:"message"`
}

// resumeLoopBySeq parks a loop for human takeover and drops the operator straight
// into its agent session — the same native session id, in the loop's worktree —
// so they continue the exact conversation by hand. With --print it only prints
// the resume command (for scripting, or running it yourself) instead of exec'ing.
// The daemon's in-flight run is stopped by the takeover; hand it back afterwards
// with `looper handback <seq>` so the daemon resumes and sees your turns.
func (r *commandRuntime) resumeLoopBySeq(cmd *cobra.Command, args []string) error {
	seq, err := loopSeqSelector(args[0])
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	client, err := r.apiClient()
	if err != nil {
		return err
	}
	var resp takeoverResumeResponse
	if err := client.Post(ctx, "/api/v1/loops/"+url.PathEscape(seq)+"/takeover", nil, &resp); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	handbackHint := "hand it back to the daemon when done:  looper handback " + seq
	if !resp.Supported {
		fmt.Fprintf(out, "Loop %s is parked for takeover, but no interactive resume is available.\n%s\n%s\n", seq, resp.Message, handbackHint)
		return nil
	}
	if printOnly, _ := cmd.Flags().GetBool("print"); printOnly {
		fmt.Fprintln(out, resp.ResumeCommand)
		fmt.Fprintf(out, "# %s\n", handbackHint)
		return nil
	}
	fmt.Fprintf(out, "▶ Taking over loop %s — dropping you into its %s session. Exit the agent when done.\n  (%s)\n\n", seq, resp.Vendor, handbackHint)
	shell := exec.CommandContext(ctx, "sh", "-c", resp.ResumeCommand)
	shell.Stdin = os.Stdin
	shell.Stdout = os.Stdout
	shell.Stderr = os.Stderr
	// Interactive: a non-zero exit (the human Ctrl-C'ing out of the agent) is normal.
	_ = shell.Run()
	fmt.Fprintf(out, "\n✔ Session detached. To let the daemon continue (seeing your turns):  looper handback %s\n", seq)
	return nil
}

// handbackLoopBySeq re-arms a taken-over loop so the daemon resumes it.
func (r *commandRuntime) handbackLoopBySeq(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		seq, err := loopSeqSelector(args[0])
		if err != nil {
			return nil, err
		}
		return r.postJSON(ctx, "/api/v1/loops/"+url.PathEscape(seq)+"/handback", nil)
	}, writeHumanLoopHandback)
}

func writeHumanLoopHandback(w io.Writer, _ json.RawMessage) error {
	fmt.Fprintln(w, "Loop handed back — the daemon will resume it and continue, seeing your turns.")
	return nil
}
