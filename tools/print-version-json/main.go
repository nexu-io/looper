package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/nexu-io/looper/internal/version"
)

func main() {
	encoded, err := json.Marshal(version.Current())
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "marshal version info: %v\n", err)
		os.Exit(1)
	}

	_, _ = fmt.Fprintln(os.Stdout, string(encoded))
}
