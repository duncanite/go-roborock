package mqtt_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncanite/go-roborock/internal/mqtt"
)

// broker is the other end of a session's connection, driven by the test.
type broker struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
}

// dialed is a connection for a session, and the broker at its other end.
func dialed(t *testing.T) (net.Conn, *broker) {
	t.Helper()

	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = listener.Close() }()

	client, err := new(net.Dialer).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = server.Close() })
	t.Cleanup(func() { _ = client.Close() })

	return client, &broker{t: t, conn: server, reader: bufio.NewReader(server)}
}

func unhex(t *testing.T, text string) []byte {
	t.Helper()

	raw, err := hex.DecodeString(strings.Join(strings.Fields(text), ""))
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// expect reads what the session sends next, which is to be these bytes.
func (b *broker) expect(what string, want []byte) {
	b.t.Helper()

	_ = b.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	got := make([]byte, len(want))
	if _, err := io.ReadFull(b.reader, got); err != nil {
		b.t.Fatalf("%s: reading %d bytes: %v", what, len(want), err)
	}

	if !bytes.Equal(got, want) {
		b.t.Fatalf("%s:\n got %x\nwant %x", what, got, want)
	}
}

func (b *broker) send(packet []byte) {
	b.t.Helper()

	if _, err := b.conn.Write(packet); err != nil {
		b.t.Fatalf("the broker writing: %v", err)
	}
}

// The packets of python-roborock's end to end test of a Q7, as its MQTT
// client, paho, wrote them and as its test broker answered.
const (
	connect   = `10 29 00 04 4d 51 54 54 05 c2 00 2d 00 00 00 00 08 31 39 36 34 38 66 39 34 00 10 32 33 34 36 37 38 65 61 38 35 34 66 31 39 39 65`
	connack   = `20 09 02 00 06 22 00 0a 21 00 14`
	subscribe = `82 2a 00 01 00 00 24 72 72 2f 6d 2f 6f 2f 75 73 65 72 31 32 33 2f 31 39 36 34 38 66 39 34 2f 64 65 76 69 63 65 2d 69 64 2d 71 37 00`
	suback    = `90 04 00 01 00 00`
	accepted  = `20 03 00 00 00`

	user     = "19648f94"
	password = "234678ea854f199e"
	inbound  = "rr/m/o/user123/19648f94/device-id-q7"
	outbound = "rr/m/i/user123/19648f94/device-id-q7"
)

// opened is a session the broker has taken, with nothing said after.
func opened(t *testing.T, opts mqtt.Options) (*mqtt.Session, *broker) {
	t.Helper()

	conn, peer := dialed(t)

	var taken sync.WaitGroup

	taken.Go(func() {
		_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

		// The CONNECT is read past, whatever its length.
		if _, err := peer.reader.ReadByte(); err != nil {
			return
		}

		length, _ := peer.reader.ReadByte()
		_, _ = io.CopyN(io.Discard, peer.reader, int64(length))
		_, _ = peer.conn.Write(unhexed(accepted))
	})

	session, err := mqtt.Connect(t.Context(), conn, opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	taken.Wait()
	t.Cleanup(func() { _ = session.Close() })

	return session, peer
}

func unhexed(text string) []byte {
	raw, _ := hex.DecodeString(strings.Join(strings.Fields(text), ""))

	return raw
}

// TestTheReferenceExchange: a session is opened, subscribed and published
// on in the bytes paho writes for the same, and reads the bytes that
// project's broker answers with: an acknowledgement with properties and
// flags no broker should set, a subscription taken, a message.
func TestTheReferenceExchange(t *testing.T) {
	t.Parallel()

	conn, peer := dialed(t)

	// What that project published, a request for a robot's status, and
	// what its broker then sent: an answer, with four bytes before it
	// that its test's encoder adds.
	payload := unhex(t, `
		423031000023830000238468a6a225006500709f245bc1482bc907cac3e1c501
		063e6244d88d7c4519475c5387fe1aa7a50db4a8b57e19758a4f0a37cad01fd0
		a15be8ef457573aadd84c8ecd6c2e76443c3588a317ac0450a5f06b64fa3e173
		0558b4712bc3cfe5688adbdea23f1af78e6daba47f7134c29383017dcd1eb378
		c1d7dc0c71b286`)
	answer := unhex(t, `
		00000067423031000000010000001768a6a22300660050ccc94f81fdb04c466d
		d7bbaa87d8e58454b75b5822d3d153d01db86f11534f7721a1a58b057f9eb762
		88df571bfe50f09a70bce1adc3f7ccf73fe46add1df5d24a6d4d484fb5750770
		7dbfc5b93fe7734bd919cd`)

	publish := append(unhex(t, `30 ae 01 00 24`), outbound...)
	publish = append(append(publish, 0), payload...)

	received := append(unhex(t, `30 92 01 00 24`), inbound...)
	received = append(append(received, 0), answer...)

	var served sync.WaitGroup

	served.Go(func() {
		peer.expect("the CONNECT", unhex(t, connect))
		peer.send(unhex(t, connack))
		peer.expect("the SUBSCRIBE", unhex(t, subscribe))
		peer.send(unhex(t, suback))
		peer.expect("the PUBLISH", publish)
		peer.send(received)
		peer.expect("the DISCONNECT", []byte{0xE0, 0})
	})

	session, err := mqtt.Connect(t.Context(), conn, mqtt.Options{User: user, Password: password})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if err = session.Subscribe(t.Context(), inbound); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err = session.Publish(t.Context(), outbound, payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	message, err := session.Next(t.Context())
	if err != nil || message.Topic != inbound || !bytes.Equal(message.Payload, answer) {
		t.Errorf("Next = %q with %x, %v", message.Topic, message.Payload, err)
	}

	if err = session.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	served.Wait()

	if !errors.Is(session.Err(), mqtt.ErrClosed) {
		t.Errorf("Err after Close = %v, want ErrClosed", session.Err())
	}

	select {
	case <-session.Done():
	default:
		t.Error("Done is open after Close")
	}

	if err = session.Publish(t.Context(), outbound, payload); !errors.Is(err, mqtt.ErrClosed) {
		t.Errorf("Publish after Close = %v, want ErrClosed", err)
	}
}

// TestRefused: a session the broker refuses is not one, and says the
// reason; so does a subscription; and a broker that ends a session it
// had taken ends it with its reason.
func TestRefused(t *testing.T) {
	t.Parallel()

	t.Run("the session", func(t *testing.T) {
		t.Parallel()

		conn, peer := dialed(t)

		go func() {
			_, _ = io.CopyN(io.Discard, peer.reader, int64(len(unhexed(connect))))
			_, _ = peer.conn.Write([]byte{0x20, 3, 0, 135, 0})
		}()

		_, err := mqtt.Connect(t.Context(), conn, mqtt.Options{User: user, Password: password})

		var refused *mqtt.RefusedError
		if !errors.As(err, &refused) || refused.Reason != 135 || refused.What != "session" ||
			!errors.Is(err, mqtt.ErrRefused) {
			t.Errorf("Connect = %v, want a refusal of the session for reason 135", err)
		}

		// The connection is the session's, and is closed with it.
		if _, err = conn.Write([]byte{0}); err == nil {
			t.Error("the connection is open after a session refused")
		}
	})

	t.Run("a subscription", func(t *testing.T) {
		t.Parallel()

		session, peer := opened(t, mqtt.Options{})

		go func() {
			_, _ = io.CopyN(io.Discard, peer.reader, 9)
			_, _ = peer.conn.Write([]byte{0x90, 4, 0, 1, 0, 0x87})
		}()

		err := session.Subscribe(t.Context(), "a/b")

		var refused *mqtt.RefusedError
		if !errors.As(err, &refused) || refused.Reason != 0x87 || refused.What != "subscription" {
			t.Errorf("Subscribe = %v, want a refusal for reason 135", err)
		}

		if session.Err() != nil {
			t.Errorf("the session ended with %v on a subscription refused", session.Err())
		}
	})

	t.Run("a session ended by the broker", func(t *testing.T) {
		t.Parallel()

		session, peer := opened(t, mqtt.Options{})

		peer.send(append(unhexed(`30 06 00 01 61 00`), 'x', 'y'))
		peer.send([]byte{0xE0, 2, 0x8E, 0})

		<-session.Done()

		// What came before the end is still read, and then the end.
		message, err := session.Next(t.Context())
		if err != nil || message.Topic != "a" || string(message.Payload) != "xy" {
			t.Errorf("Next = %q %q, %v", message.Topic, message.Payload, err)
		}

		_, err = session.Next(t.Context())

		var refused *mqtt.RefusedError
		if !errors.As(err, &refused) || refused.Reason != 0x8E || refused.What != "ended" {
			t.Errorf("Next after the end = %v, want the broker's reason 142", err)
		}
	})
}

// TestKeptAlive: a session silent for half of what it may be pings; one
// that hears nothing for that long and half again is lost. The broker's
// own figure is the one kept.
func TestKeptAlive(t *testing.T) {
	t.Parallel()

	t.Run("pings, and is answered", func(t *testing.T) {
		t.Parallel()

		session, peer := opened(t, mqtt.Options{KeepAlive: time.Second})

		begun := time.Now()

		for range 3 {
			peer.expect("a ping", []byte{0xC0, 0})
			peer.send([]byte{0xD0, 0})
		}

		if spent := time.Since(begun); spent < time.Second || spent > 4*time.Second {
			t.Errorf("three pings took %s, want them half a second apart", spent)
		}

		if session.Err() != nil {
			t.Errorf("the session ended with %v", session.Err())
		}
	})

	t.Run("hears nothing", func(t *testing.T) {
		t.Parallel()

		session, _ := opened(t, mqtt.Options{KeepAlive: time.Second})

		begun := time.Now()

		select {
		case <-session.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the session goes on with a broker that says nothing")
		}

		if spent := time.Since(begun); !errors.Is(session.Err(), mqtt.ErrLost) || spent < time.Second {
			t.Errorf("the session ended with %v after %s, want ErrLost after a second and a half", session.Err(), spent)
		}
	})

	t.Run("the broker names another", func(t *testing.T) {
		t.Parallel()

		conn, peer := dialed(t)

		go func() {
			_, _ = io.CopyN(io.Discard, peer.reader, int64(len(unhexed(connect))))
			// A user property, then a keep alive of one second.
			_, _ = peer.conn.Write(unhexed(`20 0d 00 00 0a 26 00 01 6b 00 01 76 13 00 01`))
		}()

		session, err := mqtt.Connect(t.Context(), conn, mqtt.Options{User: user, Password: password})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}

		defer func() { _ = session.Close() }()

		peer.expect("a ping within the broker's second", []byte{0xC0, 0})
	})
}

// TestMessages: what comes is given in order; one that wants an
// acknowledgement gets it; one to be delivered exactly once, which
// nothing here subscribes to, ends the session; and more unread than a
// session holds ends it rather than drop one.
func TestMessages(t *testing.T) {
	t.Parallel()

	t.Run("acknowledged", func(t *testing.T) {
		t.Parallel()

		session, peer := opened(t, mqtt.Options{})

		// Quality one, the packet numbered 0x0102, with a property.
		peer.send(unhexed(`32 0d 00 01 61 01 02 06 26 00 01 6b 00 00 7a`))
		peer.expect("the acknowledgement", []byte{0x40, 2, 1, 2})

		message, err := session.Next(t.Context())
		if err != nil || message.Topic != "a" || string(message.Payload) != "z" {
			t.Errorf("Next = %q %q, %v", message.Topic, message.Payload, err)
		}
	})

	t.Run("exactly once", func(t *testing.T) {
		t.Parallel()

		session, peer := opened(t, mqtt.Options{})

		peer.send(unhexed(`34 07 00 01 61 01 02 00 7a`))

		if _, err := session.Next(t.Context()); !errors.Is(err, mqtt.ErrProtocol) {
			t.Errorf("Next = %v, want ErrProtocol", err)
		}
	})

	t.Run("more than are read", func(t *testing.T) {
		t.Parallel()

		session, peer := opened(t, mqtt.Options{})

		// The session ends before the last is written, and the rest go
		// nowhere.
		for range 300 {
			_, _ = peer.conn.Write(unhexed(`30 05 00 01 61 00 7a`))
		}

		<-session.Done()

		read := 0

		for {
			_, err := session.Next(t.Context())
			if err != nil {
				if !errors.Is(err, mqtt.ErrOverrun) {
					t.Errorf("the session ended with %v, want ErrOverrun", err)
				}

				break
			}

			read++
		}

		if read != 256 {
			t.Errorf("%d messages were read before the end, want the 256 a session holds", read)
		}
	})
}

// TestMalformed: what is no MQTT ends the session as such.
func TestMalformed(t *testing.T) {
	t.Parallel()

	for name, packet := range map[string]string{
		"a length of five bytes":       `30ffffffff01`,
		"a short length in five bytes": `d0 80 80 80 80 00`,
		"a packet above the bound":     `30 81 80 80 02`,
		"a topic cut":                  `30 03 00 09 61`,
		"properties cut":               `30 05 00 01 61 09 7a`,
		"a number cut":                 `32 04 00 01 61 01`,
		"a SUBACK with no reason":      `90 03 00 01 00`,
		"a SUBACK with two reasons":    `90 05 00 01 00 00 00`,
		"a SUBACK cut in its number":   `90 01 00`,
		"a SUBACK with properties cut": `90 04 00 01 09 00`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			session, peer := opened(t, mqtt.Options{})

			peer.send(unhexed(packet))

			if _, err := session.Next(t.Context()); !errors.Is(err, mqtt.ErrProtocol) {
				t.Errorf("Next = %v, want ErrProtocol", err)
			}
		})
	}

	for name, answer := range map[string]string{
		"no CONNACK":            `90 04 00 01 00 00`,
		"a CONNACK cut":         `20 01 00`,
		"properties cut":        `20 04 00 00 09 13`,
		"a property of no kind": `20 05 00 00 02 7f 00`,
		"a property cut":        `20 04 00 00 01 13`,
		"a pair cut":            `20 08 00 00 05 26 00 01 6b 00`,
	} {
		t.Run("opening: "+name, func(t *testing.T) {
			t.Parallel()

			conn, peer := dialed(t)

			go func() {
				_, _ = io.CopyN(io.Discard, peer.reader, int64(len(unhexed(connect))))
				_, _ = peer.conn.Write(unhexed(answer))
			}()

			if _, err := mqtt.Connect(
				t.Context(),
				conn,
				mqtt.Options{User: user, Password: password},
			); !errors.Is(
				err,
				mqtt.ErrProtocol,
			) {
				t.Errorf("Connect = %v, want ErrProtocol", err)
			}
		})
	}
}

// TestEnds: a context that ends puts an end to what waits on the broker, and the
// session with it when it was being opened; a connection that fails is a
// session lost; and what cannot be sent is not.
func TestEnds(t *testing.T) {
	t.Parallel()

	t.Run("opening", func(t *testing.T) {
		t.Parallel()

		conn, _ := dialed(t)

		brief, done := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer done()

		if _, err := mqtt.Connect(brief, conn, mqtt.Options{}); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Connect to a broker that says nothing = %v, want the context's end", err)
		}
	})

	t.Run("subscribing and waiting", func(t *testing.T) {
		t.Parallel()

		session, _ := opened(t, mqtt.Options{})

		brief, done := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer done()

		if err := session.Subscribe(brief, "a/b"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Subscribe unanswered = %v, want the context's end", err)
		}

		if _, err := session.Next(brief); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Next with nothing coming = %v, want the context's end", err)
		}

		if session.Err() != nil {
			t.Errorf("the session ended with %v", session.Err())
		}
	})

	t.Run("the connection fails", func(t *testing.T) {
		t.Parallel()

		session, peer := opened(t, mqtt.Options{})

		_ = peer.conn.Close()

		if _, err := session.Next(t.Context()); !errors.Is(err, mqtt.ErrLost) {
			t.Errorf("Next = %v, want ErrLost", err)
		}

		if err := session.Subscribe(t.Context(), "a/b"); !errors.Is(err, mqtt.ErrLost) {
			t.Errorf("Subscribe on a session lost = %v, want ErrLost", err)
		}
	})

	t.Run("what cannot be sent", func(t *testing.T) {
		t.Parallel()

		session, _ := opened(t, mqtt.Options{})

		long := strings.Repeat("a", 1<<16)

		if err := session.Subscribe(t.Context(), ""); !errors.Is(err, mqtt.ErrArgument) {
			t.Errorf("Subscribe to no topic = %v, want ErrArgument", err)
		}

		if err := session.Publish(t.Context(), long, nil); !errors.Is(err, mqtt.ErrArgument) {
			t.Errorf("Publish to a topic too long = %v, want ErrArgument", err)
		}

		conn, _ := dialed(t)

		if _, err := mqtt.Connect(t.Context(), conn, mqtt.Options{User: long}); !errors.Is(err, mqtt.ErrArgument) {
			t.Errorf("Connect under a name too long = %v, want ErrArgument", err)
		}
	})
}

// TestSeveralAtOnce: subscriptions made at once are each answered by
// their own number, whatever order the broker answers in.
func TestSeveralAtOnce(t *testing.T) {
	t.Parallel()

	session, peer := opened(t, mqtt.Options{})

	const several = 8

	go func() {
		numbers := make([][2]byte, 0, several)

		for range several {
			_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

			head := make([]byte, 4)
			if _, err := io.ReadFull(peer.reader, head); err != nil {
				return
			}

			_, _ = io.CopyN(io.Discard, peer.reader, int64(head[1])-2)

			numbers = append(numbers, [2]byte{head[2], head[3]})
		}

		// Answered last first, the odd ones refused.
		for index := several - 1; index >= 0; index-- {
			reason := byte(0)
			if numbers[index][1]%2 == 1 {
				reason = 0x80
			}

			_, _ = peer.conn.Write([]byte{0x90, 4, numbers[index][0], numbers[index][1], 0, reason})
		}
	}()

	var (
		asking  sync.WaitGroup
		mu      sync.Mutex
		refused int
	)

	for range several {
		asking.Go(func() {
			err := session.Subscribe(t.Context(), "a/b")

			mu.Lock()
			defer mu.Unlock()

			switch {
			case errors.Is(err, mqtt.ErrRefused):
				refused++
			case err != nil:
				t.Errorf("Subscribe: %v", err)
			}
		})
	}

	asking.Wait()

	if refused != several/2 {
		t.Errorf("%d subscriptions were refused, want the %d the broker refused", refused, several/2)
	}
}
