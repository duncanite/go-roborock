// Package mqtt is the little of MQTT 5 that Roborock's broker is spoken
// to in: a session opened with a user name and a password, kept alive by
// pings, on which topics are subscribed to and messages published and
// received, all at most once. Nothing is stored, nothing is sent twice,
// and nothing is reconnected: a session that is lost is over.
//
// It is the standard library alone, on a connection its caller made.
package mqtt

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// KeepAlive is how long a session may be silent, when Options name none:
// what Roborock's own clients ask for.
const KeepAlive = 45 * time.Second

const (
	// maxPacket bounds one packet read: a map of a home comes in frames
	// of at most 64 kB, a few to a message.
	maxPacket = 4 << 20
	// unread is how many messages wait for Next before the session ends
	// as overrun.
	unread = 256
	// written bounds one write.
	written = 10 * time.Second
	// short is the size of a length, and of a packet's number.
	short = 2
	// level is the version of the protocol: 5.
	level = 5
	// flags of a CONNECT: a user name, a password, and a clean start.
	connectFlags = 0xC2
)

// The packets, by the high half of their first byte.
const (
	typeConnect    = 1
	typeConnack    = 2
	typePublish    = 3
	typePuback     = 4
	typeSubscribe  = 8
	typeSuback     = 9
	typePingreq    = 12
	typePingresp   = 13
	typeDisconnect = 14
)

// The errors of a session, to test with errors.Is.
var (
	// ErrRefused: the broker refused the session, a subscription, or
	// ended the session itself. The error is a *RefusedError, with the
	// reason.
	ErrRefused = errors.New("mqtt: refused by the broker")
	// ErrLost: the connection failed, or the broker fell silent for
	// longer than a session may be.
	ErrLost = errors.New("mqtt: the session is lost")
	// ErrClosed: the session was closed by its owner.
	ErrClosed = errors.New("mqtt: the session is closed")
	// ErrProtocol: the broker sent what is no MQTT 5, or more than a
	// packet may be.
	ErrProtocol = errors.New("mqtt: malformed packet")
	// ErrOverrun: more messages came than were read; the session ended
	// rather than drop one in silence.
	ErrOverrun = errors.New("mqtt: more messages than were read")
	// ErrArgument: something that cannot be sent; nothing was.
	ErrArgument = errors.New("mqtt: not a valid argument")
)

// RefusedError is a refusal by the broker, with the reason code it gave.
type RefusedError struct {
	// What was refused: "session", "subscription", or "ended" when the
	// broker ended a session it had taken.
	What string
	// Reason is the broker's reason code, as MQTT 5 numbers them: 134
	// a bad user name or password, 135 not authorized.
	Reason byte
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%s: %s, reason %d", ErrRefused.Error(), e.What, e.Reason)
}

// Is makes a RefusedError an ErrRefused.
func (*RefusedError) Is(target error) bool { return target == ErrRefused }

// Options shape a session.
type Options struct {
	// User and Password are who the session is for.
	User     string
	Password string
	// KeepAlive is how long the session may be silent; zero is
	// KeepAlive. The broker may name another, which is then kept.
	KeepAlive time.Duration
}

// Message is one message received.
type Message struct {
	Topic   string
	Payload []byte
}

// Session is a session with a broker. Its methods may be called from
// several goroutines.
type Session struct {
	conn net.Conn
	keep time.Duration

	writing sync.Mutex
	wrote   atomic.Int64
	heard   atomic.Int64

	mu       sync.Mutex
	waiting  map[uint16]chan byte
	last     uint16
	err      error
	done     chan struct{}
	messages chan Message
	running  sync.WaitGroup
}

// Connect opens a session on a connection, which is the session's from
// then on: it is closed when Connect fails, and when the session ends.
// The context bounds the opening alone.
func Connect(ctx context.Context, conn net.Conn, opts Options) (*Session, error) {
	keep := opts.KeepAlive
	if keep <= 0 {
		keep = KeepAlive
	}

	seconds := keep / time.Second
	if seconds < 1 || seconds > 1<<16-1 || len(opts.User) > 1<<16-1 || len(opts.Password) > 1<<16-1 {
		_ = conn.Close()

		return nil, fmt.Errorf("%w: a keep alive of %s, or a name too long", ErrArgument, keep)
	}

	// A context that ends unblocks what is being read or written.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })

	body := text(nil, "MQTT")
	body = append(body, level, connectFlags)
	body = binary.BigEndian.AppendUint16(body, uint16(seconds)) // #nosec G115 -- bounded above
	body = append(body, 0)                                      // no property
	body = text(body, "")                                       // the broker names the client
	body = text(body, opts.User)
	body = text(body, opts.Password)

	reader := bufio.NewReader(conn)

	keep, err := opened(conn, reader, packet(typeConnect<<4, body), keep)
	if err != nil || !stop() {
		_ = conn.Close()

		if ctx.Err() != nil {
			err = fmt.Errorf("opening the session: %w", context.Cause(ctx))
		}

		return nil, err
	}

	session := &Session{
		conn:     conn,
		keep:     keep,
		waiting:  map[uint16]chan byte{},
		done:     make(chan struct{}),
		messages: make(chan Message, unread),
	}

	session.wrote.Store(time.Now().UnixNano())
	session.heard.Store(time.Now().UnixNano())

	session.running.Go(func() { session.read(reader) })
	session.running.Go(session.alive)

	return session, nil
}

// opened sends the CONNECT and reads its answer: how long the session may
// be silent, which the broker may have set.
func opened(conn net.Conn, reader *bufio.Reader, connect []byte, keep time.Duration) (time.Duration, error) {
	if _, err := conn.Write(connect); err != nil {
		return 0, fmt.Errorf("%w: opening the session: %w", ErrLost, err)
	}

	answer, err := read(reader)
	if err != nil {
		return 0, fmt.Errorf("opening the session: %w", err)
	}

	kind, body := answer.kind, answer.body

	// The acknowledge flags, the reason, then the properties.
	if kind != typeConnack || len(body) < short {
		return 0, fmt.Errorf(
			"%w: the answer to a CONNECT is a packet of type %d, of %d bytes",
			ErrProtocol,
			kind,
			len(body),
		)
	}

	if body[1] != 0 {
		return 0, &RefusedError{What: "session", Reason: body[1]}
	}

	properties, _, err := block(body[short:])
	if err != nil {
		return 0, err
	}

	const serverKeepAlive = 0x13

	err = walk(properties, func(id byte, value []byte) {
		if id == serverKeepAlive {
			keep = time.Duration(binary.BigEndian.Uint16(value)) * time.Second
		}
	})
	if err != nil {
		return 0, err
	}

	if keep <= 0 {
		// The broker asks for no keep alive: ours is kept.
		keep = KeepAlive
	}

	return keep, nil
}

// Subscribe has the broker send what is published to a topic, at most
// once each. It returns when the broker has taken the subscription, or
// refused it.
func (s *Session) Subscribe(ctx context.Context, topic string) error {
	if topic == "" || len(topic) > 1<<16-1 {
		return fmt.Errorf("%w: a topic of %d bytes", ErrArgument, len(topic))
	}

	answer := make(chan byte, 1)

	s.mu.Lock()

	if s.err != nil {
		s.mu.Unlock()

		return s.err
	}

	// A packet's number is never zero.
	for {
		s.last++

		if _, taken := s.waiting[s.last]; s.last != 0 && !taken {
			break
		}
	}

	number := s.last
	s.waiting[number] = answer
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.waiting, number)
		s.mu.Unlock()
	}()

	body := binary.BigEndian.AppendUint16(nil, number)
	body = append(body, 0) // no property
	body = text(body, topic)
	body = append(body, 0) // at most once

	// A SUBSCRIBE has flags of its own.
	const subscribing = typeSubscribe<<4 | 0b0010

	if err := s.write(packet(subscribing, body)); err != nil {
		return err
	}

	select {
	case reason := <-answer:
		const failures = 0x80

		if reason >= failures {
			return &RefusedError{What: "subscription", Reason: reason}
		}

		return nil
	case <-s.done:
		return s.Err()
	case <-ctx.Done():
		return fmt.Errorf("subscribing: %w", context.Cause(ctx))
	}
}

// Publish sends a message to a topic, at most once: it returns when the
// message is written, and nothing says the broker took it.
func (s *Session) Publish(_ context.Context, topic string, payload []byte) error {
	if topic == "" || len(topic) > 1<<16-1 || len(payload) > maxPacket {
		return fmt.Errorf("%w: a topic of %d bytes, a payload of %d", ErrArgument, len(topic), len(payload))
	}

	body := text(make([]byte, 0, short+len(topic)+1+len(payload)), topic)
	body = append(body, 0) // no property
	body = append(body, payload...)

	return s.write(packet(typePublish<<4, body))
}

// Next is the next message received, in the order the broker sent them.
// When the session has ended it is the messages still unread, then the
// session's error.
func (s *Session) Next(ctx context.Context) (Message, error) {
	select {
	case message := <-s.messages:
		return message, nil
	default:
	}

	select {
	case message := <-s.messages:
		return message, nil
	case <-s.done:
		select {
		case message := <-s.messages:
			return message, nil
		default:
			return Message{}, s.Err()
		}
	case <-ctx.Done():
		return Message{}, fmt.Errorf("waiting for a message: %w", context.Cause(ctx))
	}
}

// Close ends the session, telling the broker so, and closes the
// connection. It returns when nothing of the session runs any more.
func (s *Session) Close() error {
	// Written only while the session lasts.
	_ = s.write([]byte{typeDisconnect << 4, 0})

	s.fail(ErrClosed)
	s.running.Wait()

	return nil
}

// Done is closed when the session has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is why the session ended; nil while it has not.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}

// fail ends the session, for the first reason given.
func (s *Session) fail(err error) {
	s.mu.Lock()

	if s.err != nil {
		s.mu.Unlock()

		return
	}

	s.err = err
	close(s.done)
	s.mu.Unlock()

	_ = s.conn.Close()
}

// write sends one packet, whole, one at a time.
func (s *Session) write(packet []byte) error {
	s.writing.Lock()
	defer s.writing.Unlock()

	if err := s.Err(); err != nil {
		return err
	}

	_ = s.conn.SetWriteDeadline(time.Now().Add(written))

	if _, err := s.conn.Write(packet); err != nil {
		s.fail(fmt.Errorf("%w: %w", ErrLost, err))

		return s.Err()
	}

	s.wrote.Store(time.Now().UnixNano())

	return nil
}

// alive pings the broker when nothing was sent for half of what the
// session may be silent, and ends the session when nothing was heard for
// that long and half again.
func (s *Session) alive() {
	const (
		checks = 4
		half   = 2
		grace  = 3
	)

	ticker := time.NewTicker(s.keep / checks)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			if now.Sub(time.Unix(0, s.heard.Load())) > s.keep*grace/half {
				s.fail(fmt.Errorf("%w: nothing heard for %s", ErrLost, s.keep*grace/half))

				return
			}

			if now.Sub(time.Unix(0, s.wrote.Load())) >= s.keep/half {
				_ = s.write([]byte{typePingreq << 4, 0})
			}
		}
	}
}

// read takes packets as they come until the session ends.
func (s *Session) read(reader *bufio.Reader) {
	for {
		got, err := read(reader)
		if err != nil {
			s.fail(err)

			return
		}

		s.heard.Store(time.Now().UnixNano())

		switch got.kind {
		case typePublish:
			err = s.published(got.flags, got.body)
		case typeSuback:
			err = s.subscribed(got.body)
		case typeDisconnect:
			// With no reason given, the broker ended it normally.
			var reason byte
			if len(got.body) > 0 {
				reason = got.body[0]
			}

			err = &RefusedError{What: "ended", Reason: reason}
		default:
			// A ping's answer, and what nothing here asked for.
		}

		if err != nil {
			s.fail(err)

			return
		}
	}
}

// published takes one PUBLISH: its topic, its number when it wants an
// acknowledgement, its properties, and the rest, its payload.
func (s *Session) published(flags byte, body []byte) error {
	// The quality of service is two bits; one is at least once.
	const (
		both = 0b11
		once = 1
	)

	topic, rest, err := field(body)
	if err != nil {
		return err
	}

	// Nothing here subscribes but at most once. A broker that sends to
	// be acknowledged all the same is answered, or it would send again;
	// one that sends to be delivered exactly once is not followed.
	var acknowledgement []byte

	switch flags >> 1 & both {
	case 0:
	case once:
		if len(rest) < short {
			return fmt.Errorf("%w: a PUBLISH cut in its number", ErrProtocol)
		}

		acknowledgement = []byte{typePuback << 4, short, rest[0], rest[1]}
		rest = rest[short:]
	default:
		return fmt.Errorf("%w: a PUBLISH to be delivered exactly once, which nothing subscribed to", ErrProtocol)
	}

	_, payload, err := block(rest)
	if err != nil {
		return err
	}

	if acknowledgement != nil {
		if err = s.write(acknowledgement); err != nil {
			return err
		}
	}

	select {
	case s.messages <- Message{Topic: string(topic), Payload: payload}:
		return nil
	default:
		return ErrOverrun
	}
}

// subscribed takes one SUBACK: the number of the subscription it
// answers, properties, and a reason for the one topic asked.
func (s *Session) subscribed(body []byte) error {
	if len(body) < short {
		return fmt.Errorf("%w: a SUBACK of %d bytes", ErrProtocol, len(body))
	}

	_, reasons, err := block(body[short:])
	if err != nil {
		return err
	}

	if len(reasons) != 1 {
		return fmt.Errorf("%w: a SUBACK with %d reasons for one topic", ErrProtocol, len(reasons))
	}

	s.mu.Lock()
	answer, waited := s.waiting[binary.BigEndian.Uint16(body)]
	s.mu.Unlock()

	if waited {
		// One answer is taken; a second for the same number is dropped.
		select {
		case answer <- reasons[0]:
		default:
		}
	}

	return nil
}

// packet is a packet as it is sent: its first byte, the length of the
// rest, and the rest.
func packet(first byte, body []byte) []byte {
	const longest = 4

	out := make([]byte, 0, 1+longest+len(body))
	out = append(out, first)

	// Seven bits a byte, the high bit saying another follows.
	const more = 0x80

	length := len(body)
	for {
		digit := byte(length % more)

		if length /= more; length > 0 {
			out = append(out, digit|more)

			continue
		}

		out = append(out, digit)

		break
	}

	return append(out, body...)
}

// received is one packet: its type, its flags and the rest of it.
type received struct {
	kind  byte
	flags byte
	body  []byte
}

// read takes one packet off the connection.
func read(reader *bufio.Reader) (received, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return received{}, lost(err)
	}

	// The length of the rest: seven bits a byte, the high bit saying
	// another follows, four bytes at most.
	const (
		more    = 0x80
		bits    = 7
		longest = 4
	)

	length := 0

	for read := 0; ; read++ {
		if read == longest {
			return received{}, fmt.Errorf("%w: a length of more than four bytes", ErrProtocol)
		}

		digit, digitErr := reader.ReadByte()
		if digitErr != nil {
			return received{}, lost(digitErr)
		}

		length |= int(digit&^more) << (bits * read)

		if digit&more == 0 {
			break
		}
	}

	if length > maxPacket {
		return received{}, fmt.Errorf("%w: a packet of %d bytes", ErrProtocol, length)
	}

	body := make([]byte, length)
	if _, err = io.ReadFull(reader, body); err != nil {
		return received{}, lost(err)
	}

	const half = 4

	return received{kind: first >> half, flags: first & (1<<half - 1), body: body}, nil
}

// lost is the error for a connection that failed.
func lost(err error) error { return fmt.Errorf("%w: %w", ErrLost, err) }

// text appends a text as MQTT writes one: its length in two bytes, then
// it.
func text(to []byte, value string) []byte {
	to = binary.BigEndian.AppendUint16(to, uint16(len(value))) // #nosec G115 -- every caller bounds it

	return append(to, value...)
}

// field takes a text or binary field off the front: its length in two
// bytes, then it.
func field(from []byte) (value, rest []byte, err error) {
	if len(from) < short {
		return nil, nil, fmt.Errorf("%w: a field cut in its length", ErrProtocol)
	}

	length := int(binary.BigEndian.Uint16(from))
	if len(from) < short+length {
		return nil, nil, fmt.Errorf("%w: a field of %d bytes cut", ErrProtocol, length)
	}

	return from[short : short+length], from[short+length:], nil
}

// block takes a block of properties off the front: its length, then it.
func block(from []byte) (properties, rest []byte, err error) {
	length, size := binary.Uvarint(from)
	if size <= 0 || length > uint64(len(from)-size) { // #nosec G115 -- size is at most the length it is taken from
		return nil, nil, fmt.Errorf("%w: properties cut", ErrProtocol)
	}

	end := size + int(length) // #nosec G115 -- bounded by the packet above

	return from[size:end], from[end:], nil
}

// shapes is the shape of the value of each property MQTT 5 has, by its
// identifier, from zero on: b a byte, t two bytes, f four, n a number of
// a variable length, s a text or binary field, p a pair of fields, and a
// space where the identifier is no property's.
//
//nolint:dupword // a table, not a sentence
const shapes = " bfs    ss n     fst ssbfbs s  s tttbbpfbbb"

// walk gives each property of a block with its value, for the ones whose
// value is of a fixed size, and passes over the others.
func walk(properties []byte, each func(id byte, value []byte)) error {
	malformed := fmt.Errorf("%w: properties that do not read", ErrProtocol)

	for len(properties) > 0 {
		kind := properties[0]
		properties = properties[1:]

		if int(kind) >= len(shapes) {
			return malformed
		}

		size := sized(shapes[kind], properties)
		if size <= 0 || size > len(properties) {
			return malformed
		}

		each(kind, properties[:size])

		properties = properties[size:]
	}

	return nil
}

// sized is how many bytes a value of a shape takes at the front of what
// is given; zero or less when it is cut, or of no shape.
func sized(shape byte, from []byte) int {
	switch shape {
	case 'b':
		return 1
	case 't':
		return short
	case 'f':
		return short + short
	case 'n':
		_, size := binary.Uvarint(from)

		return size
	case 's':
		return fieldSize(from)
	case 'p':
		first := fieldSize(from)
		if first <= 0 || first > len(from) {
			return 0
		}

		second := fieldSize(from[first:])
		if second <= 0 {
			return 0
		}

		return first + second
	default:
		return 0
	}
}

// fieldSize is how many bytes a field takes, its length included; zero
// when it is cut in its length.
func fieldSize(from []byte) int {
	if len(from) < short {
		return 0
	}

	return short + int(binary.BigEndian.Uint16(from))
}
