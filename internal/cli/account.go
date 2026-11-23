package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/mycophonic/primordium/filesystem"

	"github.com/duncanite/go-roborock/internal/terminal"
	"github.com/duncanite/go-roborock/roborock"
	"github.com/duncanite/go-roborock/roborock/account"
)

const (
	// maxCode bounds what is read where the code is typed: more than that
	// is no code, and is not read on.
	maxCode = 256
	// maxKept bounds what is read of a session file.
	maxKept = 4 << 20
)

// kept is a session file: the account's way into Roborock's cloud, and
// each device with its key.
type kept struct {
	Cloud   keptCloud    `json:"cloud"`
	Devices []keptDevice `json:"devices"`
}

type keptCloud struct {
	Broker string `json:"broker"`
	User   string `json:"user"`
	Secret string `json:"secret"`
	Key    string `json:"key"`
}

type keptDevice struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	Firmware string `json:"firmware"`
	Shared   bool   `json:"shared"`
	Key      string `json:"key"`
}

// Run logs in to the Roborock account the environment names, with the
// code Roborock then sends by mail, typed where it is asked for; it keeps
// what reaches the account's devices in the session file, and prints the
// devices. No key is printed.
//
// --timeout bounds what is asked of Roborock, before the code and after
// it; the typing of the code takes as long as the person does.
func (*accountCommand) Run(ctx context.Context, root *rootCommand) error {
	email := root.getenv(EnvEmail)

	switch {
	case email == "":
		return fmt.Errorf("%w: not set in the environment: %s", errUsage, EnvEmail)
	case root.Session == "":
		return fmt.Errorf("%w: --session names the file the login is kept in", errUsage)
	case root.in == nil:
		return fmt.Errorf("%w: nowhere to read the code from", errUsage)
	case root.Timeout <= 0:
		return fmt.Errorf("%w: --timeout wants a time above zero", errUsage)
	}

	client := account.New(root.api)

	asking, done := context.WithTimeout(ctx, root.Timeout)

	challenge, err := client.Ask(asking, email)

	done()

	if err != nil {
		return err
	}

	terminal.Printf(root.err, "Roborock has sent a code to the account's address. The code: ")

	code, err := typed(root.in)
	if err != nil {
		return err
	}

	fetching, done := context.WithTimeout(ctx, root.Timeout)
	defer done()

	session, err := client.Answer(fetching, challenge, code)
	if err != nil {
		return err
	}

	// A device that could not be read is said after the others, which are
	// kept and printed all the same.
	devices, unread := client.Devices(fetching, session)
	if unread != nil && !errors.Is(unread, account.ErrUnread) {
		return unread
	}

	holding := held(session.Cloud(), devices)

	if err = holding.write(root.Session); err != nil {
		return err
	}

	terminal.Printf(root.err, "Kept in %s: it opens the devices, keep it as a password is kept.\n", root.Session)

	if err = root.list(holding.Devices, unread == nil); err != nil {
		return err
	}

	return unread
}

// Run prints the devices the session file holds, with no key.
func (*devicesCommand) Run(root *rootCommand) error {
	holding, err := root.kept()
	if err != nil {
		return err
	}

	return root.list(holding.Devices, true)
}

// held is what a session file holds for a login and its devices.
func held(cloud roborock.Cloud, devices []account.Device) kept {
	holding := kept{
		Cloud:   keptCloud{Broker: cloud.Broker, User: cloud.User, Secret: cloud.Secret, Key: cloud.Key},
		Devices: make([]keptDevice, 0, len(devices)),
	}

	for _, device := range devices {
		holding.Devices = append(holding.Devices, keptDevice{
			Name:     device.Name,
			ID:       device.Identity.ID,
			Protocol: device.Identity.Protocol,
			Model:    device.Identity.Model,
			Firmware: device.Firmware,
			Shared:   device.Shared,
			Key:      device.Identity.Key,
		})
	}

	return holding
}

// write keeps it in a file its owner alone reads, in a directory made
// the same when there is none.
func (holding kept) write(name string) error {
	const (
		private = 0o700
		owner   = 0o600
	)

	raw, err := json.MarshalIndent(holding, "", "  ")
	if err != nil {
		return fmt.Errorf("writing the session: %w", err)
	}

	if err = os.MkdirAll(filepath.Dir(name), private); err != nil {
		return fmt.Errorf("the directory of the session file: %w", err)
	}

	if err = filesystem.WriteFile(name, append(raw, '\n'), owner); err != nil {
		return fmt.Errorf("writing the session file: %w", err)
	}

	return nil
}

// kept reads the session file. What is in it is not said when it does
// not read: it holds keys.
func (root *rootCommand) kept() (kept, error) {
	if root.Session == "" {
		return kept{}, fmt.Errorf("%w: --session names the file the login is kept in", errUsage)
	}

	file, err := os.Open(root.Session)
	if err != nil {
		return kept{}, fmt.Errorf("the session file: %w", err)
	}

	defer func() { _ = file.Close() }()

	raw, err := io.ReadAll(io.LimitReader(file, maxKept+1))
	if err != nil {
		return kept{}, fmt.Errorf("reading the session file: %w", err)
	}

	var holding kept

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if len(raw) > maxKept || decoder.Decode(&holding) != nil || holding.Cloud.Broker == "" {
		return kept{}, fmt.Errorf("%w: %s is not a session file; account writes one", errUsage, root.Session)
	}

	return holding, nil
}

// list prints the devices. That there are none is said only when the
// list was whole.
func (root *rootCommand) list(devices []keptDevice, whole bool) error {
	if len(devices) == 0 {
		if whole {
			terminal.Printf(root.out, "The account has no device.\n")
		}

		return nil
	}

	table := tabwriter.NewWriter(root.out, 0, 0, 2, ' ', 0)

	terminal.Printf(table, "NAME\tMODEL\tPROTOCOL\tFIRMWARE\tSHARED\tID\n")

	for _, device := range devices {
		shared := "no"
		if device.Shared {
			shared = "yes"
		}

		terminal.Printf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shown(device.Name), shown(device.Model), shown(device.Protocol), shown(device.Firmware), shared, device.ID)
	}

	if err := table.Flush(); err != nil {
		return fmt.Errorf("writing the devices: %w", err)
	}

	return nil
}

// shown is a text, or a dash where there is none: a column is never
// empty.
func shown(text string) string {
	if text == "" {
		return "-"
	}

	return text
}

// typed is the code read from where it is typed: one line, without the
// space around it. An empty line is no code, and neither is more than a
// code may be.
func typed(from io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(from, maxCode+1)).ReadString('\n')

	if len(line) > maxCode {
		return "", fmt.Errorf("%w: more than %d characters typed where the code goes", errUsage, maxCode)
	}

	code := strings.TrimSpace(line)
	if code == "" {
		if err == nil || errors.Is(err, io.EOF) {
			err = errUsage
		}

		return "", fmt.Errorf("no code was typed: %w", err)
	}

	return code, nil
}
