package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duncanite/go-roborock/internal/cli"
	"github.com/duncanite/go-roborock/roborock"
	"github.com/duncanite/go-roborock/roborock/roborocktest"
)

// device is one device of a session file.
type device map[string]any

// clouded is a cloud and a session file for its account, holding the
// devices given; with none, the cloud's own robot.
func clouded(t *testing.T, devices ...device) (*roborocktest.Cloud, string) {
	t.Helper()

	cloud, err := roborocktest.New()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cloud.Close)

	if devices == nil {
		devices = []device{robot("Downstairs")}
	}

	credentials := cloud.Cloud()

	raw, err := json.Marshal(map[string]any{
		"cloud": map[string]string{
			"broker": credentials.Broker,
			"user":   credentials.User,
			"secret": credentials.Secret,
			"key":    credentials.Key,
		},
		"devices": devices,
	})
	if err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "session.json")
	if err = os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	return cloud, file
}

// robot is the cloud's robot, under a name.
func robot(name string) device {
	return device{
		"name": name, "id": roborocktest.RobotID, "protocol": "B01", "model": roborocktest.Model,
		"firmware": "02.10.20", "shared": false, "key": roborocktest.RobotKey,
	}
}

// spoke runs the command against a cloud.
func spoke(t *testing.T, cloud *roborocktest.Cloud, args ...string) ran {
	t.Helper()

	var out, err bytes.Buffer

	status := cli.Run(t.Context(), cli.Environment{
		Args: args, Out: &out, Err: &err, Cloud: roborock.Options{TLS: cloud.TLS()},
	})

	return ran{status: status, out: out.String(), err: err.String()}
}

// TestStatus: what the robot is doing is printed a part to a line, a
// fault that is none said as it is, one that is a failure said to be,
// one with no name printed as its number alone, and a dash for what the
// robot did not say.
func TestStatus(t *testing.T) {
	t.Parallel()

	for name, reading := range map[string]struct {
		properties map[string]any
		want       string
	}{
		"docked after a clean": {
			nil,
			"ACTIVITY  charging\nBATTERY   100%\nFAULT     2103, clean complete, docked\nSUCTION   balanced\n" +
				"WATER     low\nMODE      vacuum\nDOCK      idle\n",
		},
		"stuck": {
			map[string]any{"status": 2, "quantity": 41, "fault": 500, "wind": 5, "water": 3, "mode": 1, "station_act": 3},
			"ACTIVITY  paused\nBATTERY   41%\nFAULT     500, lidar blocked (a failure)\nSUCTION   max+\n" +
				"WATER     high\nMODE      vacuum and mop\nDOCK      emptying the dustbin\n",
		},
		"no fault, and one with no name": {
			map[string]any{"status": 1, "fault": 0},
			"ACTIVITY  idle\nBATTERY   -\nFAULT     none\nSUCTION   -\nWATER     -\nMODE      -\nDOCK      -\n",
		},
		"a fault with no name": {
			map[string]any{"fault": 9999},
			"ACTIVITY  -\nBATTERY   -\nFAULT     9999\nSUCTION   -\nWATER     -\nMODE      -\nDOCK      -\n",
		},
		"nothing said": {
			map[string]any{},
			"ACTIVITY  -\nBATTERY   -\nFAULT     -\nSUCTION   -\nWATER     -\nMODE      -\nDOCK      -\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cloud, file := clouded(t)

			if reading.properties != nil {
				cloud.Set(func(c *roborocktest.Cloud) { c.Properties = reading.properties })
			}

			got := spoke(t, cloud, "--session", file, "status")

			if got.status != cli.ExitOK || got.out != reading.want || got.err != "" {
				t.Errorf("status = %d, printing\n%s\nand %q; want\n%s", got.status, got.out, got.err, reading.want)
			}
		})
	}
}

// TestDriving: each command asks the robot for the one thing, says the
// robot took it, and status then says what the robot does.
func TestDriving(t *testing.T) {
	t.Parallel()

	cloud, file := clouded(t)

	for _, step := range []struct {
		command string
		says    string
		asked   string
		then    string
	}{
		{"start", "to clean", `service.set_room_clean {"clean_type":0,"ctrl_value":1,"room_ids":[]}`, "cleaning"},
		{"pause", "to pause", `service.set_room_clean {"clean_type":0,"ctrl_value":2,"room_ids":[]}`, "paused"},
		{"stop", "to stop", `service.set_room_clean {"clean_type":0,"ctrl_value":0,"room_ids":[]}`, "idle"},
		{"dock", "to go back to its dock", `service.start_recharge {}`, "docking"},
		{"locate", "to say where it is", `service.find_device {}`, "docking"},
	} {
		got := spoke(t, cloud, "--session", file, step.command)

		if got.status != cli.ExitOK || !strings.Contains(got.out, step.says) || got.err != "" {
			t.Fatalf("%s = %d, printing %q and %q", step.command, got.status, got.out, got.err)
		}

		calls := cloud.Calls()
		if last := calls[len(calls)-1]; last.Method+" "+last.Params != step.asked {
			t.Errorf("%s asked the robot %s %s, want %s", step.command, last.Method, last.Params, step.asked)
		}

		if after := spoke(
			t,
			cloud,
			"--session",
			file,
			"status",
		); !strings.Contains(
			after.out,
			"ACTIVITY  "+step.then+"\n",
		) {
			t.Errorf("after %s, status prints\n%s\nwant the robot %s", step.command, after.out, step.then)
		}
	}
}

// TestGetAndCall: properties are printed as the robot wrote them, which
// is JSON and holds no character a terminal obeys, in the order of their
// names; a method is asked with its parameters, and its answer printed.
func TestGetAndCall(t *testing.T) {
	t.Parallel()

	cloud, file := clouded(t)
	cloud.Set(func(c *roborocktest.Cloud) { c.Properties["name"] = "a\x1b[2Jb" })

	got := spoke(t, cloud, "--session", file, "get", "quantity", "net_status", "name", "nothing_such")

	want := "name        \"a\\u001b[2Jb\"\nnet_status  {\"rssi\":-40}\nquantity    100\n"
	if got.status != cli.ExitOK || got.out != want {
		t.Errorf("get = %d, printing %q and %q; want %q", got.status, got.out, got.err, want)
	}

	got = spoke(t, cloud, "--session", file, "call", "service.anything", `{"a": [1, 2]}`)
	if got.status != cli.ExitOK || got.out != "\"ok\"\n" {
		t.Errorf("call = %d, printing %q and %q", got.status, got.out, got.err)
	}

	calls := cloud.Calls()
	if last := calls[len(calls)-1]; last.Method != "service.anything" || last.Params != `{"a":[1,2]}` {
		t.Errorf("call asked the robot %+v", last)
	}

	got = spoke(t, cloud, "--session", file, "call", "service.find_device")
	if calls = cloud.Calls(); got.status != cli.ExitOK || calls[len(calls)-1].Params != "{}" {
		t.Errorf("call with no parameters = %d, asking %+v", got.status, calls[len(calls)-1])
	}

	before := len(cloud.Calls())

	got = spoke(t, cloud, "--session", file, "call", "service.anything", "{no json")
	if got.status != cli.ExitError || !strings.Contains(got.err, "not JSON") || len(cloud.Calls()) != before {
		t.Errorf("call with what is no JSON = %d, saying %q", got.status, got.err)
	}
}

// TestWhichRobot: the command speaks to the only robot the session file
// holds that it speaks to; when there are several, or when one is
// named, to the one --device names by its name or its id. What it cannot
// tell is said, and nothing is asked of the cloud.
func TestWhichRobot(t *testing.T) {
	t.Parallel()

	other := device{
		"name": "Garage", "id": "duid-other", "protocol": "1.0", "model": "roborock.vacuum.a15",
		"firmware": "", "shared": true, "key": "key-of-the-other",
	}
	second := robot("Upstairs")
	second["id"] = "duid-second"

	twin := robot("Downstairs")
	twin["id"] = "duid-twin"

	for name, attempt := range map[string]struct {
		devices []device
		args    []string
		says    string
	}{
		"the only one spoken to":       {[]device{other, robot("Downstairs")}, nil, ""},
		"named among several":          {[]device{robot("Downstairs"), second}, []string{"--device", "Downstairs"}, ""},
		"by its id":                    {[]device{robot("Downstairs"), second, twin}, []string{"--device", roborocktest.RobotID}, ""},
		"several, none named":          {[]device{robot("Downstairs"), second}, nil, "holds 2 robots; --device names one"},
		"none spoken to":               {[]device{other}, nil, "holds no robot this command speaks to"},
		"no device at all":             {[]device{}, nil, "holds no robot this command speaks to"},
		"a name nothing has":           {[]device{robot("Downstairs")}, []string{"--device", "Attic"}, `no device named "Attic"`},
		"a name two have":              {[]device{robot("Downstairs"), twin}, []string{"--device", "Downstairs"}, "2 devices named"},
		"one named that is not spoken": {[]device{other, robot("Downstairs")}, []string{"--device", "Garage"}, "not a device this package speaks to"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cloud, file := clouded(t, attempt.devices...)

			got := spoke(t, cloud, append(append([]string{"--session", file}, attempt.args...), "status")...)

			if attempt.says == "" {
				if got.status != cli.ExitOK || !strings.Contains(got.out, "ACTIVITY  charging") {
					t.Errorf("status = %d, printing %q and %q", got.status, got.out, got.err)
				}

				return
			}

			if got.status != cli.ExitError || !strings.Contains(got.err, attempt.says) || got.out != "" {
				t.Errorf("status = %d, saying %q; want an error with %q", got.status, got.err, attempt.says)
			}

			if len(cloud.Calls()) != 0 {
				t.Error("a robot was asked")
			}
		})
	}
}

// TestNotAnswered: what the cloud or the robot refuses is said with what
// a person does about it: credentials the cloud takes no more, a robot
// that answers nothing, a robot that refuses.
func TestNotAnswered(t *testing.T) {
	t.Parallel()

	for name, attempt := range map[string]struct {
		change func(*roborocktest.Cloud)
		says   []string
	}{
		"credentials refused": {
			func(c *roborocktest.Cloud) { c.Reason = 135 },
			[]string{"refused the account's credentials", "account logs in anew"},
		},
		"a robot that says nothing": {
			func(c *roborocktest.Cloud) { c.Silent = true },
			[]string{"waiting for the robot to answer prop.get", "answers nothing"},
		},
		"a robot that refuses": {
			func(c *roborocktest.Cloud) { c.Refuse = map[string]int{"prop.get": 7} },
			[]string{"by the robot, prop.get, code 7"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cloud, file := clouded(t)
			cloud.Set(attempt.change)

			got := spoke(t, cloud, "--session", file, "--timeout", "500ms", "status")

			for _, said := range attempt.says {
				if got.status != cli.ExitError || !strings.Contains(got.err, said) || got.out != "" {
					t.Errorf("status = %d, saying %q; want an error with %q", got.status, got.err, said)
				}
			}

			if strings.Contains(got.err, roborocktest.RobotKey) || strings.Contains(got.err, roborocktest.Secret) {
				t.Errorf("status says a secret: %q", got.err)
			}
		})
	}

	cloud, file := clouded(t)

	if got := spoke(t, cloud, "--session", file, "--timeout", "0s", "status"); got.status != cli.ExitError ||
		!strings.Contains(got.err, "--timeout") {
		t.Errorf("status with no time = %d, saying %q", got.status, got.err)
	}

	if got := spoke(t, cloud, "status"); got.status != cli.ExitError || !strings.Contains(got.err, "--session") {
		t.Errorf("status with no session file = %d, saying %q", got.status, got.err)
	}
}
