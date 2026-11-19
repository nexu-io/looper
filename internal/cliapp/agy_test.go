package cliapp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestAgyBootstrapSelection(t *testing.T) {
	r := newCommandRuntime(New(Deps{}), nil)
	plan, _, err := r.planBootstrapConfig(newTakeoverCmd(t, "", false), t.TempDir(), bootstrapOptions{AgentVendor: "agy", Yes: true})
	if err != nil || plan.AgentVendor == nil || *plan.AgentVendor != config.AgentVendorAgy {
		t.Fatalf("planBootstrapConfig() = (%+v, %v), want agy", plan, err)
	}

	var output bytes.Buffer
	vendor, err := promptBootstrapVendor(bufio.NewReader(strings.NewReader("agy\n")), &output)
	if err != nil || vendor == nil || *vendor != config.AgentVendorAgy {
		t.Fatalf("promptBootstrapVendor() = (%v, %v), want agy", vendor, err)
	}
	if !strings.Contains(output.String(), "agy") {
		t.Fatalf("prompt output = %q, want agy in vendor list", output.String())
	}
}

func TestAgyTakeoverSelectionAndDetection(t *testing.T) {
	configPath := t.TempDir() + "/config.toml"
	r := newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor()})

	vendor, _, err := r.resolveTakeoverVendor(newTakeoverCmd(t, "agy", false), takeoverOptions{AgentVendor: "agy"})
	if err != nil || vendor != config.AgentVendorAgy {
		t.Fatalf("explicit selection = (%q, %v), want agy", vendor, err)
	}

	got := detectInstalledVendors(lookPathFor("agy"))
	if len(got) != 1 || got[0] != config.AgentVendorAgy {
		t.Fatalf("agy-only detection = %v, want [agy]", got)
	}

	r = newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor("agy", "codex")})
	_, _, err = r.resolveTakeoverVendor(newTakeoverCmd(t, "", true), takeoverOptions{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "agy") || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("agy/codex ambiguity error = %v, want both vendors", err)
	}
}

func TestAgyVendorHelpAndErrors(t *testing.T) {
	app := New(Deps{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	root := app.newRootCommand(nil)
	takeover, _, err := root.Find([]string{"takeover"})
	if err != nil {
		t.Fatalf("find takeover command: %v", err)
	}
	flag := takeover.Flags().Lookup("agent-vendor")
	if flag == nil || !strings.Contains(flag.Usage, "agy") {
		t.Fatalf("takeover agent-vendor help = %v, want agy", flag)
	}

	r := newTakeoverRuntime(t, t.TempDir()+"/config.toml", Deps{LookPath: lookPathFor()})
	_, _, err = r.resolveTakeoverVendor(newTakeoverCmd(t, "", true), takeoverOptions{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "agy") {
		t.Fatalf("no-agent error = %v, want agy install guidance", err)
	}
}
