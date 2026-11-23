package wire_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/duncanite/go-roborock/internal/wire"
)

// The key of python-roborock's own test device.
const key = "key123key123key1"

// unhex is the bytes a text of hexadecimal stands for, its line ends
// left out.
func unhex(t *testing.T, text string) []byte {
	t.Helper()

	raw, err := hex.DecodeString(strings.Join(strings.Fields(text), ""))
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// The messages of python-roborock's end to end test of a Q7, byte for
// byte: two requests as that project sends them, each padded twice, and
// two answers as its test broker builds them, padded once.
type vector struct {
	name    string
	frame   wire.Frame
	plain   string
	twice   bool
	message string
}

func vectors() []vector {
	return []vector{
		{
			name:  "a request for the status",
			frame: wire.Frame{Seq: 9091, Random: 9092, Timestamp: 0x68a6a225, Protocol: wire.Request},
			plain: `{"dps": {"10000": {"method": "prop.get", "msgId": "9090", "params": {"property": ["status"]}}}}`,
			twice: true,
			message: `
				423031000023830000238468a6a225006500709f245bc1482bc907cac3e1c501
				063e6244d88d7c4519475c5387fe1aa7a50db4a8b57e19758a4f0a37cad01fd0
				a15be8ef457573aadd84c8ecd6c2e76443c3588a317ac0450a5f06b64fa3e173
				0558b4712bc3cfe5688adbdea23f1af78e6daba47f7134c29383017dcd1eb378
				c1d7dc0c71b286`,
		},
		{
			name:  "a request to start cleaning",
			frame: wire.Frame{Seq: 9094, Random: 9095, Timestamp: 0x68a6a226, Protocol: wire.Request},
			plain: `{"dps": {"10000": {"method": "service.set_room_clean", "msgId": "9093", ` +
				`"params": {"clean_type": 0, "ctrl_value": 1, "room_ids": []}}}}`,
			twice: true,
			message: `
				423031000023860000238768a6a226006500a04daedb7caddb6f8e4a1b014c2b
				fdfd1b4fdf4c64fb3beda6fc9fe221e89594496c57799cc58e3548fccc29f869
				9b54fb42337e6372a6170f87203174c3bb295b6af3a723bd1042844b6f09a56c
				0b3cd00ca4ba90be7027437335bd5f47bd1bb4e50b9850ed61807ddb40c1ad99
				65e17edfa6b87def3b0892c395c746a6f732b66dcb2172b8a1ee8549d62a7633
				c001c3befa583dfaf2725084e41768e5210098169c6243`,
		},
		{
			name:  "an answer with the status",
			frame: wire.Frame{Seq: 1, Random: 23, Timestamp: 0x68a6a223, Protocol: wire.Response},
			plain: `{"dps": {"10000": "{\"msgId\": \"9090\", \"data\": {\"status\": 2}}"}}`,
			message: `
				423031000000010000001768a6a22300660050ccc94f81fdb04c466dd7bbaa87
				d8e58454b75b5822d3d153d01db86f11534f7721a1a58b057f9eb76288df571b
				fe50f09a70bce1adc3f7ccf73fe46add1df5d24a6d4d484fb57507707dbfc5b9
				3fe7734bd919cd`,
		},
		{
			name:  "an answer that it started",
			frame: wire.Frame{Seq: 2, Random: 23, Timestamp: 0x68a6a224, Protocol: wire.Response},
			plain: `{"dps": {"10000": "{\"msgId\": \"9093\", \"data\": \"ok\"}"}}`,
			message: `
				423031000000020000001768a6a22400660040ccc94f81fdb04c466dd7bbaa87
				d8e58454b75b5822d3d153d01db86f11534f772befa8bfd266058ece7f0876c0
				1890ed04668e8f913063d2e08c1a095c7cea94e3241560`,
		},
	}
}

// TestVectors: a frame sealed and encoded is python-roborock's message,
// byte for byte, and that message decoded and opened is the frame and
// its content.
func TestVectors(t *testing.T) {
	t.Parallel()

	for _, vector := range vectors() {
		t.Run(vector.name, func(t *testing.T) {
			t.Parallel()

			message := unhex(t, vector.message)

			content := []byte(vector.plain)
			if vector.twice {
				content = wire.Pad(content)
			}

			sealed, err := wire.Seal(key, vector.frame.Random, content)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}

			frame := vector.frame
			frame.Payload = sealed

			encoded, err := frame.Encode()
			if err != nil || !bytes.Equal(encoded, message) {
				t.Fatalf("Encode = %x, %v\nwant     %x", encoded, err, message)
			}

			frames, err := wire.Decode(message)
			if err != nil || len(frames) != 1 {
				t.Fatalf("Decode = %d frames, %v", len(frames), err)
			}

			got := frames[0]
			if got.Seq != frame.Seq || got.Random != frame.Random || got.Timestamp != frame.Timestamp ||
				got.Protocol != frame.Protocol {
				t.Errorf("Decode = %+v, want the fields of %+v", got, vector.frame)
			}

			opened, err := wire.Open(key, got.Random, got.Payload)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			if vector.twice {
				var padded bool

				if opened, padded = wire.Unpad(opened); !padded {
					t.Fatal("the content is not padded a second time")
				}
			}

			if string(opened) != vector.plain {
				t.Errorf("Open = %q, want %q", opened, vector.plain)
			}
		})
	}
}

// TestDecode: a message is frames end to end. Several are each given; a
// frame of no length is one with no payload and no checksum; what is cut,
// what begins no frame and what does not match its checksum are told
// apart, with the frames before.
func TestDecode(t *testing.T) {
	t.Parallel()

	first, second := unhex(t, vectors()[2].message), unhex(t, vectors()[3].message)

	empty := append([]byte("B01"), make([]byte, 16)...)
	empty[6] = 7

	frames, err := wire.Decode(append(append(bytes.Clone(first), empty...), second...))
	if err != nil || len(frames) != 3 || frames[0].Seq != 1 || frames[1].Seq != 7 || frames[1].Payload != nil ||
		frames[2].Seq != 2 {
		t.Fatalf("Decode of three frames = %+v, %v", frames, err)
	}

	// A frame is its own: the message it came in may be written over.
	message := bytes.Clone(second)

	own, err := wire.Decode(message)
	if err != nil || len(own) != 1 {
		t.Fatalf("Decode = %d frames, %v", len(own), err)
	}

	kept := bytes.Clone(own[0].Payload)

	clear(message)

	if !bytes.Equal(own[0].Payload, kept) {
		t.Error("a frame's payload changed with the message it was read from")
	}

	if frames, err = wire.Decode(nil); err != nil || len(frames) != 0 {
		t.Errorf("Decode of nothing = %d frames, %v", len(frames), err)
	}

	flipped := bytes.Clone(second)
	flipped[30] ^= 1

	for name, broken := range map[string]struct {
		message []byte
		want    error
	}{
		"cut in its payload":   {second[:len(second)-5], wire.ErrFrame},
		"cut in its fields":    {second[:10], wire.ErrFrame},
		"of another version":   {append([]byte("1.0"), second[3:]...), wire.ErrFrame},
		"something before it":  {append([]byte{0, 0, 0, 0x67}, second...), wire.ErrFrame},
		"a byte of it changed": {flipped, wire.ErrChecksum},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			frames, err := wire.Decode(append(bytes.Clone(first), broken.message...))

			if !errors.Is(err, broken.want) || len(frames) != 1 || frames[0].Seq != 1 {
				t.Errorf("Decode = %d frames, %v; want the first frame and %v", len(frames), err, broken.want)
			}
		})
	}
}

// TestEncode: a frame with no payload, or with more than a length says,
// is not encoded.
func TestEncode(t *testing.T) {
	t.Parallel()

	for name, payload := range map[string][]byte{
		"none":     nil,
		"too long": make([]byte, wire.LargestPayload+1),
	} {
		if _, err := (wire.Frame{Payload: payload}).Encode(); !errors.Is(err, wire.ErrFrame) {
			t.Errorf("Encode of a frame with a payload %s = %v, want ErrFrame", name, err)
		}
	}

	whole, err := (wire.Frame{Payload: make([]byte, wire.LargestPayload)}).Encode()
	if err != nil {
		t.Fatalf("Encode of the longest payload: %v", err)
	}

	if frames, decodeErr := wire.Decode(whole); decodeErr != nil || len(frames) != 1 ||
		len(frames[0].Payload) != wire.LargestPayload {
		t.Errorf("Decode of the longest payload = %d frames, %v", len(frames), decodeErr)
	}
}

// TestSealing: a payload opens under its key and its frame's number; a
// key is sixteen bytes; and what does not open is not said.
func TestSealing(t *testing.T) {
	t.Parallel()

	const said = "what the device said, which is long enough for three blocks"

	sealed, err := wire.Seal(key, 77, []byte(said))
	if err != nil || len(sealed) != 64 {
		t.Fatalf("Seal = %d bytes, %v", len(sealed), err)
	}

	if opened, openErr := wire.Open(key, 77, sealed); openErr != nil || string(opened) != said {
		t.Errorf("Open = %q, %v", opened, openErr)
	}

	// Whole blocks are padded with a whole block.
	if whole, sealErr := wire.Seal(key, 77, make([]byte, 32)); sealErr != nil || len(whole) != 48 {
		t.Errorf("Seal of two blocks = %d bytes, %v; want three blocks", len(whole), sealErr)
	}

	for name, attempt := range map[string]struct {
		key    string
		random uint32
		sealed []byte
		want   error
	}{
		"another key": {"key123key123key2", 77, sealed, wire.ErrSealed},
		"cut":         {key, 77, sealed[:20], wire.ErrSealed},
		"nothing":     {key, 77, nil, wire.ErrSealed},
		"a short key": {"key123", 77, sealed, wire.ErrKey},
		"a long key":  {key + key, 77, sealed, wire.ErrKey},
	} {
		opened, openErr := wire.Open(attempt.key, attempt.random, attempt.sealed)

		if !errors.Is(openErr, attempt.want) || opened != nil {
			t.Errorf("Open under %s = %q, %v; want %v", name, opened, openErr, attempt.want)
		}

		if openErr != nil &&
			(strings.Contains(openErr.Error(), "device said") || strings.Contains(openErr.Error(), "key123")) {
			t.Errorf("Open under %s says %q", name, openErr)
		}
	}

	// Nothing authenticates a payload: under its key and another frame's
	// number it opens, its first block another.
	if other, openErr := wire.Open(key, 78, sealed); openErr != nil || string(other) == said ||
		string(other[16:]) != said[16:] {
		t.Errorf("Open under another number = %q, %v", other, openErr)
	}

	if _, err = wire.Seal("key123", 77, []byte(said)); !errors.Is(err, wire.ErrKey) {
		t.Errorf("Seal under a short key = %v, want ErrKey", err)
	}
}

// TestPadding: a content unpadded is the content, and what does not end
// in a padding is given back as it is.
func TestPadding(t *testing.T) {
	t.Parallel()

	for _, length := range []int{0, 1, 15, 16, 17, 31, 32} {
		content := bytes.Repeat([]byte{0x41}, length)
		padded := wire.Pad(content)

		if len(padded)%16 != 0 || len(padded) <= length {
			t.Errorf("Pad of %d bytes = %d bytes", length, len(padded))
		}

		if back, was := wire.Unpad(padded); !was || !bytes.Equal(back, content) {
			t.Errorf("Unpad of %d bytes padded = %d bytes, %t", length, len(back), was)
		}
	}

	for name, content := range map[string][]byte{
		"nothing":            nil,
		"not whole blocks":   bytes.Repeat([]byte{4}, 20),
		"ending in zero":     make([]byte, 16),
		"ending above":       bytes.Repeat([]byte{17}, 32),
		"ending in a mix":    append(bytes.Repeat([]byte{0x41}, 13), 1, 2, 3),
		"a JSON of 16 bytes": []byte(`{"a":"bcdefghi"}`),
	} {
		if back, was := wire.Unpad(content); was || !bytes.Equal(back, content) {
			t.Errorf("Unpad of %s = %d bytes, %t; want it back as it is", name, len(back), was)
		}
	}
}
