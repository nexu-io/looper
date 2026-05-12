package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	_, _ = fmt.Fprintln(os.Stdout, strings.Join(os.Args[1:], " "))
}
