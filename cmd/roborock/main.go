// roborock reaches the Roborock devices of an account; the command is
// internal/cli.
package main

import (
	"context"
	"os"

	"github.com/mycophonic/primordium/app"
	"github.com/mycophonic/primordium/app/shutdown"

	"github.com/duncanite/go-roborock/internal/cli"
)

func main() {
	app.Main(&app.Options{Name: "roborock"}, func(ctx context.Context) error {
		env := cli.Environment{Args: os.Args[1:], In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv}

		if code := cli.Run(ctx, env); code != cli.ExitOK {
			return shutdown.ExitError(code)
		}

		return nil
	})
}
