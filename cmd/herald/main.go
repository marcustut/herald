// Command herald is the agent event gateway: a server plus a client CLI.
package main

import (
	"context"
	"os"

	"github.com/marcustut/herald/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
