package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/duncanite/go-roborock/internal/terminal"
	"github.com/duncanite/go-roborock/roborock"
)

// picked is the device of the session file the command speaks to: the
// one --device names, by its id or by its name, or the only one there is
// that is spoken to.
func (root *rootCommand) picked(devices []keptDevice) (keptDevice, error) {
	var among []keptDevice

	for _, device := range devices {
		named := device.ID == root.Device || device.Name == root.Device
		spoken := roborock.Speaks(roborock.Identity{Protocol: device.Protocol, Model: device.Model})

		if (root.Device != "" && named) || (root.Device == "" && spoken) {
			among = append(among, device)
		}
	}

	switch {
	case len(among) == 1:
		return among[0], nil
	case root.Device != "" && len(among) == 0:
		return keptDevice{}, fmt.Errorf("%w: the session file holds no device named %q; devices prints them",
			errUsage, root.Device)
	case root.Device != "":
		return keptDevice{}, fmt.Errorf("%w: the session file holds %d devices named %q; --device takes an id too",
			errUsage, len(among), root.Device)
	case len(among) == 0:
		return keptDevice{}, fmt.Errorf("%w: the session file holds no robot this command speaks to", errUsage)
	default:
		return keptDevice{}, fmt.Errorf("%w: the session file holds %d robots; --device names one",
			errUsage, len(among))
	}
}

// with opens the robot the command speaks to and does one thing with it,
// all within --timeout.
func (root *rootCommand) with(ctx context.Context, action func(context.Context, *roborock.Robot) error) error {
	if root.Timeout <= 0 {
		return fmt.Errorf("%w: --timeout wants a time above zero", errUsage)
	}

	holding, err := root.kept()
	if err != nil {
		return err
	}

	device, err := root.picked(holding.Devices)
	if err != nil {
		return err
	}

	ctx, done := context.WithTimeout(ctx, root.Timeout)
	defer done()

	session, err := roborock.Connect(ctx, roborock.Cloud{
		Broker: holding.Cloud.Broker, User: holding.Cloud.User, Secret: holding.Cloud.Secret, Key: holding.Cloud.Key,
	}, root.cloud)
	if err != nil {
		return said(err)
	}

	defer func() { _ = session.Close() }()

	robot, err := session.Robot(ctx, roborock.Identity{
		ID: device.ID, Protocol: device.Protocol, Model: device.Model, Key: device.Key,
	})
	if err != nil {
		return said(err)
	}

	return said(action(ctx, robot))
}

// said is an error with what a person does about it, where there is
// something.
func said(err error) error {
	switch {
	case errors.Is(err, roborock.ErrCredentials):
		return fmt.Errorf("%w; account logs in anew", err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w; a robot that is off, or out of reach of its network, answers nothing", err)
	default:
		return err
	}
}

// Run prints what the robot is doing: each part of its state, and a dash
// for what it did not say.
func (*statusCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, robot *roborock.Robot) error {
		state, err := robot.State(ctx)
		if err != nil {
			return err
		}

		fault := "-"

		switch {
		case state.Fault == nil:
		case state.Fault.Name() == "":
			fault = strconv.Itoa(int(*state.Fault))
		case *state.Fault == 0:
			fault = state.Fault.Name()
		default:
			fault = fmt.Sprintf("%d, %s", int(*state.Fault), state.Fault.Name())
		}

		if state.Fault != nil && state.Fault.Failure() {
			fault += " (a failure)"
		}

		battery := "-"
		if state.Battery != nil {
			battery = fmt.Sprintf("%d%%", *state.Battery)
		}

		dock := "-"
		if state.Emptying != nil {
			dock = map[bool]string{false: "idle", true: "emptying the dustbin"}[*state.Emptying]
		}

		table := tabwriter.NewWriter(root.out, 0, 0, 2, ' ', 0)

		terminal.Printf(table, "ACTIVITY\t%s\nBATTERY\t%s\nFAULT\t%s\nSUCTION\t%s\nWATER\t%s\nMODE\t%s\nDOCK\t%s\n",
			word(state.Activity), battery, fault, word(state.Suction), word(state.Water), word(state.Mode), dock)

		if err = table.Flush(); err != nil {
			return fmt.Errorf("writing the state: %w", err)
		}

		return nil
	})
}

// word is a reading, or a dash where the robot gave none.
func word[W ~string](reading *W) string {
	if reading == nil {
		return "-"
	}

	return string(*reading)
}

// drive has the robot do one thing, and says it took the request: what
// it then does is status's to print.
func (root *rootCommand) drive(
	ctx context.Context,
	taken string,
	do func(*roborock.Robot, context.Context) error,
) error {
	return root.with(ctx, func(ctx context.Context, robot *roborock.Robot) error {
		if err := do(robot, ctx); err != nil {
			return err
		}

		terminal.Printf(root.out, "%s\n", taken)

		return nil
	})
}

// Run has the robot clean the whole home.
func (*startCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.drive(ctx, "The robot took the request to clean.", (*roborock.Robot).Start)
}

// Run has the robot pause its clean.
func (*pauseCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.drive(ctx, "The robot took the request to pause.", (*roborock.Robot).Pause)
}

// Run has the robot end its clean.
func (*stopCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.drive(ctx, "The robot took the request to stop.", (*roborock.Robot).Stop)
}

// Run sends the robot back to its dock.
func (*dockCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.drive(ctx, "The robot took the request to go back to its dock.", (*roborock.Robot).Dock)
}

// Run has the robot say where it is.
func (*locateCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.drive(ctx, "The robot took the request to say where it is.", (*roborock.Robot).Locate)
}

// Run prints the properties named, each as the robot wrote it, in the
// order of their names; one the robot has not is not printed.
func (command *getCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, robot *roborock.Robot) error {
		properties, err := robot.Properties(ctx, command.Names...)
		if err != nil {
			return err
		}

		table := tabwriter.NewWriter(root.out, 0, 0, 2, ' ', 0)

		for _, name := range slices.Sorted(maps.Keys(properties)) {
			terminal.Printf(table, "%s\t%s\n", name, string(properties[name]))
		}

		if err = table.Flush(); err != nil {
			return fmt.Errorf("writing the properties: %w", err)
		}

		return nil
	})
}

// Run asks the robot for a method and prints the data of its answer.
func (command *callCommand) Run(ctx context.Context, root *rootCommand) error {
	var params json.RawMessage

	if err := json.Unmarshal([]byte(command.Params), &params); err != nil {
		return fmt.Errorf("%w: the parameters are not JSON", errUsage)
	}

	return root.with(ctx, func(ctx context.Context, robot *roborock.Robot) error {
		data, err := robot.Call(ctx, command.Method, params)
		if err != nil {
			return err
		}

		terminal.Printf(root.out, "%s\n", strings.TrimSpace(string(data)))

		return nil
	})
}
