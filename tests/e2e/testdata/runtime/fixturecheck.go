package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		path := filepath.Join("bench", name)
		raw, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(raw)) == "" {
			fmt.Fprintf(os.Stderr, "%s is empty or unreadable: %v\n", path, err)
			os.Exit(1)
		}
	}
}
