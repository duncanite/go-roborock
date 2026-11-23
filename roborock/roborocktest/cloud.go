// Package roborocktest is Roborock's cloud for tests: its MQTT broker,
// under TLS, with an account and one robot of the Q7 family behind it,
// which answers what it is asked as the open implementations read a
// robot.
package roborocktest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5" // #nosec G501 -- how the real broker's credentials are derived
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/duncanite/go-roborock/internal/wire"
	"github.com/duncanite/go-roborock/roborock"
)

// The account the cloud holds, and its robot.
const (
	User   = "cloud-user"
	Secret = "cloud-secret" // #nosec G101 -- the fake's own, of no account
	Key    = "cloud-key"

	RobotID  = "duid-q7"
	RobotKey = "key123key123key1"
	Model    = "roborock.vacuum.sc01"
)

// What the robot holds when it sits on its dock, charged, after a clean.
const (
	charging = 4
	docked   = 2103
	full     = 100
	balanced = 2
)

// Call is one request the robot received.
type Call struct {
	Method string
	Params string
	// Number is the request's own, as it wrote it.
	Number string
}

// Cloud is the fake, serving until Close. What a test sets is set under
// Set.
type Cloud struct {
	listener net.Listener
	trust    *x509.CertPool
	serving  sync.WaitGroup

	mu sync.Mutex
	// Properties is what the robot holds, by the names its protocol gives
	// them.
	Properties map[string]any
	// Refuse is the code the robot refuses a method with.
	Refuse map[string]int
	// Silent has the robot answer nothing.
	Silent bool
	// Once has the robot pad its answers once, where it pads them twice.
	Once bool
	// Noise has the robot say, before each answer, what a client is to
	// pass over: a map, a frame under another key, an answer to a
	// request nobody made, and what is no frame at all.
	Noise bool
	// Bare has the robot answer with no data.
	Bare bool
	// Reason, when not zero, is the reason the broker refuses a session
	// with.
	Reason byte

	calls []Call
	conns map[net.Conn]struct{}
}

// New is a cloud whose robot sits on its dock, charged.
func New() (*Cloud, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("the broker's key: %w", err)
	}

	const lasting = time.Hour

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "roborocktest"},
		NotBefore:             time.Now().Add(-lasting),
		NotAfter:              time.Now().Add(lasting),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)}, //nolint:mnd // the loopback address
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("the broker's certificate: %w", err)
	}

	parsed, err := x509.ParseCertificate(raw)
	if err != nil {
		return nil, fmt.Errorf("the broker's certificate: %w", err)
	}

	trust := x509.NewCertPool()
	trust.AddCert(parsed)

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: key}},
	})
	if err != nil {
		return nil, fmt.Errorf("the broker listening: %w", err)
	}

	cloud := &Cloud{
		listener: listener,
		trust:    trust,
		Properties: map[string]any{
			"status": charging, "fault": docked, "quantity": full, "wind": balanced, "water": 1, "mode": 0,
			"station_act": 0, "dust_action": 0, "net_status": map[string]any{"rssi": -40},
		},
		conns: map[net.Conn]struct{}{},
	}

	cloud.serving.Go(cloud.accept)

	return cloud, nil
}

// Cloud is the account's way into this cloud.
func (c *Cloud) Cloud() roborock.Cloud {
	return roborock.Cloud{Broker: "ssl://" + c.listener.Addr().String(), User: User, Secret: Secret, Key: Key}
}

// Identity is the robot's.
func (*Cloud) Identity() roborock.Identity {
	return roborock.Identity{ID: RobotID, Protocol: "B01", Model: Model, Key: RobotKey}
}

// TLS is what trusts this cloud's broker.
func (c *Cloud) TLS() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.trust}
}

// Set changes what the cloud does, while it serves.
func (c *Cloud) Set(change func(*Cloud)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	change(c)
}

// Calls is every request the robot received so far, in order.
func (c *Cloud) Calls() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]Call(nil), c.calls...)
}

// Drop cuts every session the broker has, as a network does.
func (c *Cloud) Drop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for conn := range c.conns {
		_ = conn.Close()
	}
}

// Close ends the cloud.
func (c *Cloud) Close() {
	_ = c.listener.Close()

	c.Drop()
	c.serving.Wait()
}

func (c *Cloud) accept() {
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}

		c.mu.Lock()
		c.conns[conn] = struct{}{}
		c.mu.Unlock()

		c.serving.Go(func() {
			defer func() {
				_ = conn.Close()

				c.mu.Lock()
				delete(c.conns, conn)
				c.mu.Unlock()
			}()

			c.serve(conn)
		})
	}
}

// Credentials is the user name and the password the broker takes for an
// account's credentials.
func Credentials(cloud roborock.Cloud) (user, password string) {
	return digest(cloud.User + ":" + cloud.Key)[2:10], digest(cloud.Secret + ":" + cloud.Key)[16:]
}

// digest is the MD5 of a text, in hexadecimal.
func digest(text string) string {
	sum := md5.Sum([]byte(text)) // #nosec G401 -- as the real broker has it

	return hex.EncodeToString(sum[:])
}

// The packets of MQTT the broker reads and writes.
const (
	connect    = 1
	publish    = 3
	subscribe  = 8
	pingreq    = 12
	disconnect = 14

	refusedCredentials = 134
	refusedTopic       = 135

	// The first byte of what the broker answers with.
	connack  = 0x20
	suback   = 0x90
	pingresp = 0xD0
)

// line is one session of the broker's.
type line struct {
	cloud      *Cloud
	conn       net.Conn
	writing    sync.Mutex
	subscribed bool
	user       string
	password   string
	hearing    string
	saying     string
}

func (l *line) write(packet ...byte) {
	l.writing.Lock()
	defer l.writing.Unlock()

	_, _ = l.conn.Write(packet)
}

// serve is one session of the broker's, until it ends.
func (c *Cloud) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)

	session := &line{cloud: c, conn: conn}
	session.user, session.password = Credentials(c.Cloud())
	session.hearing = "rr/m/i/" + User + "/" + session.user + "/" + RobotID
	session.saying = "rr/m/o/" + User + "/" + session.user + "/" + RobotID

	for {
		kind, body, err := packet(reader)
		if err != nil || !session.took(kind, body) {
			return
		}
	}
}

// took takes one packet, and says the session goes on.
func (l *line) took(kind byte, body []byte) bool {
	switch kind {
	case connect:
		l.cloud.mu.Lock()
		reason := l.cloud.Reason
		l.cloud.mu.Unlock()

		if reason == 0 && !credentialed(body, l.user, l.password) {
			reason = refusedCredentials
		}

		// With no property.
		l.write(connack, 3, 0, reason, 0)

		return reason == 0
	case subscribe:
		return l.subscribe(body)
	case publish:
		l.publish(body)
	case pingreq:
		l.write(pingresp, 0)
	case disconnect:
		return false
	}

	return true
}

// subscribe takes a subscription, to the robot's topic alone.
func (l *line) subscribe(body []byte) bool {
	// The packet's number, no property, then the topic.
	const before = 3

	if len(body) < before {
		return false
	}

	topic, _, _ := field(body[before:])

	reason := byte(0)
	if string(topic) != l.saying {
		reason = refusedTopic
	}

	l.subscribed = l.subscribed || reason == 0

	// With no property.
	l.write(suback, 4, body[0], body[1], 0, reason)

	return true
}

// publish takes what is published: for the robot, which answers.
func (l *line) publish(body []byte) {
	topic, rest, _ := field(body)
	if string(topic) != l.hearing || len(rest) == 0 {
		return
	}

	for _, message := range l.cloud.asked(rest[1:]) {
		if l.subscribed {
			l.write(published(l.saying, message)...)
		}
	}
}

// packet reads one packet: its type and the rest of it.
func packet(reader *bufio.Reader) (byte, []byte, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err //nolint:wrapcheck // the session ends, whatever it is
	}

	length, err := binary.ReadUvarint(reader)
	if err != nil {
		return 0, nil, err //nolint:wrapcheck // the session ends, whatever it is
	}

	body := make([]byte, length)
	if _, err = io.ReadFull(reader, body); err != nil {
		return 0, nil, err //nolint:wrapcheck // the session ends, whatever it is
	}

	return first >> 4, body, nil
}

// field takes a field off the front: its length in two bytes, then it.
func field(from []byte) (value, rest []byte, ok bool) {
	const short = 2

	if len(from) < short {
		return nil, nil, false
	}

	length := int(binary.BigEndian.Uint16(from))
	if len(from) < short+length {
		return nil, nil, false
	}

	return from[short : short+length], from[short+length:], true
}

// credentialed says a CONNECT is MQTT 5, with no property and no client
// id, under the user name and the password given.
func credentialed(body []byte, user, password string) bool {
	protocol, rest, read := field(body)

	// The level, the flags, the keep alive and no property.
	const fixed = 5

	if !read || string(protocol) != "MQTT" || len(rest) < fixed || rest[0] != 5 || rest[1] != 0xC2 || rest[4] != 0 {
		return false
	}

	client, rest, read := field(rest[fixed:])
	if !read || len(client) != 0 {
		return false
	}

	named, rest, read := field(rest)
	if !read || string(named) != user {
		return false
	}

	secret, _, read := field(rest)

	return read && string(secret) == password
}

// published is a PUBLISH of a message to a topic, to be delivered at
// most once.
func published(topic string, message []byte) []byte {
	body := binary.BigEndian.AppendUint16(nil, uint16(len(topic))) // #nosec G115 -- the fake's own topic
	body = append(body, topic...)
	body = append(body, 0)
	body = append(body, message...)

	return append(binary.AppendUvarint([]byte{0x30}, uint64(len(body))), body...)
}

// asked is what the robot answers a message with: the frames to publish,
// each a message.
func (c *Cloud) asked(message []byte) [][]byte {
	frames, err := wire.Decode(message)
	if err != nil || len(frames) != 1 || frames[0].Protocol != wire.Request {
		return nil
	}

	content, err := wire.Open(RobotKey, frames[0].Random, frames[0].Payload)
	if err != nil {
		return nil
	}

	// A request is padded twice.
	content, twice := wire.Unpad(content)
	if !twice {
		return nil
	}

	var said struct {
		Points map[string]struct {
			Method string          `json:"method"`
			Number string          `json:"msgId"`
			Params json.RawMessage `json:"params"`
		} `json:"dps"`
	}

	if json.Unmarshal(content, &said) != nil {
		return nil
	}

	request, asked := said.Points["10000"]
	if !asked || len(said.Points) != 1 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = append(c.calls, Call{Method: request.Method, Params: string(request.Params), Number: request.Number})

	if c.Silent {
		return nil
	}

	answer := map[string]any{"msgId": request.Number, "code": 0, "method": request.Method}

	if code, refused := c.Refuse[request.Method]; refused {
		answer["code"] = code
	} else if !c.Bare {
		answer["data"] = c.done(request.Method, request.Params)
	}

	var out [][]byte

	if c.Noise {
		other, _ := json.Marshal(map[string]any{"msgId": "1", "code": 0, "data": "another's"})

		out = append(out,
			c.framed(wire.Map, RobotKey, []byte("a map, which is no JSON")),
			c.framed(wire.Response, "another-key-0123", c.pointed(other)),
			c.framed(wire.Response, RobotKey, c.pointed(other)),
			c.framed(wire.Response, RobotKey, []byte(`{"dps":{"10001":"no JSON","10004":7}}`)),
			[]byte("what is no frame"),
		)
	}

	text, _ := json.Marshal(answer)

	return append(out, c.framed(wire.Response, RobotKey, c.pointed(text)))
}

// pointed is an answer as a robot writes it: the time, and under a data
// point the answer as text.
func (*Cloud) pointed(answer []byte) []byte {
	content, _ := json.Marshal(map[string]any{
		"t":   time.Now().Unix(),
		"dps": map[string]string{"10001": string(answer)},
	})

	return content
}

// framed is a content sealed under a key and framed, as a robot's.
func (c *Cloud) framed(protocol uint16, key string, content []byte) []byte {
	const random = 23

	if !c.Once {
		content = wire.Pad(content)
	}

	sealed, err := wire.Seal(key, random, content)
	if err != nil {
		return nil
	}

	frame, err := wire.Frame{
		Seq: 1, Random: random, Timestamp: uint32(time.Now().Unix()), // #nosec G115 -- the protocol's own four bytes
		Protocol: protocol, Payload: sealed,
	}.Encode()
	if err != nil {
		return nil
	}

	return frame
}

// done does what a method asks of the robot, and gives the data of its
// answer.
func (c *Cloud) done(method string, params json.RawMessage) any {
	switch method {
	case "prop.get":
		var asked struct {
			Names []string `json:"property"`
		}

		_ = json.Unmarshal(params, &asked)

		held := map[string]any{}

		for _, name := range asked.Names {
			if value, has := c.Properties[name]; has {
				held[name] = value
			}
		}

		return held
	case "service.set_room_clean":
		var asked struct {
			Control int `json:"ctrl_value"` //nolint:tagliatelle // the protocol's own key
		}

		_ = json.Unmarshal(params, &asked)

		// Stopped, cleaning, paused.
		c.Properties["status"] = []int{1, 5, 2}[asked.Control%3]
	case "service.start_recharge":
		const docking = 3

		c.Properties["status"] = docking
	}

	return "ok"
}
