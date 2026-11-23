package roborock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/duncanite/go-roborock/internal/wire"
)

// The protocol and the family of models this package speaks to.
const (
	protocolB01 = "B01"
	// family is what the last part of a model's name holds for the Q7s:
	// "roborock.vacuum.sc01".
	family = "sc"
)

// Speaks says a device is one this package speaks to: of the protocol
// B01, and of the Q7 family, whose messages are these. The other family
// of that protocol, the Q10s, speaks another way, and the devices of the
// other protocols are not reached through the cloud alone.
func Speaks(identity Identity) bool {
	model := identity.Model[strings.LastIndex(identity.Model, ".")+1:]

	return identity.Protocol == protocolB01 && strings.Contains(model, family)
}

// RefusedError is a robot's refusal of what was asked, with the code it
// gave. Roborock publishes no table of them.
type RefusedError struct {
	// Method is what was asked.
	Method string
	// Code is the robot's.
	Code int
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%s: by the robot, %s, code %d", ErrRefused.Error(), e.Method, e.Code)
}

// Is makes a RefusedError an ErrRefused.
func (*RefusedError) Is(target error) bool { return target == ErrRefused }

// Robot is a robot of the Q7 family, spoken to through the cloud. Its
// methods may be called from several goroutines; each waits for the
// robot's answer, within its context: a robot that is off, or out of its
// network's reach, answers nothing.
type Robot struct {
	session  *Session
	identity Identity
	asking   string
	hearing  string

	mu      sync.Mutex
	waiting map[string]chan answer
	last    uint64
}

// answer is what a robot answered a request with.
type answer struct {
	Code *int            `json:"code"`
	Data json.RawMessage `json:"data"`
}

// request is what is asked of a robot.
type request struct {
	Method string `json:"method"`
	ID     string `json:"msgId"`
	Params any    `json:"params"`
}

// The data point a request is written under.
const asked = "10000"

// The numbers a frame and a request carry are drawn within these, as the
// open implementations draw them.
const (
	leastSeq    = 100_000
	seqs        = 900_000
	leastRandom = 10_000
	randoms     = 90_000
	leastID     = 100_000_000_000
	ids         = 900_000_000_000
)

// drawn is a number drawn at random from least on, among count.
func drawn(least, count int64) int64 {
	number, err := rand.Int(rand.Reader, big.NewInt(count))
	if err != nil {
		// The system's source of random numbers does not fail.
		panic(err)
	}

	return least + number.Int64()
}

// Call asks the robot for a method, with its parameters, and returns the
// data of its answer: what Roborock's app asks, by its name. A robot that
// answers with a code other than zero refused: *RefusedError.
func (r *Robot) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "" {
		return nil, fmt.Errorf("%w: no method", ErrArgument)
	}

	if params == nil {
		// A method with no parameter is asked with an empty object.
		params = struct{}{}
	}

	answered := make(chan answer, 1)

	r.mu.Lock()

	if r.last == 0 {
		r.last = uint64(drawn(leastID, ids)) // #nosec G115 -- drawn above zero
	}

	// Twelve digits, as text.
	r.last = leastID + (r.last-leastID+1)%ids
	number := strconv.FormatUint(r.last, 10)
	r.waiting[number] = answered
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.waiting, number)
		r.mu.Unlock()
	}()

	content, err := json.Marshal(map[string]map[string]request{
		"dps": {asked: {Method: method, ID: number, Params: params}},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: the parameters of %s: %w", ErrArgument, method, err)
	}

	frame := wire.Frame{
		Seq:       uint32(drawn(leastSeq, seqs)),       // #nosec G115 -- within six digits
		Random:    uint32(drawn(leastRandom, randoms)), // #nosec G115 -- within five digits
		Timestamp: uint32(time.Now().Unix()),           // #nosec G115 -- the protocol's own four bytes
		Protocol:  wire.Request,
	}

	// A request to a Q7 is padded once before it is sealed, and once more
	// as it is: the open implementations do, and so does this.
	if frame.Payload, err = wire.Seal(r.identity.Key, frame.Random, wire.Pad(content)); err != nil {
		return nil, fmt.Errorf("%w: the robot's key: %w", ErrArgument, err)
	}

	message, err := frame.Encode()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrArgument, method, err)
	}

	if err = r.session.broker.Publish(ctx, r.asking, message); err != nil {
		return nil, said(err)
	}

	select {
	case got := <-answered:
		if got.Code != nil && *got.Code != 0 {
			return nil, &RefusedError{Method: method, Code: *got.Code}
		}

		return got.Data, nil
	case <-r.session.Done():
		return nil, r.session.Err()
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the robot to answer %s: %w", method, context.Cause(ctx))
	}
}

// Identity is the robot's.
func (r *Robot) Identity() Identity { return r.identity }

// Properties reads properties of the robot by the names its protocol
// gives them, and returns each as the robot wrote it. A name the robot
// has not is left out of what it answers.
func (r *Robot) Properties(ctx context.Context, names ...string) (map[string]json.RawMessage, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: no property named", ErrArgument)
	}

	data, err := r.Call(ctx, "prop.get", map[string][]string{"property": names})
	if err != nil {
		return nil, err
	}

	var properties map[string]json.RawMessage

	if json.Unmarshal(data, &properties) != nil || properties == nil {
		return nil, fmt.Errorf("%w: prop.get gave no properties", ErrAnswer)
	}

	return properties, nil
}

// clean is what a whole-home clean is driven with.
type clean struct {
	Kind    int   `json:"clean_type"` //nolint:tagliatelle // the protocol's own key
	Control int   `json:"ctrl_value"` //nolint:tagliatelle // the protocol's own key
	Rooms   []int `json:"room_ids"`   //nolint:tagliatelle // the protocol's own key
}

// What a clean is told.
const (
	controlStop  = 0
	controlStart = 1
	controlPause = 2
)

// Start has the robot clean the whole home. Each of these returns when
// the robot has taken the request; what it then does is read with State.
//
// The protocol has no resume. Whether Start goes on with a clean that
// was paused, or begins another, is not known.
func (r *Robot) Start(ctx context.Context) error { return r.drive(ctx, controlStart) }

// Pause has the robot pause its clean.
func (r *Robot) Pause(ctx context.Context) error { return r.drive(ctx, controlPause) }

// Stop has the robot end its clean, where it is.
func (r *Robot) Stop(ctx context.Context) error { return r.drive(ctx, controlStop) }

// Dock sends the robot back to its dock.
func (r *Robot) Dock(ctx context.Context) error {
	_, err := r.Call(ctx, "service.start_recharge", nil)

	return err
}

// Locate has the robot say where it is, aloud.
func (r *Robot) Locate(ctx context.Context) error {
	_, err := r.Call(ctx, "service.find_device", nil)

	return err
}

func (r *Robot) drive(ctx context.Context, control int) error {
	_, err := r.Call(ctx, "service.set_room_clean", clean{Control: control, Rooms: []int{}})

	return err
}

// heard takes one message of the cloud's from the robot: its frames, each
// opened under the robot's key, and in each the answers to what was
// asked. What does not open, what is no answer, a map among them, and
// what answers nothing asked here is passed over: the topic carries all
// the robot says, to every session of the account.
func (r *Robot) heard(message []byte) {
	// The frames read before one that does not are still taken.
	frames, _ := wire.Decode(message)

	for _, frame := range frames {
		content, err := wire.Open(r.identity.Key, frame.Random, frame.Payload)
		if err != nil {
			continue
		}

		// Padded twice by some, once by others.
		content, _ = wire.Unpad(content)

		var said struct {
			Points map[string]json.RawMessage `json:"dps"`
		}

		if json.Unmarshal(content, &said) != nil {
			continue
		}

		for _, point := range said.Points {
			r.answered(point)
		}
	}
}

// answered takes the value of one data point: an answer when it is, or
// holds as text, an object with the number of a request made here.
func (r *Robot) answered(point json.RawMessage) {
	var text string

	if json.Unmarshal(point, &text) == nil {
		point = json.RawMessage(text)
	}

	var got struct {
		answer

		ID json.RawMessage `json:"msgId"`
	}

	decoder := json.NewDecoder(bytes.NewReader(point))
	if decoder.Decode(&got) != nil || got.ID == nil {
		return
	}

	// The number, written as text or as a number.
	number := strings.Trim(string(got.ID), `"`)

	r.mu.Lock()
	waiter, waited := r.waiting[number]
	r.mu.Unlock()

	if waited {
		// One answer is taken; a second for the same number is dropped.
		select {
		case waiter <- got.answer:
		default:
		}
	}
}
