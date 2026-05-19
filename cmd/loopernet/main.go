package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexu-io/looper/internal/network/cloud"
	"github.com/nexu-io/looper/internal/version"
)

func main() {
	os.Exit(run(os.Environ()))
}

func run(envList []string) int {
	env := map[string]string{}
	for _, entry := range envList {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				env[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	cfg, err := cloud.LoadConfigFromEnv(env, version.Value)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "loopernet:", err)
		return 1
	}
	ctx := context.Background()
	service, err := cloud.Open(ctx, cfg)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "loopernet:", err)
		return 1
	}
	defer service.Close()
	server := cloud.NewServer(cfg, service)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		_ = server.Shutdown(context.Background())
	}()
	if err := server.Start(); err != nil && err.Error() != "http: Server closed" {
		_, _ = fmt.Fprintln(os.Stderr, "loopernet:", err)
		return 1
	}
	return 0
}
