package cliapp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestPiOmpBootstrapSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vendor config.AgentVendor
	}{
		{"pi", config.AgentVendorPi},
		{"omp", config.AgentVendorOmp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCommandRuntime(New(Deps{}), nil)
			plan, _, err := r.planBootstrapConfig(newTakeoverCmd(t, "", false), t.TempDir(), bootstrapOptions{AgentVendor: string(tc.vendor), Yes: true})
			if err != nil || plan.AgentVendor == nil || *plan.AgentVendor != tc.vendor {
				t.Fatalf("planBootstrapConfig() = (%+v, %v), want %s", plan, err, tc.vendor)
			}

			var output bytes.Buffer
			vendor, err := promptBootstrapVendor(bufio.NewReader(strings.NewReader(string(tc.vendor)+"\n")), &output)
			if err != nil || vendor == nil || *vendor != tc.vendor {
				t.Fatalf("promptBootstrapVendor() = (%v, %v), want %s", vendor, err, tc.vendor)
			}
		})
	}
}

func TestPiOmpTakeoverSelectionAndDetection(t *testing.T) {
	configPath := t.TempDir() + "/config.toml"
	r := newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor()})

	for _, tc := range []struct {
		vendor config.AgentVendor
		binary string
	}{
		{config.AgentVendorPi, "pi"},
		{config.AgentVendorOmp, "omp"},
	} {
		t.Run(string(tc.vendor)+"_explicit", func(t *testing.T) {
			vendor, _, err := r.resolveTakeoverVendor(newTakeoverCmd(t, string(tc.vendor), false), takeoverOptions{AgentVendor: string(tc.vendor)})
			if err != nil || vendor != tc.vendor {
				t.Fatalf("explicit selection = (%q, %v), want %s", vendor, err, tc.vendor)
			}
		})
		t.Run(string(tc.vendor)+"_detect", func(t *testing.T) {
			got := detectInstalledVendors(lookPathFor(tc.binary))
			if len(got) != 1 || got[0] != tc.vendor {
				t.Fatalf("%s-only detection = %v, want [%s]", tc.binary, got, tc.vendor)
			}
		})
	}

	r = newTakeoverRuntime(t, configPath, Deps{LookPath: lookPathFor("pi", "omp")})
	_, _, err := r.resolveTakeoverVendor(newTakeoverCmd(t, "", true), takeoverOptions{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "pi") || !strings.Contains(err.Error(), "omp") {
		t.Fatalf("pi/omp ambiguity error = %v, want both vendors", err)
	}
}

func TestPiOmpVendorHelpAndErrors(t *testing.T) {
	app := New(Deps{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	root := app.newRootCommand(nil)
	takeover, _, err := root.Find([]string{"takeover"})
	if err != nil {
		t.Fatalf("find takeover command: %v", err)
	}
	flag := takeover.Flags().Lookup("agent-vendor")
	if flag == nil || !strings.Contains(flag.Usage, "pi") || !strings.Contains(flag.Usage, "omp") {
		t.Fatalf("takeover agent-vendor help = %v, want pi and omp", flag)
	}

	r := newTakeoverRuntime(t, t.TempDir()+"/config.toml", Deps{LookPath: lookPathFor()})
	_, _, err = r.resolveTakeoverVendor(newTakeoverCmd(t, "", true), takeoverOptions{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "pi") || !strings.Contains(err.Error(), "omp") {
		t.Fatalf("no-agent error = %v, want pi/omp install guidance", err)
	}
}
