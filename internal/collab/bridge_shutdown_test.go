package collab

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestBridgeStopReturnsResponseAfterSidecarExits(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	t.Setenv("FEISHU_APP_ID", "fixture")
	t.Setenv("FEISHU_APP_SECRET", "fixture")
	t.Setenv("LOOPER_FEISHU_INBOX_URL", "")
	t.Setenv("LOOPER_FEISHU_INBOX_TOKEN", "")
	for i := 0; i < 12; i++ {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			cfg, dir := hostFixture(t, "#!/bin/sh\nexit 0\n")
			cli := filepath.Join(dir, "lark.sh")
			if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' '{\"ok\":true,\"data\":{}}'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg.HITL = config.HITLConfig{Enabled: true, AnswerTransport: "feishu", Feishu: &config.HITLFeishuConfig{CLIPath: cli, ChatID: "oc_test", OwnerOpenID: "ou_owner"}}
			entry := filepath.Join(dir, "bridge.mjs")
			if err := os.WriteFile(entry, []byte(`import {createServer} from 'node:http';
import {createInterface} from 'node:readline';
createInterface({input:process.stdin}).once('line',line=>{
const init=JSON.parse(line);const server=createServer((req,res)=>res.end('{}'));
server.listen(init.socket);const stop=()=>server.close(()=>process.exit(0));
process.stdin.once('end',stop);process.once('SIGTERM',stop)});`), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, Options{Config: cfg, StateDir: dir, NetworkStatePath: filepath.Join(dir, "network.json"), BridgeEntry: entry})
			}()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				data, err := Request(ctx, dir, "GET", "/status", nil)
				var status Status
				if err == nil && json.Unmarshal(data, &status) == nil && status.State == "running" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("host did not become ready")
				case <-tick.C:
				}
			}
			data, err := Request(ctx, dir, "POST", "/stop", nil)
			if err != nil || !strings.Contains(string(data), `"status":"stopped"`) {
				t.Fatalf("lost stop acknowledgement: %s %v", data, err)
			}
		})
	}
}
