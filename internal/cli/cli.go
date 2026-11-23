// Package cli is the roborock command: the Roborock account, and the
// devices it holds. The wiring of flags and output over the account
// package, with the file a login is kept in, which is the program's and
// not the library's.
package cli

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/alecthomas/kong"

	"github.com/duncanite/go-roborock/internal/terminal"
	"github.com/duncanite/go-roborock/roborock/account"
)

// Exit statuses.
const (
	ExitOK    = 0
	ExitError = 1
)

// EnvEmail names the variable the account's address is read from.
const EnvEmail = "ROBOROCK_EMAIL"

// errUsage is a command line the command cannot act on.
var errUsage = errors.New("cannot run")

// exitStatus is how the parser's own exit, on --help, leaves Run: kong
// calls its exit function and goes on, so the function panics with the
// status and Run recovers it.
type exitStatus int

// Environment is what the command runs in.
type Environment struct {
	// Args is its arguments, without its name.
	Args []string
	// Out and Err are where it writes.
	Out io.Writer
	Err io.Writer
	// In is where it reads what is typed: the code Roborock mails.
	In io.Reader
	// Getenv reads the environment, where the account's address is.
	Getenv func(string) string
	// Account is where the account's API is, and what reaches it; the
	// zero value is Roborock's own.
	Account account.Options
}

type rootCommand struct {
	Session string        `env:"ROBOROCK_SESSION" help:"The file the account's login and its devices' keys are kept in. What it holds opens the devices."`
	Timeout time.Duration `default:"30s"          env:"ROBOROCK_TIMEOUT"                                                                                  help:"How long Roborock is given to answer."`

	Account accountCommand `cmd:"" help:"Log in to the Roborock account ROBOROCK_EMAIL names, with the code Roborock then mails, and keep what reaches its devices in the session file."`
	Devices devicesCommand `cmd:"" help:"Print the devices the session file holds."`

	in     io.Reader
	out    io.Writer
	err    io.Writer
	getenv func(string) string
	api    account.Options
}

type (
	accountCommand struct{}
	devicesCommand struct{}
)

// Run parses the arguments and runs the command.
func Run(ctx context.Context, env Environment) (status int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			exited, ok := recovered.(exitStatus)
			if !ok {
				panic(recovered)
			}

			status = int(exited)
		}
	}()

	root := rootCommand{in: env.In, out: env.Out, err: env.Err, getenv: env.Getenv, api: env.Account}
	if root.getenv == nil {
		root.getenv = func(string) string { return "" }
	}

	parser, err := kong.New(
		&root,
		kong.Name("roborock"),
		kong.Description("Reach the Roborock devices of an account."),
		kong.UsageOnError(),
		kong.Writers(env.Out, env.Err),
		kong.Exit(func(code int) { panic(exitStatus(code)) }),
		kong.BindTo(ctx, (*context.Context)(nil)),
	)
	if err != nil {
		terminal.Printf(env.Err, "roborock: %s\n", err)

		return ExitError
	}

	kctx, err := parser.Parse(env.Args)
	if err != nil {
		parser.Errorf("%s", err)

		return ExitError
	}

	if err = kctx.Run(&root); err != nil {
		terminal.Printf(env.Err, "roborock: %s\n", err)

		return ExitError
	}

	return ExitOK
}
