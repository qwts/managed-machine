//go:build ignore

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/qwts/managed-machine/internal/legacy"
)

func main() {
	out := filepath.Join("..", "..", "lib", "setup-cutover.sh")
	content := legacy.RenderSetupCutoverSh()
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "cutover_gen: %v\n", err)
		os.Exit(1)
	}
}
