package main

import (
	"fmt"
	"os"

	"github.com/powerformer/looper/internal/version"
)

func main() {
	_, _ = fmt.Fprint(os.Stdout, version.LDFlags(version.BuildOverridesFromEnv(os.Getenv)))
}
