// Package roborock holds what reaches a Roborock device: an account's way
// into Roborock's cloud, and what names one device and opens its
// messages. The account package gives both, from a login.
//
// Nothing is written anywhere: where they are kept is the program's
// business.
package roborock

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// Cloud is an account's way into Roborock's cloud, where a device that
// speaks nothing on the LAN is reached. It is a secret: formatted, logged
// or marshalled as JSON it shows its broker and nothing else. One kept in
// an unexported field of another struct is out of reach of these methods,
// and fmt prints it as it is.
type Cloud struct {
	// Broker is the URL of the MQTT broker the account's devices are
	// reached through, as the account gives it: "ssl://host:port".
	Broker string
	// User names the account to the cloud; it is in the topics too.
	User string
	// Secret and Key are what the broker's password is made of.
	Secret string
	Key    string
}

// Format keeps the secrets out of a formatted value, whatever the verb.
func (c Cloud) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, c.shown()) }

// LogValue keeps them out of a log record.
func (c Cloud) LogValue() slog.Value { return slog.StringValue(c.shown()) }

// MarshalJSON keeps them out of JSON: a Cloud marshals as what it shows,
// a string.
func (c Cloud) MarshalJSON() ([]byte, error) { return marshalled(c.shown()) }

func (c Cloud) shown() string { return fmt.Sprintf("roborock.Cloud{Broker:%q}", c.Broker) }

// Identity is what names a device of an account and opens its messages.
// The key is a secret: an Identity formatted, logged or marshalled as
// JSON shows the rest, and not that. One kept in an unexported field of
// another struct is out of reach of these methods, and fmt prints it as
// it is.
type Identity struct {
	// ID is what the account names the device by.
	ID string
	// Protocol is the version of the protocol the device speaks, as the
	// account writes it: "1.0", "A01", "B01".
	Protocol string
	// Model is what the device is, as Roborock names its products:
	// "roborock.vacuum.sc01". Empty when the account does not say.
	Model string
	// Key is the key the device's messages are encrypted under.
	Key string
}

// Format keeps the key out of a formatted value, whatever the verb.
func (i Identity) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, i.shown()) }

// LogValue keeps it out of a log record.
func (i Identity) LogValue() slog.Value { return slog.StringValue(i.shown()) }

// MarshalJSON keeps it out of JSON: an Identity marshals as what it
// shows, a string.
func (i Identity) MarshalJSON() ([]byte, error) { return marshalled(i.shown()) }

func (i Identity) shown() string {
	return fmt.Sprintf("roborock.Identity{ID:%q Protocol:%q Model:%q}", i.ID, i.Protocol, i.Model)
}

func marshalled(shown string) ([]byte, error) {
	raw, err := json.Marshal(shown)
	if err != nil {
		return nil, fmt.Errorf("marshalling what a secret shows: %w", err)
	}

	return raw, nil
}
