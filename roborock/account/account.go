// Package account fetches from a Roborock account what reaches its
// devices: the account's way into Roborock's cloud, and for each device
// its id and the key its messages are encrypted under. A device has no
// other source for its key.
//
// Roborock publishes no API. This is the one its own app uses, as the
// open implementations call it: a login by the account's address with a
// one-time code Roborock sends by mail, then the devices of the
// account's home. It cannot run unattended, and Roborock can change it.
//
// Nothing is written anywhere: where a login is kept is the program's
// business.
package account

import (
	"context"
	"crypto/hmac"
	"crypto/md5" // #nosec G501 -- the digest Roborock's API is signed with, no choice of this package
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duncanite/go-roborock/roborock"
)

const (
	// maxAnswer bounds one answer read; a home's devices, each with the
	// list of what its model reports, is some hundreds of kilobytes.
	maxAnswer = 16 << 20

	pathRegion    = "/api/v1/getUrlByEmail"
	pathCode      = "/api/v4/email/code/send"
	pathSign      = "/api/v3/key/sign"
	pathAgreement = "/api/v3/app/agreement/latest"
	pathLogin     = "/api/v4/auth/email/login/code"
	pathHome      = "/api/v1/getHomeDetail"
	pathDevices   = "/v3/user/homes/"

	// The version of the user agreement a login names when Roborock does
	// not say which is the latest.
	agreementMajor = 14
	agreementMinor = 0

	// What the API answers a request it takes with.
	taken = 200

	// The API's own names, which are not in the canonical form of a
	// header's.
	headerClient   = "header_clientid"
	headerLanguage = "header_clientlang"
	language       = "en"

	fieldEmail = "email"
)

// The codes the API refuses with, as far as they are told apart.
const (
	codeAddress       = 2003
	codeNoAccount     = 2008
	codeSession       = 2010
	codeWrong         = 2018
	codeStale         = 3006
	codeNoAgreement   = 3009
	codeElsewhere     = 3030
	codeNoAccountHere = 3039
	codeTooOften      = 9002
)

// The errors a Client hands back, to test with errors.Is. None carries
// the address, the code or a credential.
var (
	// ErrAccount: no region of Roborock's knows an account by the
	// address.
	ErrAccount = errors.New("no roborock account by that address")
	// ErrTooOften: codes were asked for too often.
	ErrTooOften = errors.New("roborock was asked for a code too often")
	// ErrCredentials: Roborock refused the code, or takes the session no
	// more.
	ErrCredentials = errors.New("roborock refused the code or the session")
	// ErrAgreement: the account has not accepted Roborock's user
	// agreement as it stands, which is done in Roborock's app; or it is
	// an account of the Mi Home app, which this API does not serve.
	ErrAgreement = errors.New("roborock wants its user agreement accepted in its app")
	// ErrRefused: any other answer that is not a success. The error
	// carries the code Roborock refused with.
	ErrRefused = errors.New("roborock refused the request")
	// ErrAnswer: an answer is not in the shape the open implementations
	// read, is larger than an answer may be, or sends the client on to
	// somewhere that is not HTTPS.
	ErrAnswer = errors.New("roborock's answer is not in the expected shape")
	// ErrUnread: a device of the home could not be read; Devices gives
	// the others with it.
	ErrUnread = errors.New("a device of the account could not be read")
	// ErrArgument: something that cannot be sent; nothing was.
	ErrArgument = errors.New("not a valid argument")
)

// Doer makes one HTTP request; an *http.Client is one.
type Doer interface {
	// Do sends the request and returns the answer, as http.Client.Do does.
	Do(request *http.Request) (*http.Response, error)
}

// Options shape a Client.
type Options struct {
	// Gateways are where an account is looked for, in order: the login
	// servers of Roborock's regions. Empty is all four: the United
	// States', Europe's, China's and Russia's. Each is an HTTPS URL.
	Gateways []string
	// HTTP is what every request goes through; nil is a client of this
	// package's own, that follows no redirect and has a transport of its
	// own. A redirect followed would carry the address and the code to
	// wherever it points: a Doer given is to follow none either. And
	// Roborock's login servers speak TLS 1.2 and nothing later: a Doer
	// given is to speak it.
	HTTP Doer
	// Install names this installation to Roborock, as its app names a
	// phone: any text, the same from one login to the next. Empty is one
	// drawn at random for the Client.
	Install string
}

// Client is the API. Its methods may be called from several goroutines.
type Client struct {
	gateways []string
	http     Doer
	install  string
}

// New is a client. Nothing is sent until a method is called.
func New(opts Options) *Client {
	client := &Client{gateways: opts.Gateways, http: opts.HTTP, install: opts.Install}

	if len(client.gateways) == 0 {
		client.gateways = []string{
			"https://usiot.roborock.com",
			"https://euiot.roborock.com",
			"https://cniot.roborock.com",
			"https://ruiot.roborock.com",
		}
	}

	if client.http == nil {
		const handshake = 10 * time.Second

		client.http = &http.Client{
			// Not the process's default transport: a program that has
			// narrowed that one to TLS 1.3 would reach no login server.
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout: handshake,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}

	if client.install == "" {
		client.install = rand.Text()
	}

	return client
}

// Challenge is a code asked for: Roborock has sent it to the account's
// address, and Answer takes it. Formatted or logged, it shows nothing of
// the account.
type Challenge struct {
	email   string
	base    string
	client  string
	country string
	prefix  string
}

// Format keeps the challenge out of a formatted value, whatever the verb.
func (Challenge) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "roborock account challenge")
}

// LogValue keeps it out of a log record.
func (Challenge) LogValue() slog.Value { return slog.StringValue("roborock account challenge") }

// Session is an account logged in to. It is a secret: formatted or
// logged, it shows nothing of itself.
type Session struct {
	base   string
	client string
	token  string
	api    string
	mac    string
	cloud  roborock.Cloud
}

// Format keeps the session out of a formatted value, whatever the verb.
func (Session) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "roborock account session")
}

// LogValue keeps it out of a log record.
func (Session) LogValue() slog.Value { return slog.StringValue("roborock account session") }

// Cloud is the account's way into Roborock's cloud: what its devices are
// reached with, beside each one's identity. Roborock issues another with
// every login.
func (s Session) Cloud() roborock.Cloud { return s.cloud }

// Device is one device of an account.
type Device struct {
	// Name is what its owner called it.
	Name string
	// Firmware is the version of what it runs. Empty when the account
	// does not say.
	Firmware string
	// Shared says the device is another account's, shared with this one.
	Shared bool
	// Identity is what names it and opens its messages.
	Identity roborock.Identity
}

// grant is a login as the API gives it.
type grant struct {
	Token string `json:"token"`
	Cloud access `json:"rriot"`
}

// access is the part of a login that is the cloud's.
type access struct {
	User   string `json:"u"`
	Secret string `json:"s"`
	Mac    string `json:"h"`
	Key    string `json:"k"`
	At     places `json:"r"`
}

// places is where the cloud is.
type places struct {
	API    string `json:"a"`
	Broker string `json:"m"`
}

// home is a home as the cloud's API gives it.
type home struct {
	Devices  []record  `json:"devices"`
	Received []record  `json:"receivedDevices"`
	Products []product `json:"products"`
}

// record is one device as the home gives it.
type record struct {
	ID       string `json:"duid"`
	Name     string `json:"name"`
	Key      string `json:"localKey"`
	Product  string `json:"productId"`
	Protocol string `json:"pv"`
	Firmware string `json:"fv"`
}

// product is what devices are of.
type product struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

// loose is a value the API writes as a number at times and as text at
// others, or leaves null.
type loose string

// UnmarshalJSON reads a number, a text or a null.
func (l *loose) UnmarshalJSON(raw []byte) error {
	var text *string

	if json.Unmarshal(raw, &text) == nil {
		*l = ""

		if text != nil {
			*l = loose(*text)
		}

		return nil
	}

	var number json.Number

	if err := json.Unmarshal(raw, &number); err != nil {
		return fmt.Errorf("neither a number nor a text: %w", err)
	}

	*l = loose(number.String())

	return nil
}

// Ask finds the region the account is in and has Roborock send a one-time
// code to the account's address; it returns what Answer needs with it.
// Every call sends a code, and Roborock refuses to send them too often:
// ErrTooOften. An address no region knows is ErrAccount.
//
// A region that does not answer does not hide the account in another:
// the next is asked. When none then knows the account, the error is that
// of the first region that did not answer, since the account may be its.
func (c *Client) Ask(ctx context.Context, email string) (Challenge, error) {
	if email == "" {
		return Challenge{}, fmt.Errorf("%w: an empty address", ErrArgument)
	}

	sum := md5.Sum([]byte(email + c.install)) // #nosec G401 -- how the API names a client, no secret kept by it

	challenge := Challenge{email: email, client: base64.StdEncoding.EncodeToString(sum[:])}

	var (
		unasked error
		silent  int
	)

	for _, gateway := range c.gateways {
		known, refused, failed := c.region(ctx, gateway, &challenge)

		switch {
		case refused != nil:
			return Challenge{}, refused
		case failed != nil && ctx.Err() != nil:
			return Challenge{}, failed
		case failed != nil:
			silent++

			if unasked == nil {
				unasked = failed
			}

			continue
		case !known:
			continue
		}

		elsewhere, err := c.mail(ctx, challenge)
		if err != nil {
			return Challenge{}, err
		}

		if !elsewhere {
			return challenge, nil
		}
	}

	if unasked != nil {
		return Challenge{}, fmt.Errorf("%d of the %d regions did not answer, and no other knows the account: %w",
			silent, len(c.gateways), unasked)
	}

	return Challenge{}, fmt.Errorf("%w: asked in %d regions", ErrAccount, len(c.gateways))
}

// Answer logs in to the account with the code Roborock sent for the
// challenge. A code Roborock refuses is ErrCredentials; an account that
// has not accepted the user agreement in Roborock's app is ErrAgreement.
func (c *Client) Answer(ctx context.Context, challenge Challenge, code string) (Session, error) {
	if challenge.base == "" || code == "" {
		return Session{}, fmt.Errorf("%w: a challenge and a code are both needed", ErrArgument)
	}

	// The login is taken with a text of the client's own and what the API
	// signs it as.
	drawn := rand.Text()[:16]

	var signature struct {
		Key string `json:"k"`
	}

	status, err := c.gateway(ctx, call{
		method:  http.MethodPost,
		base:    challenge.base,
		path:    pathSign,
		query:   url.Values{"s": {drawn}},
		headers: map[string]string{headerClient: challenge.client},
	}, &signature)
	if err != nil {
		return Session{}, err
	}

	if status != taken {
		return Session{}, refusal(http.MethodPost, pathSign, status)
	}

	if signature.Key == "" {
		return Session{}, fmt.Errorf("%w: POST %s gave no signature", ErrAnswer, pathSign)
	}

	major, minor := c.agreement(ctx, challenge)

	var granted grant

	status, err = c.gateway(ctx, call{
		method: http.MethodPost,
		base:   challenge.base,
		path:   pathLogin,
		form: url.Values{
			"country":      {challenge.country},
			"countryCode":  {challenge.prefix},
			fieldEmail:     {challenge.email},
			"code":         {code},
			"majorVersion": {strconv.Itoa(major)},
			"minorVersion": {strconv.Itoa(minor)},
		},
		// The API takes this login from its app: these are what the open
		// implementations say an app is.
		headers: map[string]string{
			headerClient:         challenge.client,
			headerLanguage:       language,
			"header_appversion":  "4.54.02",
			"header_phonesystem": "iOS",
			"header_phonemodel":  "iPhone16,1",
			"x-mercy-ks":         drawn,
			"x-mercy-k":          signature.Key,
		},
	}, &granted)
	if err != nil {
		return Session{}, err
	}

	if status != taken {
		return Session{}, refusal(http.MethodPost, pathLogin, status)
	}

	session := Session{
		base:   challenge.base,
		client: challenge.client,
		token:  granted.Token,
		api:    granted.Cloud.At.API,
		mac:    granted.Cloud.Mac,
		cloud: roborock.Cloud{
			Broker: granted.Cloud.At.Broker,
			User:   granted.Cloud.User,
			Secret: granted.Cloud.Secret,
			Key:    granted.Cloud.Key,
		},
	}

	if slices.Contains([]string{
		session.token, session.api, session.mac,
		session.cloud.Broker, session.cloud.User, session.cloud.Secret, session.cloud.Key,
	}, "") {
		return Session{}, fmt.Errorf("%w: POST %s gave a login with a part missing", ErrAnswer, pathLogin)
	}

	return session, nil
}

var plain = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// Devices is the devices of the account's home, its own first and then
// those shared with it, each in the order Roborock lists them.
//
// A device with no id or no key cannot be read: it is left out, and the
// error is then ErrUnread, with the devices that could be read returned
// beside it. One device Roborock writes otherwise does not hide the
// others.
func (c *Client) Devices(ctx context.Context, session Session) ([]Device, error) {
	if session.token == "" {
		return nil, fmt.Errorf("%w: no session", ErrArgument)
	}

	held, err := c.home(ctx, session)
	if err != nil {
		return nil, err
	}

	models := map[string]string{}
	for _, product := range held.Products {
		models[product.ID] = product.Model
	}

	devices := make([]Device, 0, len(held.Devices)+len(held.Received))
	unread := 0

	for index, listed := range [][]record{held.Devices, held.Received} {
		for _, one := range listed {
			if one.ID == "" || one.Key == "" {
				unread++

				continue
			}

			devices = append(devices, Device{
				Name:     one.Name,
				Firmware: one.Firmware,
				Shared:   index == 1,
				Identity: roborock.Identity{
					ID:       one.ID,
					Protocol: one.Protocol,
					Model:    models[one.Product],
					Key:      one.Key,
				},
			})
		}
	}

	if unread > 0 {
		return devices, fmt.Errorf("%w: %d of the %d the home has", ErrUnread, unread, unread+len(devices))
	}

	return devices, nil
}

// region asks one gateway where the account is, and writes it into the
// challenge. A gateway that does not know the account says so with no
// country. One that answers with a refusal is told apart from one that
// could not be asked, or whose answer could not be read.
func (c *Client) region(
	ctx context.Context, gateway string, challenge *Challenge,
) (known bool, refused, unasked error) {
	var where struct {
		URL     string `json:"url"`
		Country loose  `json:"country"`
		Prefix  loose  `json:"countrycode"`
	}

	code, err := c.gateway(ctx, call{
		method: http.MethodPost,
		base:   gateway,
		path:   pathRegion,
		query:  url.Values{fieldEmail: {challenge.email}, "needtwostepauth": {"false"}},
	}, &where)
	if err != nil {
		return false, nil, err
	}

	if code != taken {
		return false, refusal(http.MethodPost, pathRegion, code), nil
	}

	if where.Country == "" || where.Prefix == "" {
		return false, nil, nil
	}

	challenge.base = where.URL
	challenge.country = string(where.Country)
	challenge.prefix = string(where.Prefix)

	return true, nil, nil
}

// mail has the challenge's region send the code. A region that says it
// knows the account may still refuse to, as another region's: elsewhere.
func (c *Client) mail(ctx context.Context, challenge Challenge) (elsewhere bool, err error) {
	code, err := c.gateway(ctx, call{
		method: http.MethodPost,
		base:   challenge.base,
		path:   pathCode,
		form:   url.Values{fieldEmail: {challenge.email}, "type": {"login"}, "platform": {""}},
		headers: map[string]string{
			headerClient:   challenge.client,
			headerLanguage: language,
		},
	}, nil)
	if err != nil {
		return false, err
	}

	switch code {
	case taken:
		return false, nil
	case codeElsewhere:
		return true, nil
	default:
		return false, refusal(http.MethodPost, pathCode, code)
	}
}

// agreement is the version of the user agreement a login is to name: the
// latest for the account's country. Roborock refuses a login that names a
// stale one; when it does not say which is the latest, the one the open
// implementations fall back on is named.
func (c *Client) agreement(ctx context.Context, challenge Challenge) (major, minor int) {
	var latest struct {
		Major *int `json:"majorVersion"`
		Minor *int `json:"minorVersion"`
	}

	code, err := c.gateway(ctx, call{
		method:  http.MethodGet,
		base:    challenge.base,
		path:    pathAgreement,
		query:   url.Values{"country": {challenge.country}},
		headers: map[string]string{headerLanguage: language},
	}, &latest)
	if err != nil || code != taken || latest.Major == nil || latest.Minor == nil {
		// This request failing does not stop a login.
		return agreementMajor, agreementMinor
	}

	return *latest.Major, *latest.Minor
}

// home asks the login server which home is the account's, and the
// cloud's API for it.
func (c *Client) home(ctx context.Context, session Session) (home, error) {
	var detail struct {
		Home loose `json:"rrHomeId"`
	}

	code, err := c.gateway(ctx, call{
		method:  http.MethodGet,
		base:    session.base,
		path:    pathHome,
		headers: map[string]string{headerClient: session.client, "Authorization": session.token},
	}, &detail)
	if err != nil {
		return home{}, err
	}

	if code != taken {
		return home{}, refusal(http.MethodGet, pathHome, code)
	}

	// It goes into a path.
	if !plain.MatchString(string(detail.Home)) {
		return home{}, fmt.Errorf("%w: GET %s gave no home", ErrAnswer, pathHome)
	}

	var answer struct {
		Success bool  `json:"success"`
		Code    loose `json:"code"`
		Result  *home `json:"result"`
	}

	path := pathDevices + string(detail.Home)

	if err = c.exchange(ctx, call{
		method:  http.MethodGet,
		base:    session.api,
		path:    path,
		headers: map[string]string{"Authorization": session.signature(path, time.Now())},
	}, &answer); err != nil {
		return home{}, err
	}

	if !answer.Success {
		return home{}, fmt.Errorf("%w: GET %s: code %q", ErrRefused, pathDevices, bounded(string(answer.Code)))
	}

	if answer.Result == nil {
		return home{}, fmt.Errorf("%w: GET %s gave no home", ErrAnswer, pathDevices)
	}

	return *answer.Result, nil
}

// signature is the Authorization of one request for a path of the cloud's
// API that has neither a query nor a body: the place of each stays empty
// in what is signed, and the digest of nothing there is refused.
func (s Session) signature(path string, now time.Time) string {
	stamp := strconv.FormatInt(now.Unix(), 10)
	nonce := rand.Text()[:8]
	where := md5.Sum([]byte(path)) // #nosec G401 -- how the API is signed, no choice of this package

	signed := hmac.New(sha256.New, []byte(s.mac))

	_, _ = io.WriteString(signed, strings.Join([]string{
		s.cloud.User, s.cloud.Secret, nonce, stamp, hex.EncodeToString(where[:]), "", "",
	}, ":"))

	//nolint:gocritic // sprintfQuotedString: the API's own quoting, which escapes nothing
	return fmt.Sprintf(`Hawk id="%s",s="%s",ts="%s",nonce="%s",mac="%s"`,
		s.cloud.User, s.cloud.Secret, stamp, nonce, base64.StdEncoding.EncodeToString(signed.Sum(nil)))
}

// refusal is the error for a code no route expects, under the error the
// code stands for when it is one that is told apart.
func refusal(method, path string, code int) error {
	kind := ErrRefused

	switch code {
	case codeAddress:
		kind = ErrArgument
	case codeNoAccount, codeNoAccountHere:
		kind = ErrAccount
	case codeSession, codeWrong:
		kind = ErrCredentials
	case codeStale, codeNoAgreement:
		kind = ErrAgreement
	case codeTooOften:
		kind = ErrTooOften
	}

	return fmt.Errorf("%w: %s %s: code %d", kind, method, path, code)
}

// bounded is a text of the API's cut to what an error may carry of it.
func bounded(text string) string {
	const most = 32

	if len(text) > most {
		return text[:most]
	}

	return text
}

// call is one request.
type call struct {
	method  string
	base    string
	path    string
	query   url.Values
	form    url.Values
	headers map[string]string
}

// gateway sends one request to a login server and returns the code it
// was answered with; the data of a request taken is decoded into the
// value given, when one is.
func (c *Client) gateway(ctx context.Context, request call, into any) (int, error) {
	var answer struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}

	if err := c.exchange(ctx, request, &answer); err != nil {
		return 0, err
	}

	if answer.Code == nil {
		return 0, fmt.Errorf("%w: %s %s gave no code", ErrAnswer, request.method, request.path)
	}

	if *answer.Code == taken && into != nil {
		if json.Unmarshal(answer.Data, into) != nil {
			return 0, undecoded(request)
		}
	}

	return *answer.Code, nil
}

// undecoded is the error for an answer that is not the JSON expected. The
// decoder's own error may quote the answer, which may hold a token or a
// key: it is not handed on.
func undecoded(request call) error {
	return fmt.Errorf("%w: %s %s is not the JSON expected", ErrAnswer, request.method, request.path)
}

// exchange sends one request, to HTTPS only, and decodes the answer into
// the value given. A 401 or a 403 is ErrCredentials and a 429 ErrTooOften,
// whatever the route.
func (c *Client) exchange(ctx context.Context, request call, into any) error {
	route := request.method + " " + request.path

	target, err := url.Parse(request.base)
	if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil {
		// Where the API sends the client is not said: it is a stranger's
		// text, and may be long.
		return fmt.Errorf("%w: %s is not asked of an HTTPS address", ErrAnswer, route)
	}

	// Which server was asked is said, cut to what a host's name may be.
	const longest = 64

	route = request.method + " " + target.Host[:min(len(target.Host), longest)] + request.path

	target = target.JoinPath(request.path)
	target.RawQuery = request.query.Encode()

	var body io.Reader = http.NoBody
	if request.form != nil {
		body = strings.NewReader(request.form.Encode())
	}

	sent, err := http.NewRequestWithContext(ctx, request.method, target.String(), body)
	if err != nil {
		return fmt.Errorf("%w: %s cannot be sent", ErrArgument, route)
	}

	sent.Header.Set("Accept", "application/json")

	if request.form != nil {
		sent.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	for name, value := range request.headers {
		// Set would rewrite the API's own names.
		sent.Header[name] = []string{value}
	}

	response, err := c.http.Do(sent)
	if err != nil {
		// The client's error names the URL, whose query may hold the
		// account's address: what failed is handed on without it.
		if located, is := errors.AsType[*url.Error](err); is {
			err = located.Err
		}

		return fmt.Errorf("%s: %w", route, err)
	}

	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAnswer+1))
	if err != nil {
		return fmt.Errorf("%s: reading the answer: %w", route, err)
	}

	switch {
	case len(raw) > maxAnswer:
		return fmt.Errorf("%w: %s: over %d bytes", ErrAnswer, route, maxAnswer)
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s: HTTP %d", ErrCredentials, route, response.StatusCode)
	case response.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s: HTTP %d", ErrTooOften, route, response.StatusCode)
	case response.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: %s: HTTP %d", ErrRefused, route, response.StatusCode)
	}

	if json.Unmarshal(raw, into) != nil {
		return undecoded(request)
	}

	return nil
}
