package roborock

import (
	"context"
	"crypto/md5" // #nosec G501 -- how the broker's credentials are derived, no choice of this package
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sync"

	"github.com/duncanite/go-roborock/internal/mqtt"
)

// The errors of a session and of a robot, to test with errors.Is.
var (
	// ErrArgument: something that cannot be sent or opened; nothing was.
	ErrArgument = errors.New("roborock: not a valid argument")
	// ErrUnsupported: a device this package does not speak to. It speaks
	// to the Q7 family, of the protocol B01, through the cloud.
	ErrUnsupported = errors.New("roborock: not a device this package speaks to")
	// ErrCredentials: the cloud refused the account's credentials. It
	// does so too when it was asked too much, and nothing tells the two
	// apart; a new login has been seen to clear both.
	ErrCredentials = errors.New("roborock: the cloud refused the account's credentials")
	// ErrRefused: the cloud or a robot refused what was asked. A robot's
	// refusal is a *RefusedError, with its code.
	ErrRefused = errors.New("roborock: refused")
	// ErrLost: the connection to the cloud failed, or fell silent.
	ErrLost = errors.New("roborock: the connection to the cloud is lost")
	// ErrClosed: the session was closed by its owner.
	ErrClosed = errors.New("roborock: the session is closed")
	// ErrAnswer: a robot's answer is not in the shape the open
	// implementations read.
	ErrAnswer = errors.New("roborock: the answer is not in the expected shape")
)

// Dialer makes the connection to the cloud's broker; a *net.Dialer is
// one.
type Dialer interface {
	// DialContext connects, as net.Dialer.DialContext does.
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Options shape a session.
type Options struct {
	// Dialer makes the connection; nil is a net.Dialer.
	Dialer Dialer
	// TLS is how the broker is trusted; nil is the system's roots, for
	// the broker's own name. A session is under TLS always: one given
	// with no server name gets the broker's.
	TLS *tls.Config
}

// Session is an account's session with Roborock's cloud: one connection,
// which all the account's robots are spoken to through. Its methods may
// be called from several goroutines.
//
// Nothing reconnects it: a session lost is over, and Done says so.
type Session struct {
	broker *mqtt.Session
	cloud  Cloud
	user   string

	mu     sync.Mutex
	robots map[string]*Robot

	routing sync.WaitGroup
}

// defaultPort is the broker's when its address names none.
const defaultPort = "8883"

// Connect opens a session with the cloud under an account's credentials,
// as a login gave them. The context bounds the opening alone. A broker
// that refuses the credentials is ErrCredentials.
func Connect(ctx context.Context, cloud Cloud, opts Options) (*Session, error) {
	address, err := url.Parse(cloud.Broker)
	if err != nil || address.Hostname() == "" {
		// The address is the cloud's text, and is not said.
		return nil, fmt.Errorf("%w: the broker's address does not read", ErrArgument)
	}

	// The credentials go where the address says: in clear they go nowhere.
	switch address.Scheme {
	case "ssl", "tls", "mqtts":
	default:
		return nil, fmt.Errorf("%w: a broker that is not spoken to under TLS", ErrArgument)
	}

	if cloud.User == "" || cloud.Secret == "" || cloud.Key == "" {
		return nil, fmt.Errorf("%w: credentials with a part missing", ErrArgument)
	}

	port := address.Port()
	if port == "" {
		port = defaultPort
	}

	dialer := opts.Dialer
	if dialer == nil {
		dialer = new(net.Dialer)
	}

	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address.Hostname(), port))
	if err != nil {
		return nil, fmt.Errorf("%w: reaching the broker: %w", ErrLost, err)
	}

	trust := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLS != nil {
		trust = opts.TLS.Clone()
	}

	if trust.ServerName == "" {
		trust.ServerName = address.Hostname()
	}

	secured := tls.Client(conn, trust)

	if err = secured.HandshakeContext(ctx); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("%w: securing the connection to the broker: %w", ErrLost, err)
	}

	user := digest(cloud.User + ":" + cloud.Key)[2:10]

	broker, err := mqtt.Connect(ctx, secured, mqtt.Options{
		User:     user,
		Password: digest(cloud.Secret + ":" + cloud.Key)[16:],
	})
	if err != nil {
		return nil, said(err)
	}

	session := &Session{broker: broker, cloud: cloud, user: user, robots: map[string]*Robot{}}

	session.routing.Go(session.route)

	return session, nil
}

// digest is the MD5 of a text, in hexadecimal.
func digest(text string) string {
	sum := md5.Sum([]byte(text)) // #nosec G401 -- how the broker's credentials are derived

	return hex.EncodeToString(sum[:])
}

// The reasons a broker refuses with that are the account's credentials:
// a bad user name or password, and not authorized.
const (
	reasonBadCredentials = 134
	reasonNotAuthorized  = 135
)

// said is an error of the broker's session in this package's words.
func said(err error) error {
	var refused *mqtt.RefusedError

	switch {
	case err == nil:
		return nil
	case errors.As(err, &refused):
		// A subscription refused so is the robot's, which is not the
		// account's; the session refused or ended so is the account's.
		if refused.What != "subscription" &&
			(refused.Reason == reasonBadCredentials || refused.Reason == reasonNotAuthorized) {
			return fmt.Errorf("%w: the broker's reason %d", ErrCredentials, refused.Reason)
		}

		return fmt.Errorf("%w: by the broker, the %s, reason %d", ErrRefused, refused.What, refused.Reason)
	case errors.Is(err, mqtt.ErrClosed):
		return ErrClosed
	case errors.Is(err, mqtt.ErrArgument):
		return fmt.Errorf("%w: %w", ErrArgument, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return fmt.Errorf("%w: %w", ErrLost, err)
	}
}

// named is an id that can go into a topic as it is.
var named = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Robot opens a robot of the account: the cloud is asked for what the
// robot says, and the robot is spoken to from then on. A robot already
// open on the session is ErrArgument; one that is not of the Q7 family is
// ErrUnsupported.
func (s *Session) Robot(ctx context.Context, identity Identity) (*Robot, error) {
	if !named.MatchString(identity.ID) {
		return nil, fmt.Errorf("%w: a device's id is letters, digits, dashes and underscores", ErrArgument)
	}

	if !Speaks(identity) {
		return nil, fmt.Errorf(
			"%w: the protocol %q and the model %q",
			ErrUnsupported,
			identity.Protocol,
			identity.Model,
		)
	}

	robot := &Robot{
		session:  s,
		identity: identity,
		asking:   "rr/m/i/" + s.cloud.User + "/" + s.user + "/" + identity.ID,
		hearing:  "rr/m/o/" + s.cloud.User + "/" + s.user + "/" + identity.ID,
		waiting:  map[string]chan answer{},
	}

	s.mu.Lock()

	if _, open := s.robots[robot.hearing]; open {
		s.mu.Unlock()

		return nil, fmt.Errorf("%w: the robot is open on this session already", ErrArgument)
	}

	s.robots[robot.hearing] = robot
	s.mu.Unlock()

	if err := s.broker.Subscribe(ctx, robot.hearing); err != nil {
		s.mu.Lock()
		delete(s.robots, robot.hearing)
		s.mu.Unlock()

		return nil, said(err)
	}

	return robot, nil
}

// Close ends the session and closes its connection. It returns when
// nothing of the session runs any more.
func (s *Session) Close() error {
	err := s.broker.Close()

	s.routing.Wait()

	if err != nil {
		return said(err)
	}

	return nil
}

// Done is closed when the session has ended.
func (s *Session) Done() <-chan struct{} { return s.broker.Done() }

// Err is why the session ended; nil while it has not.
func (s *Session) Err() error { return said(s.broker.Err()) }

// route gives each message of the cloud's to the robot it is from, until
// the session ends.
func (s *Session) route() {
	for {
		message, err := s.broker.Next(context.Background())
		if err != nil {
			return
		}

		s.mu.Lock()
		robot := s.robots[message.Topic]
		s.mu.Unlock()

		if robot != nil {
			robot.heard(message.Payload)
		}
	}
}
