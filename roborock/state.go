package roborock

import (
	"context"
	"encoding/json"
)

// Activity is what a robot is doing.
type Activity string

// What a robot is doing, as it says it by a number.
const (
	// Asleep: idle for long enough to have gone to sleep.
	Asleep Activity = "asleep"
	// Idle: awake, waiting to be told.
	Idle Activity = "idle"
	// Paused: in a clean that was paused.
	Paused Activity = "paused"
	// Docking: on its way back to the dock.
	Docking Activity = "docking"
	// Charging: on the dock.
	Charging Activity = "charging"
	// Cleaning: vacuuming, mopping or both; Mode says which.
	Cleaning Activity = "cleaning"
	// Updating: taking a firmware.
	Updating Activity = "updating"
	// Washing and Drying: at a dock that washes and dries the mop.
	Washing Activity = "washing the mop"
	Drying  Activity = "drying the mop"
	// Resting: asleep in the middle of a task.
	Resting Activity = "asleep in a task"
)

// Suction is how hard a robot vacuums.
type Suction string

// The suctions, weakest first.
const (
	SuctionQuiet    Suction = "quiet"
	SuctionBalanced Suction = "balanced"
	SuctionTurbo    Suction = "turbo"
	SuctionMax      Suction = "max"
	SuctionMaxPlus  Suction = "max+"
)

// Water is how much water a robot mops with.
type Water string

// The amounts of water, least first.
const (
	WaterLow    Water = "low"
	WaterMedium Water = "medium"
	WaterHigh   Water = "high"
)

// Mode is what a robot cleans with.
type Mode string

// The modes.
const (
	ModeVacuum Mode = "vacuum"
	ModeBoth   Mode = "vacuum and mop"
	ModeMop    Mode = "mop"
)

// Fault is what a robot reports as its fault, by Roborock's number. Not
// every one is a failure: a robot back from a clean reports one for as
// long as it sits on its dock.
type Fault int

// The faults that are none.
const (
	// faultDone and faultDocked: a clean has ended, and the robot is on
	// its way to the dock, or on it.
	faultDone   Fault = 2102
	faultDocked Fault = 2103
)

// Failure says the fault is one the open implementations name as
// something for a person to see to. A number they have no name for is not
// said to be one: a robot sitting on its dock, charged, reports such a
// number, 2105.
func (f Fault) Failure() bool { return f != 0 && f != faultDone && f != faultDocked && f.Name() != "" }

// Name is what the open implementations call the fault; empty for one
// they have no name for.
func (f Fault) Name() string {
	return map[Fault]string{
		0:           "none",
		407:         "a scheduled clean skipped, one is running",
		500:         "lidar blocked",
		501:         "robot lifted or hanging",
		502:         "battery low",
		503:         "dustbin not installed",
		509:         "cliff sensor error",
		510:         "bumper stuck",
		511:         "cannot dock",
		512:         "cannot dock",
		513:         "robot trapped",
		514:         "robot trapped",
		518:         "battery low",
		522:         "mop not installed",
		533:         "shutting down after a long sleep",
		534:         "shutting down, battery low",
		560:         "side brush entangled",
		568:         "main wheels entangled",
		569:         "main wheels entangled",
		570:         "main brush entangled",
		572:         "main brush entangled",
		594:         "dust bag not installed",
		611:         "positioning failed",
		612:         "map changed",
		629:         "mop mount fell off",
		668:         "system error",
		2003:        "battery low, scheduled clean cancelled",
		2007:        "cannot reach the target",
		2012:        "cannot reach the target",
		2100:        "battery low, will resume after charging",
		faultDone:   "clean complete",
		faultDocked: "clean complete, docked",
	}[f]
}

// State is what a robot says of itself. Each part is nil when the robot
// did not say it, or said it in a number this package has no word for: a
// reading is never a default.
type State struct {
	// Activity is what it is doing.
	Activity *Activity
	// Battery is its charge, in percent.
	Battery *int
	// Fault is what it reports as its fault.
	Fault *Fault
	// Suction, Water and Mode are how it is set to clean.
	Suction *Suction
	Water   *Water
	Mode    *Mode
	// Emptying says its dock is emptying its dustbin.
	Emptying *bool
}

// The properties a state is read from, by the names the protocol gives
// them.
const (
	propStatus  = "status"
	propBattery = "quantity"
	propFault   = "fault"
	propSuction = "wind"
	propWater   = "water"
	propMode    = "mode"
	propStation = "station_act"
	propDust    = "dust_action"
)

// State asks the robot what it is doing. The robot tells nothing of its
// own accord that this package reads: a state is had by asking.
func (r *Robot) State(ctx context.Context) (State, error) {
	properties, err := r.Properties(ctx,
		propStatus, propFault, propBattery, propSuction, propWater, propMode, propStation, propDust)
	if err != nil {
		return State{}, err
	}

	var state State

	state.Activity = word(properties[propStatus], map[int]Activity{
		0: Asleep, 1: Idle, 2: Paused, 3: Docking, 4: Charging, 5: Cleaning, 6: Cleaning, 7: Cleaning,
		8: Updating, 9: Washing, 10: Drying, 11: Resting,
	})
	state.Suction = word(properties[propSuction], map[int]Suction{
		1: SuctionQuiet, 2: SuctionBalanced, 3: SuctionTurbo, 4: SuctionMax, 5: SuctionMaxPlus,
	})
	state.Water = word(properties[propWater], map[int]Water{1: WaterLow, 2: WaterMedium, 3: WaterHigh})
	state.Mode = word(properties[propMode], map[int]Mode{0: ModeVacuum, 1: ModeBoth, 2: ModeMop})

	const full = 100

	if battery, said := number(properties[propBattery]); said && battery >= 0 && battery <= full {
		state.Battery = &battery
	}

	if code, said := number(properties[propFault]); said {
		fault := Fault(code)
		state.Fault = &fault
	}

	state.Emptying = emptying(properties[propStation], properties[propDust])

	return state, nil
}

// number is a whole number a robot wrote, and whether it wrote one.
func number(raw json.RawMessage) (int, bool) {
	var value int

	// A null decodes into a number as nothing, with no error.
	if raw == nil || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return 0, false
	}

	return value, true
}

// word is the word for a number a robot wrote; nil when it wrote none,
// or one there is no word for.
func word[W any](raw json.RawMessage, words map[int]W) *W {
	value, said := number(raw)
	if !said {
		return nil
	}

	known, has := words[value]
	if !has {
		return nil
	}

	return &known
}

// emptying reads the two properties a dock's emptying shows in, as one
// robot was seen to write them: three and one while it empties, zero and
// zero otherwise. Anything else is not read.
func emptying(station, dust json.RawMessage) *bool {
	const collecting = 3

	stationed, saidStation := number(station)
	dusting, saidDust := number(dust)

	var busy bool

	switch {
	case saidStation && stationed == collecting, saidDust && dusting == 1:
		busy = true
	case saidStation && stationed == 0 && (!saidDust || dusting == 0), saidDust && dusting == 0 && !saidStation:
	default:
		return nil
	}

	return &busy
}
