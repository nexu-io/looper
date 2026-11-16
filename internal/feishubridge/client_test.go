package feishubridge

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestClientReplacesLiveSidecarThroughIPCAndDrainsChild(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	dir, err := os.MkdirTemp("", "bridge-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	entry := filepath.Join(dir, "fixture.mjs")
	// Real Node process and Unix socket, with the same inherited-pipe lifecycle.
	source := `import {createServer} from 'node:http';
import {unlinkSync} from 'node:fs';
import {createInterface} from 'node:readline';
const input=createInterface({input:process.stdin});
input.once('line',line=>{
 const init=JSON.parse(line);
 const stop=()=>{server.close(()=>{try{unlinkSync(init.socket)}catch{};process.exit(0)});};
 const server=createServer((req,res)=>{res.end('{}');if(req.url==='/shutdown')setImmediate(stop)});
 server.listen(init.socket);
 process.stdin.once('end',stop);
 process.once('SIGTERM',stop);
});`
	if err := os.WriteFile(entry, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	init := Init{Socket: filepath.Join(dir, "bridge.sock"), AppID: "fixture", AppSecret: "fixture"}
	first, err := Start(ctx, entry, dir, init, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Start(ctx, entry, dir, init, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	select {
	case <-first.Done():
	case <-ctx.Done():
		t.Fatal("previous sidecar survived replacement")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second.Done():
	default:
		t.Fatal("close returned before child exited")
	}
}
