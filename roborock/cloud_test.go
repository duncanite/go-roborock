package roborock_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncanite/go-roborock/roborock"
	"github.com/duncanite/go-roborock/roborock/roborocktest"
)

// served is a cloud, a session with it and its robot.
func served(t *testing.T) (*roborocktest.Cloud, *roborock.Session, *roborock.Robot) {
	t.Helper()

	cloud, err := roborocktest.New()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cloud.Close)

	session, err := roborock.Connect(t.Context(), cloud.Cloud(), roborock.Options{TLS: cloud.TLS()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	t.Cleanup(func() { _ = session.Close() })

	robot, err := session.Robot(t.Context(), cloud.Identity())
	if err != nil {
		t.Fatalf("Robot: %v", err)
	}

	return cloud, session, robot
}

// brief is a context for what is to end soon.
func brief(t *testing.T) context.Context {
	t.Helper()

	ctx, done := context.WithTimeout(t.Context(), 300*time.Millisecond)
	t.Cleanup(done)

	return ctx
}

// TestCredentials: the user name and the password the test broker takes
// are those the reference implementation derives, so that a session it
// takes was opened as Roborock's broker wants it.
func TestCredentials(t *testing.T) {
	t.Parallel()

	// python-roborock's own test values, and what its client then sends.
	user, password := roborocktest.Credentials(roborock.Cloud{User: "user123", Secret: "pass123", Key: "qiCNieZa"})

	if user != "19648f94" || password != "234678ea854f199e" {
		t.Errorf("Credentials = %q, %q; want 19648f94 and 234678ea854f199e", user, password)
	}
}

// TestRobot: a robot says what it is doing, in words; it is driven, and
// says what it then does. Each request is the one the open
// implementations send, under a number of its own of twelve digits.
func TestRobot(t *testing.T) {
	t.Parallel()

	cloud, _, robot := served(t)

	state, err := robot.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	if state.Activity == nil || *state.Activity != roborock.Charging || state.Battery == nil || *state.Battery != 100 ||
		state.Suction == nil || *state.Suction != roborock.SuctionBalanced ||
		state.Water == nil || *state.Water != roborock.WaterLow ||
		state.Mode == nil || *state.Mode != roborock.ModeVacuum ||
		state.Emptying == nil || *state.Emptying {
		t.Errorf("State = %s, want a robot charging at 100, balanced, low, vacuum, not emptying", shown(state))
	}

	// A robot back from a clean reports a fault that is none.
	if state.Fault == nil || *state.Fault != 2103 || state.Fault.Failure() ||
		state.Fault.Name() != "clean complete, docked" {
		t.Errorf("Fault = %s, want 2103, which is no failure", shown(state))
	}

	for _, step := range []struct {
		name  string
		drive func(context.Context) error
		then  roborock.Activity
	}{
		{"Start", robot.Start, roborock.Cleaning},
		{"Pause", robot.Pause, roborock.Paused},
		{"Stop", robot.Stop, roborock.Idle},
		{"Dock", robot.Dock, roborock.Docking},
		{"Locate", robot.Locate, roborock.Docking},
	} {
		if err = step.drive(t.Context()); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}

		state, err = robot.State(t.Context())
		if err != nil || state.Activity == nil || *state.Activity != step.then {
			t.Errorf("after %s the robot is %s, %v; want %s", step.name, shown(state), err, step.then)
		}
	}

	want := []string{
		`prop.get {"property":["status","fault","quantity","wind","water","mode","station_act","dust_action"]}`,
		`service.set_room_clean {"clean_type":0,"ctrl_value":1,"room_ids":[]}`,
		`service.set_room_clean {"clean_type":0,"ctrl_value":2,"room_ids":[]}`,
		`service.set_room_clean {"clean_type":0,"ctrl_value":0,"room_ids":[]}`,
		`service.start_recharge {}`,
		`service.find_device {}`,
	}

	numbers := map[string]bool{}
	twelve := regexp.MustCompile(`^[1-9]\d{11}$`)

	var got []string

	for _, call := range cloud.Calls() {
		if !twelve.MatchString(call.Number) || numbers[call.Number] {
			t.Errorf("a request is numbered %q, want twelve digits of its own", call.Number)
		}

		numbers[call.Number] = true

		if said := call.Method + " " + call.Params; call.Method != "prop.get" || len(got) == 0 {
			got = append(got, said)
		}
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the robot was asked\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if robot.Identity().ID != roborocktest.RobotID {
		t.Errorf("Identity = %v", robot.Identity())
	}
}

// shown is a state with its parts written out, for a failure's message.
func shown(state roborock.State) string {
	part := func(value any) string {
		of := reflect.ValueOf(value)
		if of.IsNil() {
			return "nil"
		}

		return fmt.Sprint(of.Elem().Interface())
	}

	return fmt.Sprintf("{activity %s, battery %s, fault %s, suction %s, water %s, mode %s, emptying %s}",
		part(state.Activity), part(state.Battery), part(state.Fault), part(state.Suction), part(state.Water),
		part(state.Mode), part(state.Emptying))
}

// TestReadings: each part of a state is the word for the number the
// robot wrote, and nil when it wrote none, or one there is no word for:
// never a default.
func TestReadings(t *testing.T) {
	t.Parallel()

	for name, reading := range map[string]struct {
		properties map[string]any
		want       string
	}{
		"nothing said": {
			map[string]any{},
			"{activity nil, battery nil, fault nil, suction nil, water nil, mode nil, emptying nil}",
		},
		"numbers with no word": {
			map[string]any{
				"status": 99, "quantity": 150, "wind": 0, "water": 4, "mode": 3, "station_act": 7, "fault": 9999,
			},
			"{activity nil, battery nil, fault 9999, suction nil, water nil, mode nil, emptying nil}",
		},
		"words where numbers go": {
			map[string]any{"status": "4", "quantity": "full", "fault": nil, "wind": 2.5, "dust_action": "1"},
			"{activity nil, battery nil, fault nil, suction nil, water nil, mode nil, emptying nil}",
		},
		"mopping, the dock emptying": {
			map[string]any{
				"status": 7, "quantity": 0, "wind": 5, "water": 3, "mode": 2, "station_act": 3, "dust_action": 1, "fault": 0,
			},
			"{activity cleaning, battery 0, fault 0, suction max+, water high, mode mop, emptying true}",
		},
		"the dust alone says emptying": {
			map[string]any{"status": 0, "dust_action": 1, "wind": 1, "water": 2, "mode": 1},
			"{activity asleep, battery nil, fault nil, suction quiet, water medium, mode vacuum and mop, emptying true}",
		},
		"the station alone says idle": {
			map[string]any{"status": 11, "station_act": 0, "wind": 3},
			"{activity asleep in a task, battery nil, fault nil, suction turbo, water nil, mode nil, emptying false}",
		},
		"the dust says something else": {
			map[string]any{"status": 8, "station_act": 0, "dust_action": 2, "wind": 4},
			"{activity updating, battery nil, fault nil, suction max, water nil, mode nil, emptying nil}",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cloud, _, robot := served(t)
			cloud.Set(func(c *roborocktest.Cloud) { c.Properties = reading.properties })

			state, err := robot.State(t.Context())
			if err != nil || shown(state) != reading.want {
				t.Errorf("State = %s, %v\nwant    %s", shown(state), err, reading.want)
			}
		})
	}

	for status, want := range map[int]roborock.Activity{
		1: roborock.Idle, 2: roborock.Paused, 3: roborock.Docking, 5: roborock.Cleaning, 6: roborock.Cleaning,
		9: roborock.Washing, 10: roborock.Drying,
	} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			t.Parallel()

			cloud, _, robot := served(t)
			cloud.Set(func(c *roborocktest.Cloud) { c.Properties = map[string]any{"status": status} })

			state, err := robot.State(t.Context())
			if err != nil || state.Activity == nil || *state.Activity != want {
				t.Errorf("State = %s, %v; want %s", shown(state), err, want)
			}
		})
	}
}

// TestFaults: a fault is a failure when the open implementations name it
// as one: not none, not the two a clean that ended leaves, and not a
// number they have no name for, which a robot at rest reports too.
func TestFaults(t *testing.T) {
	t.Parallel()

	for fault, want := range map[roborock.Fault]struct {
		failure bool
		name    string
	}{
		0:    {false, "none"},
		2102: {false, "clean complete"},
		2103: {false, "clean complete, docked"},
		500:  {true, "lidar blocked"},
		512:  {true, "cannot dock"},
		2100: {true, "battery low, will resume after charging"},
		2105: {false, ""},
		9999: {false, ""},
		-1:   {false, ""},
	} {
		if fault.Failure() != want.failure || fault.Name() != want.name {
			t.Errorf("the fault %d is a failure: %t, named %q; want %t and %q",
				fault, fault.Failure(), fault.Name(), want.failure, want.name)
		}
	}
}

// TestAnswers: an answer is found among what a robot says beside it, and
// whether it is padded once or twice; a refusal carries the robot's code;
// an answer with no data is one for a command, and none for a state; and
// a robot that says nothing is waited for as long as the context lasts.
func TestAnswers(t *testing.T) {
	t.Parallel()

	t.Run("among other things, padded once", func(t *testing.T) {
		t.Parallel()

		for _, once := range []bool{false, true} {
			cloud, _, robot := served(t)
			cloud.Set(func(c *roborocktest.Cloud) { c.Noise, c.Once = true, once })

			state, err := robot.State(t.Context())
			if err != nil || state.Battery == nil || *state.Battery != 100 {
				t.Errorf("State with answers padded once: %t = %s, %v", once, shown(state), err)
			}
		}
	})

	t.Run("refused", func(t *testing.T) {
		t.Parallel()

		cloud, session, robot := served(t)
		cloud.Set(func(c *roborocktest.Cloud) { c.Refuse = map[string]int{"service.start_recharge": 1} })

		err := robot.Dock(t.Context())

		var refused *roborock.RefusedError
		if !errors.As(err, &refused) || refused.Code != 1 || refused.Method != "service.start_recharge" ||
			!errors.Is(err, roborock.ErrRefused) {
			t.Errorf("Dock = %v, want the robot's refusal with its code 1", err)
		}

		if session.Err() != nil {
			t.Errorf("the session ended with %v on a robot's refusal", session.Err())
		}
	})

	t.Run("no data", func(t *testing.T) {
		t.Parallel()

		cloud, _, robot := served(t)
		cloud.Set(func(c *roborocktest.Cloud) { c.Bare = true })

		if err := robot.Locate(t.Context()); err != nil {
			t.Errorf("Locate answered with no data = %v, want it taken", err)
		}

		if _, err := robot.State(t.Context()); !errors.Is(err, roborock.ErrAnswer) {
			t.Errorf("State answered with no data = %v, want ErrAnswer", err)
		}
	})

	t.Run("nothing", func(t *testing.T) {
		t.Parallel()

		cloud, session, robot := served(t)
		cloud.Set(func(c *roborocktest.Cloud) { c.Silent = true })

		if _, err := robot.State(brief(t)); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("State of a robot that says nothing = %v, want the context's end", err)
		}

		// The robot is asked again, and answers.
		cloud.Set(func(c *roborocktest.Cloud) { c.Silent = false })

		if _, err := robot.State(t.Context()); err != nil || session.Err() != nil {
			t.Errorf("State after one unanswered = %v, the session %v", err, session.Err())
		}
	})
}

// TestCalls: a property is read as the robot wrote it, a name it has not
// is left out, and a method is asked by its name with its parameters.
func TestCalls(t *testing.T) {
	t.Parallel()

	cloud, _, robot := served(t)

	properties, err := robot.Properties(t.Context(), "net_status", "quantity", "nothing_such")
	if err != nil || len(properties) != 2 || string(properties["net_status"]) != `{"rssi":-40}` ||
		string(properties["quantity"]) != "100" {
		t.Errorf("Properties = %v, %v", properties, err)
	}

	data, err := robot.Call(t.Context(), "service.anything", map[string]int{"a": 1})
	if err != nil || string(data) != `"ok"` {
		t.Errorf("Call = %s, %v", data, err)
	}

	calls := cloud.Calls()
	if last := calls[len(calls)-1]; last.Method != "service.anything" || last.Params != `{"a":1}` {
		t.Errorf("the robot was asked %+v", last)
	}

	if _, err = robot.Call(t.Context(), "", nil); !errors.Is(err, roborock.ErrArgument) {
		t.Errorf("Call of no method = %v, want ErrArgument", err)
	}

	if _, err = robot.Properties(t.Context()); !errors.Is(err, roborock.ErrArgument) {
		t.Errorf("Properties of none = %v, want ErrArgument", err)
	}

	if _, err = robot.Call(t.Context(), "service.anything", func() {}); !errors.Is(err, roborock.ErrArgument) {
		t.Errorf("Call with what is no JSON = %v, want ErrArgument", err)
	}

	if len(cloud.Calls()) != len(calls) {
		t.Error("the robot was asked what could not be sent")
	}
}

// TestSeveralAtOnce: requests made at once are each answered by their
// own.
func TestSeveralAtOnce(t *testing.T) {
	t.Parallel()

	_, _, robot := served(t)

	var asking sync.WaitGroup

	for index := range 24 {
		asking.Go(func() {
			name := []string{"quantity", "status", "wind"}[index%3]
			want := []string{"100", "4", "2"}[index%3]

			properties, err := robot.Properties(t.Context(), name)
			if err != nil || len(properties) != 1 || string(properties[name]) != want {
				t.Errorf("Properties(%s) = %v, %v; want %s", name, properties, err, want)
			}
		})
	}

	asking.Wait()
}

// TestNotOpened: what is no session and no robot is said to be none,
// before anything is sent where it could be: a broker not under TLS, an
// address that does not read, credentials with a part missing, a robot
// of another protocol or family, an id that is no id, and a robot open
// already. The broker's refusals are told apart: of the credentials, and
// of a robot that is not the account's.
func TestNotOpened(t *testing.T) {
	t.Parallel()

	cloud, session, robot := served(t)

	for name, attempt := range map[string]struct {
		change func(*roborock.Cloud)
		want   error
	}{
		"in clear":           {func(c *roborock.Cloud) { c.Broker = strings.Replace(c.Broker, "ssl", "tcp", 1) }, roborock.ErrArgument},
		"no scheme":          {func(c *roborock.Cloud) { c.Broker = strings.TrimPrefix(c.Broker, "ssl://") }, roborock.ErrArgument},
		"an address unread":  {func(c *roborock.Cloud) { c.Broker = "ssl://%zz" }, roborock.ErrArgument},
		"no secret":          {func(c *roborock.Cloud) { c.Secret = "" }, roborock.ErrArgument},
		"another key":        {func(c *roborock.Cloud) { c.Key = "another" }, roborock.ErrCredentials},
		"another user":       {func(c *roborock.Cloud) { c.User = "another" }, roborock.ErrCredentials},
		"nobody listening":   {func(c *roborock.Cloud) { c.Broker = "ssl://127.0.0.1:1" }, roborock.ErrLost},
		"a broker by a name": {func(c *roborock.Cloud) { c.Broker = strings.Replace(c.Broker, "127.0.0.1", "localhost", 1) }, roborock.ErrLost},
		"the scheme of MQTT": {func(c *roborock.Cloud) { c.Broker = strings.Replace(c.Broker, "ssl", "mqtts", 1) }, nil},
		"the account, as is": {func(*roborock.Cloud) {}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			credentials := cloud.Cloud()
			attempt.change(&credentials)

			opened, err := roborock.Connect(t.Context(), credentials, roborock.Options{TLS: cloud.TLS()})
			if opened != nil {
				_ = opened.Close()
			}

			if !errors.Is(err, attempt.want) || (attempt.want == nil) != (opened != nil) {
				t.Errorf("Connect = %v, want %v", err, attempt.want)
			}

			if err != nil &&
				(strings.Contains(err.Error(), roborocktest.Secret) || strings.Contains(err.Error(), roborocktest.Key)) {
				t.Errorf("Connect says a secret: %v", err)
			}
		})
	}

	// The broker's certificate is the test's own: the system does not
	// trust it.
	if _, err := roborock.Connect(t.Context(), cloud.Cloud(), roborock.Options{}); !errors.Is(err, roborock.ErrLost) {
		t.Errorf("Connect to a broker the system does not trust = %v, want ErrLost", err)
	}

	for name, attempt := range map[string]struct {
		change func(*roborock.Identity)
		want   error
	}{
		"another protocol":  {func(i *roborock.Identity) { i.Protocol = "1.0" }, roborock.ErrUnsupported},
		"the other family":  {func(i *roborock.Identity) { i.Model = "roborock.vacuum.ss07" }, roborock.ErrUnsupported},
		"no model":          {func(i *roborock.Identity) { i.Model = "" }, roborock.ErrUnsupported},
		"an id with a path": {func(i *roborock.Identity) { i.ID = "a/b" }, roborock.ErrArgument},
		"an id of a filter": {func(i *roborock.Identity) { i.ID = "#" }, roborock.ErrArgument},
		"no id":             {func(i *roborock.Identity) { i.ID = "" }, roborock.ErrArgument},
		"open already":      {func(*roborock.Identity) {}, roborock.ErrArgument},
		"not the account's": {func(i *roborock.Identity) { i.ID = "another-robot" }, roborock.ErrRefused},
	} {
		identity := cloud.Identity()
		attempt.change(&identity)

		if _, err := session.Robot(t.Context(), identity); !errors.Is(err, attempt.want) {
			t.Errorf("Robot %s = %v, want %v", name, err, attempt.want)
		}
	}

	// A robot the broker refused can be asked for again.
	another := cloud.Identity()
	another.ID = "another-robot"

	if _, err := session.Robot(t.Context(), another); !errors.Is(err, roborock.ErrRefused) {
		t.Errorf("Robot refused, asked again = %v, want ErrRefused again", err)
	}

	if _, err := robot.State(t.Context()); err != nil {
		t.Errorf("State after the refusals = %v", err)
	}
}

// TestRefusedByTheBroker: a broker that refuses the session for the
// account's credentials is told apart from one that refuses it for
// anything else.
func TestRefusedByTheBroker(t *testing.T) {
	t.Parallel()

	for reason, want := range map[byte]error{
		134: roborock.ErrCredentials,
		135: roborock.ErrCredentials,
		159: roborock.ErrRefused,
	} {
		cloud, err := roborocktest.New()
		if err != nil {
			t.Fatal(err)
		}

		cloud.Set(func(c *roborocktest.Cloud) { c.Reason = reason })

		_, err = roborock.Connect(t.Context(), cloud.Cloud(), roborock.Options{TLS: cloud.TLS()})

		if !errors.Is(err, want) || !strings.Contains(err.Error(), strconv.Itoa(int(reason))) {
			t.Errorf("Connect refused for %d = %v, want %v with the reason", reason, err, want)
		}

		cloud.Close()
	}
}

// TestEnded: a session cut is lost, and says so to what waits on it and
// to what asks after; a session closed is closed.
func TestEnded(t *testing.T) {
	t.Parallel()

	t.Run("cut", func(t *testing.T) {
		t.Parallel()

		cloud, session, robot := served(t)
		cloud.Set(func(c *roborocktest.Cloud) { c.Silent = true })

		waited := make(chan error, 1)

		go func() {
			_, err := robot.State(t.Context())
			waited <- err
		}()

		// Cut once the robot has been asked.
		for len(cloud.Calls()) == 0 {
			time.Sleep(time.Millisecond)
		}

		cloud.Drop()

		if err := <-waited; !errors.Is(err, roborock.ErrLost) {
			t.Errorf("State on a session cut = %v, want ErrLost", err)
		}

		<-session.Done()

		if !errors.Is(session.Err(), roborock.ErrLost) {
			t.Errorf("Err = %v, want ErrLost", session.Err())
		}

		if err := robot.Start(t.Context()); !errors.Is(err, roborock.ErrLost) {
			t.Errorf("Start on a session lost = %v, want ErrLost", err)
		}

		if _, err := session.Robot(t.Context(), roborock.Identity{
			ID: "later", Protocol: "B01", Model: roborocktest.Model, Key: roborocktest.RobotKey,
		}); !errors.Is(err, roborock.ErrLost) {
			t.Errorf("Robot on a session lost = %v, want ErrLost", err)
		}
	})

	t.Run("closed", func(t *testing.T) {
		t.Parallel()

		_, session, robot := served(t)

		if session.Err() != nil {
			t.Errorf("Err of a session that lasts = %v", session.Err())
		}

		if err := session.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}

		<-session.Done()

		if !errors.Is(session.Err(), roborock.ErrClosed) {
			t.Errorf("Err = %v, want ErrClosed", session.Err())
		}

		if _, err := robot.State(t.Context()); !errors.Is(err, roborock.ErrClosed) {
			t.Errorf("State on a session closed = %v, want ErrClosed", err)
		}
	})

	t.Run("a key that seals nothing", func(t *testing.T) {
		t.Parallel()

		cloud, _, _ := served(t)

		other, err := roborock.Connect(t.Context(), cloud.Cloud(), roborock.Options{TLS: cloud.TLS()})
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = other.Close() }()

		short := cloud.Identity()
		short.Key = "short"

		keyless, err := other.Robot(t.Context(), short)
		if err != nil {
			t.Fatalf("Robot: %v", err)
		}

		if _, err = keyless.State(
			t.Context(),
		); !errors.Is(err, roborock.ErrArgument) ||
			strings.Contains(err.Error(), "short") {
			t.Errorf("State under a key of five bytes = %v, want ErrArgument and not the key", err)
		}

		// Another key of the right size opens nothing the robot says.
		wrong := cloud.Identity()
		wrong.Key = "another-key-0123"

		third, err := roborock.Connect(t.Context(), cloud.Cloud(), roborock.Options{TLS: cloud.TLS()})
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = third.Close() }()

		deaf, err := third.Robot(t.Context(), wrong)
		if err != nil {
			t.Fatalf("Robot: %v", err)
		}

		if _, err = deaf.State(brief(t)); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("State under another key = %v, want no answer", err)
		}
	})
}

// TestSecrets: a robot's key and an account's secrets are in no value
// formatted.
func TestSecrets(t *testing.T) {
	t.Parallel()

	cloud, _, robot := served(t)

	shown := fmt.Sprintf("%v %+v %#v %s", cloud.Cloud(), robot.Identity(), cloud.Identity(), cloud.Cloud())

	raw, err := json.Marshal(map[string]any{"cloud": cloud.Cloud(), "identity": cloud.Identity()})
	if err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{roborocktest.Secret, roborocktest.Key, roborocktest.RobotKey, roborocktest.User} {
		if strings.Contains(shown+string(raw), secret) {
			t.Errorf("%q is shown", secret)
		}
	}
}
