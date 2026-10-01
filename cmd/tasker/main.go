// Command tasker is a single-binary work tracker for humans and coding agents.
package main

import (
	"os"

	"github.com/mrb101/tasker/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
