package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsFeishuAnswerTransport(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.HITL.Enabled = true
	cfg.HITL.AnswerTransport = "feishu"
	err = Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "hitl.answerTransport") {
		t.Fatalf("Validate() error = %v, want Feishu notification-only validation error", err)
	}
}

func TestValidateAllowsSourceOfTruthAnswerTransports(t *testing.T) {
	for _, transport := range []string{"", "github", "respond"} {
		t.Run(transport, func(t *testing.T) {
			cfg, err := DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			cfg.HITL.Enabled = true
			cfg.HITL.AnswerTransport = transport
			if err := Validate(cfg); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}
