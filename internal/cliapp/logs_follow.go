package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type loopLogsFollowChunk struct {
	RunID       *string `json:"runId,omitempty"`
	CurrentStep *string `json:"currentStep,omitempty"`
	ExecutionID *string `json:"executionId,omitempty"`
	Vendor      *string `json:"vendor,omitempty"`
	PID         *int64  `json:"pid,omitempty"`
	Status      *string `json:"status,omitempty"`
	Content     string  `json:"content"`
}

func (r *commandRuntime) followLoopLogs(cmd *cobra.Command, loopID string) error {
	if _, err := parseOptionalPositiveInt(getStringFlag(cmd, "tail"), "--tail"); err != nil {
		return err
	}

	client, err := r.apiClient()
	if err != nil {
		return err
	}

	query := url.Values{}
	query.Set("follow", "1")
	if getBoolFlag(cmd, "stderr") {
		query.Set("stderr", "1")
	}

	response, err := client.Stream(cmd.Context(), "/api/v1/loops/"+url.PathEscape(loopID)+"/logs?"+query.Encode(), "text/event-stream")
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	lastExecutionID := ""
	lastRunID := ""
	sawLogContent := false
	sawEnd := false

	for {
		eventName, payload, err := readServerSentEvent(reader)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if sawEnd && errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("log stream terminated unexpectedly")
			}
			return err
		}

		switch eventName {
		case "snapshot":
			data, decodeErr := decodeLoopLogsOutput(json.RawMessage(payload))
			if decodeErr != nil {
				return decodeErr
			}
			if loopLogsSnapshotHasOutput(data, getBoolFlag(cmd, "stderr"), getBoolFlag(cmd, "full"), getStringFlag(cmd, "tail")) {
				sawLogContent = true
			}
			if err := writeHumanLoopLogsSnapshot(cmd.OutOrStdout(), data, getBoolFlag(cmd, "stderr"), getBoolFlag(cmd, "full"), getStringFlag(cmd, "tail"), true); err != nil {
				return err
			}
			if data.Agent != nil {
				lastExecutionID = data.Agent.ExecutionID
			}
			if data.Run != nil {
				lastRunID = data.Run.RunID
			}
		case "chunk":
			var chunk loopLogsFollowChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				return fmt.Errorf("decode loop logs stream chunk: %w", err)
			}
			if chunk.ExecutionID != nil && *chunk.ExecutionID != "" && *chunk.ExecutionID != lastExecutionID {
				if lastExecutionID != "" || lastRunID != "" {
					if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
						return err
					}
				}
				if chunk.RunID != nil && *chunk.RunID != lastRunID {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Run %s · step: %s\n", *chunk.RunID, formatScalar(chunk.CurrentStep)); err != nil {
						return err
					}
					lastRunID = *chunk.RunID
				}
				if chunk.RunID != nil && lastRunID == "" {
					lastRunID = *chunk.RunID
				}
				if chunk.Vendor != nil && chunk.Status != nil {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Agent: %s · pid %s · %s\n\n", *chunk.Vendor, formatScalar(chunk.PID), *chunk.Status); err != nil {
						return err
					}
				}
				lastExecutionID = *chunk.ExecutionID
			}
			if _, err := io.WriteString(cmd.OutOrStdout(), chunk.Content); err != nil {
				return err
			}
			if strings.TrimSpace(chunk.Content) != "" {
				sawLogContent = true
			}
		case "end":
			sawEnd = true
			if !sawLogContent {
				payload, err := r.getJSON(cmd.Context(), "/api/v1/loops/"+url.PathEscape(loopID)+"/logs")
				if err != nil {
					return err
				}
				data, err := decodeLoopLogsOutput(payload)
				if err != nil {
					return err
				}
				if loopLogsRunFailureMessage(data) != "" {
					if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
						return err
					}
					return writeHumanLoopLogsSnapshot(cmd.OutOrStdout(), data, getBoolFlag(cmd, "stderr"), getBoolFlag(cmd, "full"), getStringFlag(cmd, "tail"), false)
				}
			}
			return nil
		}
	}
}

func loopLogsSnapshotHasOutput(data loopLogsOutput, stderr bool, full bool, tail string) bool {
	content, err := loopLogsInitialContent(data, stderr, full, tail)
	if err != nil {
		return false
	}
	return strings.TrimSpace(content) != "" || loopLogsRunFailureMessage(data) != ""
}

func readServerSentEvent(reader *bufio.Reader) (string, string, error) {
	eventName := "message"
	dataLines := make([]string, 0, 1)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			line = strings.TrimRight(line, "\r\n")
			if errors.Is(err, io.EOF) {
				if line == "" {
					if len(dataLines) > 0 {
						return eventName, strings.Join(dataLines, "\n"), nil
					}
					return "", "", io.EOF
				}
			} else {
				return "", "", err
			}
		} else {
			line = strings.TrimRight(line, "\r\n")
		}
		if line == "" {
			if len(dataLines) == 0 {
				if errors.Is(err, io.EOF) {
					return "", "", io.EOF
				}
				continue
			}
			return eventName, strings.Join(dataLines, "\n"), nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			dataLines = append(dataLines, data)
		}
		if errors.Is(err, io.EOF) {
			return eventName, strings.Join(dataLines, "\n"), nil
		}
	}
}
