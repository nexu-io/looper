package cliapp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestGrokBuildBootstrapSelection(t *testing.T) {
	r := newCommandRuntime(New(Deps{}), nil)
	plan, _, err := r.planBootstrapConfig(newTakeoverCmd(t, "", false), t.TempDir(), bootstrapOptions{AgentVendor: "grok-build", Yes: true})
	if err != nil || plan.AgentVendor == nil || *plan.AgentVendor != config.AgentVendorGrokBuild {
		t.Fatalf("planBootstrapConfig() = (%+v, %v), want grok-build", plan, err)
	}

	var output bytes.Buffer
	vendor, err := promptBootstrapVendor(bufio.NewReader(strings.NewReader("grok-build\n")), &output)
	if err != nil || vendor == nil || *vendor != config.AgentVendorGrokBuild {
		t.Fatalf("promptBootstrapVendor() = (%v, %v), want grok-build", vendor, err)
	}
}

func TestGrokBuildTakeoverSelectionAndDetection(t *testing.T) {
	configPath := t.TempDir() + "/config.toml"
	r := newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor()})
	vendor, _, err := r.resolveTakeoverVendor(newTakeoverCmd(t, "grok-build", false), takeoverOptions{AgentVendor: "grok-build"})
	if err != nil || vendor != config.AgentVendorGrokBuild {
		t.Fatalf("explicit Grok selection = (%q, %v), want grok-build", vendor, err)
	}

	got := detectInstalledVendors(lookPathFor("grok"))
	if len(got) != 1 || got[0] != config.AgentVendorGrokBuild {
		t.Fatalf("Grok-only detection = %v, want [grok-build]", got)
	}
	r = newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor("grok", "codex")})
	_, _, err = r.resolveTakeoverVendor(newTakeoverCmd(t, "", true), takeoverOptions{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "grok-build") || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("Grok ambiguity error = %v, want both vendors", err)
	}
}

func TestGrokBuildVendorHelpAndErrors(t *testing.T) {
	app := New(Deps{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	root := app.newRootCommand(nil)
	takeover, _, err := root.Find([]string{"takeover"})
	if err != nil {
		t.Fatalf("find takeover command: %v", err)
	}
	flag := takeover.Flags().Lookup("agent-vendor")
	if flag == nil || !strings.Contains(flag.Usage, "grok-build") {
		t.Fatalf("takeover agent-vendor help = %v, want grok-build", flag)
	}

	r := newTakeoverRuntime(t, t.TempDir()+"/config.toml", Deps{LookPath: lookPathFor()})
	_, _, err = r.resolveTakeoverVendor(newTakeoverCmd(t, "", true), takeoverOptions{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "grok") {
		t.Fatalf("no-agent error = %v, want grok install guidance", err)
	}
}
