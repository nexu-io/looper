package main

import (
	"fmt"
	"os"

	"github.com/nexu-io/looper/internal/version"
)

func main() {
	_, _ = fmt.Fprint(os.Stdout, version.LDFlags(version.BuildOverridesFromEnv(os.Getenv)))
}
