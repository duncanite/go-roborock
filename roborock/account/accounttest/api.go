// Package accounttest is Roborock's account API for tests: the login
// servers of a region and the cloud's API behind them, in one, answering
// as the open implementations read them, with an account, a home and
// devices of its own.
package accounttest

import (
	"crypto/hmac"
	"crypto/md5" // #nosec G501 -- the digest the real API is signed with
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The account the API holds, the code it sends, and the login it gives.
const (
	Email   = "owner@example.test"
	Code    = "123456"
	Country = "US"
	Prefix  = "1"
	Home    = "424242"

	Token  = "account-token" // #nosec G101 -- the fake's own, of no account
	User   = "cloud-user"
	Secret = "cloud-secret" // #nosec G101 -- the fake's own, of no account
	Mac    = "cloud-mac"
	Key    = "cloud-key"
	Broker = "ssl://broker.example.test:8883"

	// Major and Minor are the version of the user agreement the API
	// calls the latest; Stale are the ones a client names when it was
	// not told.
	Major      = 17
	Minor      = 3
	StaleMajor = 14
	StaleMinor = 0
)

// The routes, as Refuse and Raw name them.
const (
	Region    = "/api/v1/getUrlByEmail"
	SendCode  = "/api/v4/email/code/send"
	Sign      = "/api/v3/key/sign"
	Agreement = "/api/v3/app/agreement/latest"
	Login     = "/api/v4/auth/email/login/code"
	Detail    = "/api/v1/getHomeDetail"
	Devices   = "/v3/user/homes/" + Home
)

// Device is one device of the home, as the test says it.
type Device struct {
	ID       string
	Name     string
	Key      string
	Product  string
	Protocol string
	Firmware string
}

// Product is one product the home's devices are of.
type Product struct {
	ID    string
	Model string
}

// Request is one request the API received.
type Request struct {
	Method        string
	Path          string
	Query         string
	Body          string
	Client        string
	Authorization string
}

// API is the fake, serving until Close. What a test sets is set before
// the client asks.
type API struct {
	*httptest.Server

	mu sync.Mutex
	// Unknown has this region not know the account.
	Unknown bool
	// Onward, when not empty, is where the region sends the client on
	// to, in place of this API.
	Onward string
	// Install, when not empty, is what the client is to have named its
	// installation: the API then refuses any other client id.
	Install string
	// Refuse is the code a route refuses with, by its path.
	Refuse map[string]int
	// Raw is what a route answers in place of its own answer, by its
	// path.
	Raw map[string]string
	// Fail, when not zero, is the HTTP status every route answers with.
	Fail int
	// NoAgreement has the API not say which user agreement is the
	// latest, and take the stale one.
	NoAgreement bool
	// Numbers has the API write as numbers what it otherwise writes as
	// text: the country's prefix and the home.
	Numbers bool
	// Own and Received are the home's devices, and Products what they
	// are.
	Own      []Device
	Received []Device
	Products []Product

	drawn    string
	requests []Request
}

// New is an API with one robot of its own in the home, and one shared
// with it.
func New() *API {
	api := &API{
		Own: []Device{{
			ID: "duid-own", Name: "Downstairs", Key: "key-of-the-own", Product: "product-q7",
			Protocol: "B01", Firmware: "02.10.20",
		}},
		Received: []Device{{
			ID: "duid-shared", Name: "Garage", Key: "key-of-the-shared", Product: "product-s7",
			Protocol: "1.0", Firmware: "02.56.02",
		}},
		Products: []Product{
			{ID: "product-q7", Model: "roborock.vacuum.sc01"},
			{ID: "product-s7", Model: "roborock.vacuum.a15"},
		},
	}

	api.Server = httptest.NewTLSServer(http.HandlerFunc(api.serve))

	return api
}

// Browser is what reaches the API: a client that trusts it, and follows
// no redirect.
func (a *API) Browser() *http.Client {
	client := a.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return client
}

// Requests is every request received so far, in order.
func (a *API) Requests() []Request {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]Request(nil), a.requests...)
}

// ClientID is what a client that names its installation so calls itself,
// for the account.
func ClientID(install string) string {
	sum := md5.Sum([]byte(Email + install)) // #nosec G401 -- as the real API has it

	return base64.StdEncoding.EncodeToString(sum[:])
}

// Signature is the signature the cloud's API takes for a request of a
// path that has neither a query nor a body.
func Signature(user, secret, mac, nonce, stamp, path string) string {
	where := md5.Sum([]byte(path)) // #nosec G401 -- as the real API has it

	signed := hmac.New(sha256.New, []byte(mac))

	_, _ = io.WriteString(signed, strings.Join([]string{
		user, secret, nonce, stamp, hex.EncodeToString(where[:]), "", "",
	}, ":"))

	return base64.StdEncoding.EncodeToString(signed.Sum(nil))
}

func (a *API) serve(writer http.ResponseWriter, request *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))

	a.requests = append(a.requests, Request{
		Method:        request.Method,
		Path:          request.URL.Path,
		Query:         request.URL.RawQuery,
		Body:          string(body),
		Client:        header(request, "header_clientid"),
		Authorization: request.Header.Get("Authorization"),
	})

	if a.Fail != 0 {
		writer.WriteHeader(a.Fail)

		return
	}

	if raw, has := a.Raw[request.URL.Path]; has {
		_, _ = io.WriteString(writer, raw)

		return
	}

	if request.URL.Path == Devices {
		a.devices(writer, request)

		return
	}

	if code, has := a.Refuse[request.URL.Path]; has {
		envelope(writer, code, nil)

		return
	}

	form := values(string(body))

	switch request.Method + " " + request.URL.Path {
	case "POST " + Region:
		a.region(writer, request)
	case "POST " + SendCode:
		a.send(writer, request, form)
	case "POST " + Sign:
		a.sign(writer, request)
	case "GET " + Agreement:
		a.agreement(writer, request)
	case "POST " + Login:
		a.login(writer, request, form)
	case "GET " + Detail:
		a.detail(writer, request)
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

// header is a header by the name the client wrote it under, which is not
// the canonical one.
func header(request *http.Request, name string) string {
	for written, held := range request.Header {
		if strings.EqualFold(written, name) && len(held) > 0 {
			return held[0]
		}
	}

	return ""
}

// values is a form's fields, the first of each.
func values(body string) map[string]string {
	fields := map[string]string{}

	parsed, err := url.ParseQuery(body)
	if err != nil {
		return fields
	}

	for name, held := range parsed {
		fields[name] = held[0]
	}

	return fields
}

// The codes the API refuses with.
const (
	taken      = 200
	missing    = 1001
	noAccount  = 2008
	noSession  = 2010
	wrongCode  = 2018
	stale      = 3006
	badHeaders = 4001
)

func envelope(writer http.ResponseWriter, code int, data any) {
	writer.Header().Set("Content-Type", "application/json")

	_ = json.NewEncoder(writer).Encode(map[string]any{"code": code, "msg": "said by the fake", "data": data})
}

// named says the request carries the client id the API expects: any, and
// the one the installation named makes when the test said which.
func (a *API) named(request *http.Request) bool {
	client := header(request, "header_clientid")

	return client != "" && (a.Install == "" || client == ClientID(a.Install))
}

func (a *API) region(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()

	if query.Get("email") == "" || query.Get("needtwostepauth") != "false" {
		envelope(writer, missing, nil)

		return
	}

	onward := a.URL
	if a.Onward != "" {
		onward = a.Onward
	}

	where := map[string]any{"url": onward, "country": nil, "countrycode": nil}

	if !a.Unknown && query.Get("email") == Email {
		where["country"], where["countrycode"] = Country, Prefix

		if a.Numbers {
			where["countrycode"], _ = strconv.Atoi(Prefix)
		}
	}

	envelope(writer, taken, where)
}

func (a *API) send(writer http.ResponseWriter, request *http.Request, form map[string]string) {
	_, platform := form["platform"]

	switch {
	case !a.named(request) || header(request, "header_clientlang") != "en" ||
		request.Header.Get("Content-Type") != "application/x-www-form-urlencoded":
		envelope(writer, badHeaders, nil)
	case form["type"] != "login" || !platform || form["platform"] != "":
		envelope(writer, missing, nil)
	case form["email"] != Email:
		envelope(writer, noAccount, nil)
	default:
		envelope(writer, taken, nil)
	}
}

var drawn = regexp.MustCompile(`^[A-Za-z0-9]{16}$`)

func (a *API) sign(writer http.ResponseWriter, request *http.Request) {
	text := request.URL.Query().Get("s")

	if !a.named(request) || !drawn.MatchString(text) {
		envelope(writer, badHeaders, nil)

		return
	}

	a.drawn = text

	envelope(writer, taken, map[string]any{"k": "signed:" + text})
}

func (a *API) agreement(writer http.ResponseWriter, request *http.Request) {
	if a.NoAgreement || request.URL.Query().Get("country") != Country || header(request, "header_clientlang") != "en" {
		envelope(writer, missing, nil)

		return
	}

	envelope(writer, taken, map[string]any{"majorVersion": Major, "minorVersion": Minor})
}

func (a *API) login(writer http.ResponseWriter, request *http.Request, form map[string]string) {
	major, minor := strconv.Itoa(Major), strconv.Itoa(Minor)
	if a.NoAgreement {
		major, minor = strconv.Itoa(StaleMajor), strconv.Itoa(StaleMinor)
	}

	switch {
	case !a.named(request) || a.drawn == "" ||
		header(request, "x-mercy-ks") != a.drawn || header(request, "x-mercy-k") != "signed:"+a.drawn ||
		header(request, "header_clientlang") != "en" || header(request, "header_appversion") == "" ||
		header(request, "header_phonesystem") == "" || header(request, "header_phonemodel") == "" ||
		request.Header.Get("Content-Type") != "application/x-www-form-urlencoded":
		envelope(writer, badHeaders, nil)
	case form["email"] != Email || form["country"] != Country || form["countryCode"] != Prefix:
		envelope(writer, noAccount, nil)
	case form["code"] != Code:
		envelope(writer, wrongCode, nil)
	case form["majorVersion"] != major || form["minorVersion"] != minor:
		envelope(writer, stale, nil)
	default:
		envelope(writer, taken, map[string]any{
			"token": Token,
			"rriot": map[string]any{
				"u": User, "s": Secret, "h": Mac, "k": Key,
				"r": map[string]any{"r": "US", "a": a.URL, "m": Broker, "l": "https://wood.example.test"},
			},
		})
	}
}

func (a *API) detail(writer http.ResponseWriter, request *http.Request) {
	switch {
	case !a.named(request) || request.Header.Get("Authorization") != Token:
		envelope(writer, noSession, nil)
	case a.Numbers:
		home, _ := strconv.Atoi(Home)

		envelope(writer, taken, map[string]any{"rrHomeId": home})
	default:
		envelope(writer, taken, map[string]any{"rrHomeId": Home})
	}
}

var hawk = regexp.MustCompile(`^Hawk id="([^"]*)",s="([^"]*)",ts="([0-9]+)",nonce="([^"]+)",mac="([^"]*)"$`)

// devices is the home, for a request signed as the cloud's API has it.
func (a *API) devices(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")

	parts := hawk.FindStringSubmatch(request.Header.Get("Authorization"))

	if request.Method != http.MethodGet || parts == nil || parts[1] != User || parts[2] != Secret ||
		parts[5] != Signature(User, Secret, Mac, parts[4], parts[3], request.URL.Path) {
		writer.WriteHeader(http.StatusUnauthorized)

		return
	}

	if code, has := a.Refuse[Devices]; has {
		_, _ = fmt.Fprintf(writer, `{"success":false,"code":%d,"msg":"said by the fake"}`, code)

		return
	}

	listed := func(devices []Device) []map[string]any {
		out := make([]map[string]any, 0, len(devices))

		for _, device := range devices {
			out = append(out, map[string]any{
				"duid": device.ID, "name": device.Name, "localKey": device.Key, "productId": device.Product,
				"pv": device.Protocol, "fv": device.Firmware, "online": true,
			})
		}

		return out
	}

	products := make([]map[string]any, 0, len(a.Products))
	for _, product := range a.Products {
		products = append(products, map[string]any{
			"id": product.ID, "model": product.Model, "category": "robot.vacuum.cleaner",
		})
	}

	_ = json.NewEncoder(writer).Encode(map[string]any{
		"success": true,
		"result": map[string]any{
			"id":      1,
			"devices": listed(a.Own), "receivedDevices": listed(a.Received), "products": products,
			"rooms": []map[string]any{{"id": 1}},
		},
	})
}
