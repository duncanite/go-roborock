package account_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/duncanite/go-roborock/roborock"
	"github.com/duncanite/go-roborock/roborock/account"
	"github.com/duncanite/go-roborock/roborock/account/accounttest"
)

// served is an API and a client that asks it, and it alone.
func served(t *testing.T) (*accounttest.API, *account.Client) {
	t.Helper()

	api := accounttest.New()
	t.Cleanup(api.Close)

	return api, account.New(account.Options{Gateways: []string{api.URL}, HTTP: api.Browser()})
}

// loggedIn is a session of the API's account.
func loggedIn(t *testing.T, client *account.Client) account.Session {
	t.Helper()

	challenge, err := client.Ask(t.Context(), accounttest.Email)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	session, err := client.Answer(t.Context(), challenge, accounttest.Code)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	return session
}

// TestLogin: a code asked for and answered gives the account's way into
// the cloud, as the login wrote it, and the home's devices: its own and
// then those shared with it, each with its key and the model its product
// is.
func TestLogin(t *testing.T) {
	t.Parallel()

	for _, numbers := range []bool{false, true} {
		t.Run(fmt.Sprintf("numbers written as numbers: %t", numbers), func(t *testing.T) {
			t.Parallel()

			api, client := served(t)
			api.Numbers = numbers

			session := loggedIn(t, client)

			cloud := roborock.Cloud{
				Broker: accounttest.Broker, User: accounttest.User, Secret: accounttest.Secret, Key: accounttest.Key,
			}
			if got := session.Cloud(); !reflect.DeepEqual(got, cloud) {
				t.Errorf("Cloud = %#v, want %#v", struct{ roborock.Cloud }{got}, struct{ roborock.Cloud }{cloud})
			}

			devices, err := client.Devices(t.Context(), session)
			if err != nil {
				t.Fatalf("Devices: %v", err)
			}

			want := []account.Device{
				{
					Name: "Downstairs", Firmware: "02.10.20",
					Identity: roborock.Identity{
						ID: "duid-own", Protocol: "B01", Model: "roborock.vacuum.sc01", Key: "key-of-the-own",
					},
				},
				{
					Name: "Garage", Firmware: "02.56.02", Shared: true,
					Identity: roborock.Identity{
						ID: "duid-shared", Protocol: "1.0", Model: "roborock.vacuum.a15", Key: "key-of-the-shared",
					},
				},
			}

			if !reflect.DeepEqual(devices, want) {
				t.Errorf("Devices = %d of them, not the home's two as they are", len(devices))
			}
		})
	}
}

// TestWhatIsSent: each request is the one the open implementations send.
// The address is asked in a query, the code is asked for and answered in
// a form, the client is named the same throughout, as its installation
// makes it, and the home is asked for under a signature of its own path.
func TestWhatIsSent(t *testing.T) {
	t.Parallel()

	api := accounttest.New()
	t.Cleanup(api.Close)

	api.Install = "install-one"

	client := account.New(account.Options{
		Gateways: []string{api.URL}, HTTP: api.Browser(), Install: "install-one",
	})

	if _, err := client.Devices(t.Context(), loggedIn(t, client)); err != nil {
		t.Fatalf("Devices: %v", err)
	}

	// Computed apart, from the formula the reference implementation has.
	const named = "BUIF5aoR3H+LP6QuHIbKIg=="

	if got := accounttest.ClientID("install-one"); got != named {
		t.Fatalf("the API's client id for the installation = %q, want %q", got, named)
	}

	requests := api.Requests()

	want := []string{
		"POST " + accounttest.Region, "POST " + accounttest.SendCode, "POST " + accounttest.Sign,
		"GET " + accounttest.Agreement, "POST " + accounttest.Login, "GET " + accounttest.Detail,
		"GET " + accounttest.Devices,
	}

	got := make([]string, 0, len(requests))
	for _, request := range requests {
		got = append(got, request.Method+" "+request.Path)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %q, want %q", got, want)
	}

	if query, _ := url.ParseQuery(requests[0].Query); query.Get("email") != accounttest.Email ||
		query.Get("needtwostepauth") != "false" || requests[0].Body != "" {
		t.Errorf("the region is asked with %q and a body of %d bytes", requests[0].Query, len(requests[0].Body))
	}

	if form, _ := url.ParseQuery(requests[1].Body); form.Get("email") != accounttest.Email ||
		form.Get("type") != "login" || !form.Has("platform") || requests[1].Query != "" {
		t.Errorf("the code is asked for with the form %q", requests[1].Body)
	}

	form, _ := url.ParseQuery(requests[4].Body)

	for field, value := range map[string]string{
		"country": accounttest.Country, "countryCode": accounttest.Prefix, "email": accounttest.Email,
		"code": accounttest.Code, "majorVersion": "17", "minorVersion": "3",
	} {
		if form.Get(field) != value {
			t.Errorf("the login's %s = %q, want %q", field, form.Get(field), value)
		}
	}

	if strings.Contains(requests[4].Query, accounttest.Code) {
		t.Error("the code is in the login's query")
	}

	for index, request := range requests[:6] {
		// Neither the region nor the agreement is asked under a name.
		if anonymous := index == 0 || index == 3; anonymous != (request.Client == "") ||
			(!anonymous && request.Client != named) {
			t.Errorf("%s is asked as the client %q", request.Path, request.Client)
		}
	}

	if requests[5].Authorization != accounttest.Token {
		t.Errorf("the home's id is asked under %q, want the token alone", requests[5].Authorization)
	}

	signed := regexp.MustCompile(
		`^Hawk id="cloud-user",s="cloud-secret",ts="[0-9]{10}",nonce="[A-Z2-7]{8}",mac="[A-Za-z0-9+/]{43}="$`,
	)
	if !signed.MatchString(requests[6].Authorization) {
		t.Errorf("the home is asked under %q", requests[6].Authorization)
	}
}

// TestSignature: what the test API takes as a signature is the one the
// reference implementation's formula makes, so that a home it serves was
// asked for as Roborock wants it.
func TestSignature(t *testing.T) {
	t.Parallel()

	// Computed apart, from the formula the reference implementation has.
	const want = "AAYlXKI4pU4qL7BDThbX0LQmDDA8NJMZpJEKAOmoNpE="

	got := accounttest.Signature("user123", "pass123", "hmackey456", "AbCdEf12", "1700000000", "/v3/user/homes/123456")
	if got != want {
		t.Errorf("Signature = %q, want %q", got, want)
	}
}

// TestRegions: the account is looked for in each region in turn. One
// that does not know it is passed over, and so is one that says it knows
// it and then refuses to send the code as another region's. An account no
// region knows is ErrAccount.
func TestRegions(t *testing.T) {
	t.Parallel()

	pair := func(t *testing.T) (*accounttest.API, *accounttest.API, *account.Client) {
		t.Helper()

		first, second := accounttest.New(), accounttest.New()
		t.Cleanup(first.Close)
		t.Cleanup(second.Close)

		return first, second, account.New(account.Options{
			Gateways: []string{first.URL, second.URL}, HTTP: second.Browser(),
		})
	}

	t.Run("the first does not know the account", func(t *testing.T) {
		t.Parallel()

		first, second, client := pair(t)
		first.Unknown = true

		session := loggedIn(t, client)

		if _, err := client.Devices(t.Context(), session); err != nil {
			t.Fatalf("Devices: %v", err)
		}

		if len(first.Requests()) != 1 || len(second.Requests()) != 7 {
			t.Errorf("the first region was asked %d times and the second %d, want 1 and 7",
				len(first.Requests()), len(second.Requests()))
		}
	})

	t.Run("the first says the account is elsewhere", func(t *testing.T) {
		t.Parallel()

		first, second, client := pair(t)
		first.Refuse = map[string]int{accounttest.SendCode: 3030}

		loggedIn(t, client)

		if len(first.Requests()) != 2 || len(second.Requests()) != 5 {
			t.Errorf("the first region was asked %d times and the second %d, want 2 and 5",
				len(first.Requests()), len(second.Requests()))
		}
	})

	t.Run("the first cannot be asked", func(t *testing.T) {
		t.Parallel()

		first, second, client := pair(t)
		first.Fail = http.StatusBadGateway

		loggedIn(t, client)

		if len(first.Requests()) != 1 || len(second.Requests()) != 5 {
			t.Errorf("the first region was asked %d times and the second %d, want 1 and 5",
				len(first.Requests()), len(second.Requests()))
		}
	})

	t.Run("one cannot be asked and the other does not know the account", func(t *testing.T) {
		t.Parallel()

		for _, failing := range []int{0, 1} {
			first, second, client := pair(t)
			regions := []*accounttest.API{first, second}

			regions[failing].Fail = http.StatusBadGateway
			regions[1-failing].Unknown = true

			_, err := client.Ask(t.Context(), accounttest.Email)

			host := strings.TrimPrefix(regions[failing].URL, "https://")

			if errors.Is(err, account.ErrAccount) || !errors.Is(err, account.ErrRefused) ||
				!strings.Contains(err.Error(), host) || !strings.Contains(err.Error(), "HTTP 502") ||
				!strings.Contains(err.Error(), "1 of the 2 regions") {
				t.Errorf("Ask = %v, want the refusal of %s, which may hold the account", err, host)
			}

			if len(first.Requests()) != 1 || len(second.Requests()) != 1 {
				t.Errorf("the regions were asked %d and %d times, want once each",
					len(first.Requests()), len(second.Requests()))
			}
		}
	})

	t.Run("a region that refuses ends the search", func(t *testing.T) {
		t.Parallel()

		first, second, client := pair(t)
		first.Refuse = map[string]int{accounttest.Region: 9002}

		if _, err := client.Ask(t.Context(), accounttest.Email); !errors.Is(err, account.ErrTooOften) {
			t.Errorf("Ask = %v, want ErrTooOften", err)
		}

		if len(second.Requests()) != 0 {
			t.Error("the second region was asked after the first refused")
		}
	})

	t.Run("no region knows the account", func(t *testing.T) {
		t.Parallel()

		first, second, client := pair(t)
		first.Unknown, second.Unknown = true, true

		if _, err := client.Ask(t.Context(), accounttest.Email); !errors.Is(err, account.ErrAccount) {
			t.Errorf("Ask = %v, want ErrAccount", err)
		}

		if len(first.Requests()) != 1 || len(second.Requests()) != 1 {
			t.Error("a code was asked for of a region that does not know the account")
		}
	})
}

// TestAgreement: a login names the version of the user agreement the
// API calls the latest, and the one the open implementations fall back
// on when the API does not say.
func TestAgreement(t *testing.T) {
	t.Parallel()

	api, client := served(t)
	api.NoAgreement = true

	loggedIn(t, client)

	requests := api.Requests()
	form, _ := url.ParseQuery(requests[len(requests)-1].Body)

	if form.Get("majorVersion") != "14" || form.Get("minorVersion") != "0" {
		t.Errorf("the login names the agreement %s.%s, want 14.0", form.Get("majorVersion"), form.Get("minorVersion"))
	}
}

// TestRefusals: each code Roborock refuses with is the error it stands
// for, and any other is ErrRefused, with the code.
func TestRefusals(t *testing.T) {
	t.Parallel()

	for _, refused := range []struct {
		route string
		code  int
		want  error
	}{
		{accounttest.Region, 2003, account.ErrArgument},
		{accounttest.Region, 1001, account.ErrRefused},
		{accounttest.SendCode, 2008, account.ErrAccount},
		{accounttest.SendCode, 9002, account.ErrTooOften},
		{accounttest.SendCode, 5555, account.ErrRefused},
		{accounttest.Sign, 5555, account.ErrRefused},
		{accounttest.Login, 2018, account.ErrCredentials},
		{accounttest.Login, 3009, account.ErrAgreement},
		{accounttest.Login, 3006, account.ErrAgreement},
		{accounttest.Login, 3039, account.ErrAccount},
		{accounttest.Detail, 2010, account.ErrCredentials},
		{accounttest.Devices, 5555, account.ErrRefused},
	} {
		t.Run(fmt.Sprintf("%s refused with %d", refused.route, refused.code), func(t *testing.T) {
			t.Parallel()

			api, client := served(t)
			api.Refuse = map[string]int{refused.route: refused.code}

			err := whole(t.Context(), client, accounttest.Code)

			if !errors.Is(err, refused.want) || !strings.Contains(err.Error(), strconv.Itoa(refused.code)) {
				t.Errorf("the login = %v, want %v with the code %d", err, refused.want, refused.code)
			}
		})
	}

	t.Run("a wrong code", func(t *testing.T) {
		t.Parallel()

		_, client := served(t)

		if err := whole(t.Context(), client, "000000"); !errors.Is(err, account.ErrCredentials) {
			t.Errorf("the login with a wrong code = %v, want ErrCredentials", err)
		}
	})

	for status, want := range map[int]error{
		http.StatusUnauthorized:        account.ErrCredentials,
		http.StatusForbidden:           account.ErrCredentials,
		http.StatusTooManyRequests:     account.ErrTooOften,
		http.StatusInternalServerError: account.ErrRefused,
	} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			t.Parallel()

			api, client := served(t)
			api.Fail = status

			if err := whole(t.Context(), client, accounttest.Code); !errors.Is(err, want) {
				t.Errorf("the login = %v, want %v", err, want)
			}
		})
	}
}

// whole is a login from the address to the devices, and the first error
// of it.
func whole(ctx context.Context, client *account.Client, code string) error {
	challenge, err := client.Ask(ctx, accounttest.Email)
	if err != nil {
		return err
	}

	session, err := client.Answer(ctx, challenge, code)
	if err != nil {
		return err
	}

	_, err = client.Devices(ctx, session)

	return err
}

// TestAnswers: an answer that is not in the shape expected is ErrAnswer,
// and what it held is not said: a login missing a part, no home, no
// signature, something that is no JSON, and a region that sends the
// client on to somewhere that is not HTTPS.
func TestAnswers(t *testing.T) {
	t.Parallel()

	const secret = "a-secret-in-the-answer"

	for name, raw := range map[string]map[string]string{
		"a login with no broker": {accounttest.Login: `{"code":200,"data":{"token":"` + secret +
			`","rriot":{"u":"u","s":"s","h":"h","k":"k","r":{"a":"https://api.example.test"}}}}`},
		"a login that is no object": {accounttest.Login: `{"code":200,"data":"` + secret + `"}`},
		"no code":                   {accounttest.SendCode: `{"data":"` + secret + `"}`},
		"no JSON":                   {accounttest.Region: secret},
		"no signature":              {accounttest.Sign: `{"code":200,"data":{}}`},
		"no home":                   {accounttest.Detail: `{"code":200,"data":{}}`},
		"a home that is a path":     {accounttest.Detail: `{"code":200,"data":{"rrHomeId":"../` + secret + `"}}`},
		"devices with no result":    {accounttest.Devices: `{"success":true}`},
		"devices that are no JSON":  {accounttest.Devices: secret},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			api, client := served(t)
			api.Raw = raw

			err := whole(t.Context(), client, accounttest.Code)

			if !errors.Is(err, account.ErrAnswer) || strings.Contains(err.Error(), secret) {
				t.Errorf("the login = %v, want ErrAnswer and nothing of the answer", err)
			}
		})
	}

	for name, onward := range map[string]string{
		"in clear":      "http://login.example.test",
		"no address":    "login.example.test",
		"with a secret": "https://user:pass@login.example.test",
	} {
		t.Run("sent on "+name, func(t *testing.T) {
			t.Parallel()

			api, client := served(t)
			api.Onward = onward

			_, err := client.Ask(t.Context(), accounttest.Email)

			if !errors.Is(err, account.ErrAnswer) || len(api.Requests()) != 1 {
				t.Errorf("Ask = %v after %d requests, want ErrAnswer and nothing sent on", err, len(api.Requests()))
			}
		})
	}
}

// TestUnread: a device with no id or no key is left out and said, and
// the others are given with it.
func TestUnread(t *testing.T) {
	t.Parallel()

	api, client := served(t)
	api.Own = append(api.Own, accounttest.Device{ID: "duid-keyless", Name: "Keyless"})
	api.Received = append(api.Received, accounttest.Device{Key: "a-key", Name: "Nameless"})

	devices, err := client.Devices(t.Context(), loggedIn(t, client))

	if !errors.Is(err, account.ErrUnread) || !strings.Contains(err.Error(), "2 of the 4") {
		t.Errorf("Devices = %v, want ErrUnread for 2 of the 4", err)
	}

	if len(devices) != 2 || devices[0].Identity.ID != "duid-own" || devices[1].Identity.ID != "duid-shared" {
		t.Errorf("Devices gave %d devices beside the error, want the two that read", len(devices))
	}
}

// TestArguments: what cannot be sent is not: no address, a challenge Ask
// did not make, no code, no session.
func TestArguments(t *testing.T) {
	t.Parallel()

	api, client := served(t)

	if _, err := client.Ask(t.Context(), ""); !errors.Is(err, account.ErrArgument) {
		t.Errorf("Ask with no address = %v, want ErrArgument", err)
	}

	if _, err := client.Answer(
		t.Context(),
		account.Challenge{},
		accounttest.Code,
	); !errors.Is(
		err,
		account.ErrArgument,
	) {
		t.Errorf("Answer to no challenge = %v, want ErrArgument", err)
	}

	if _, err := client.Devices(t.Context(), account.Session{}); !errors.Is(err, account.ErrArgument) {
		t.Errorf("Devices of no session = %v, want ErrArgument", err)
	}

	challenge, err := client.Ask(t.Context(), accounttest.Email)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	before := len(api.Requests())

	if _, err = client.Answer(t.Context(), challenge, ""); !errors.Is(err, account.ErrArgument) {
		t.Errorf("Answer with no code = %v, want ErrArgument", err)
	}

	if len(api.Requests()) != before {
		t.Error("an answer with no code was sent")
	}
}

var errNoRoute = errors.New("no route")

// failing is a Doer that reaches nothing, and says so as http.Client
// does: with the URL asked.
type failing struct{}

func (failing) Do(request *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: request.URL.String(), Err: errNoRoute}
}

// TestSecrets: neither the address, the code nor a key is in an error or
// in what a value shows of itself, under fmt or slog.
func TestSecrets(t *testing.T) {
	t.Parallel()

	unreached := account.New(account.Options{Gateways: []string{"https://login.example.test"}, HTTP: failing{}})

	_, err := unreached.Ask(t.Context(), accounttest.Email)
	if err == nil || strings.Contains(err.Error(), "owner") || !strings.Contains(err.Error(), "no route") {
		t.Errorf("Ask through what reaches nothing = %v, want what failed and not the address", err)
	}

	_, client := served(t)

	challenge, err := client.Ask(t.Context(), accounttest.Email)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	session, err := client.Answer(t.Context(), challenge, accounttest.Code)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	devices, err := client.Devices(t.Context(), session)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}

	var logged strings.Builder

	slog.New(slog.NewTextHandler(&logged, nil)).Info("shown",
		"challenge", challenge, "session", session, "cloud", session.Cloud(), "devices", devices)

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		fmt.Fprintf(&logged, verb+verb+verb+verb, challenge, session, session.Cloud(), devices)
	}

	shown := logged.String()

	for _, secret := range []string{
		"owner", accounttest.Token, accounttest.Secret, accounttest.Mac, accounttest.Key, accounttest.User,
		"key-of-the-own", "key-of-the-shared",
	} {
		if strings.Contains(shown, secret) {
			t.Errorf("%q is shown by a value formatted or logged", secret)
		}
	}

	for _, said := range []string{accounttest.Broker, "duid-own", "roborock.vacuum.sc01"} {
		if !strings.Contains(shown, said) {
			t.Errorf("%q is not shown, and is no secret", said)
		}
	}
}

// ending is a Doer that ends a context when a path is asked for, before
// it is.
type ending struct {
	inner account.Doer
	path  string
	end   context.CancelFunc
}

func (e ending) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Path == e.path {
		e.end()

		if err := request.Context().Err(); err != nil {
			return nil, err
		}
	}

	return e.inner.Do(request)
}

// TestEnds: a context that ends puts an end to a login, wherever it was:
// at the agreement too, whose failure is otherwise passed over.
func TestEnds(t *testing.T) {
	t.Parallel()

	for _, path := range []string{accounttest.Sign, accounttest.Agreement, accounttest.Login} {
		t.Run("at "+path, func(t *testing.T) {
			t.Parallel()

			api := accounttest.New()
			t.Cleanup(api.Close)

			ended, end := context.WithCancel(t.Context())
			defer end()

			client := account.New(account.Options{
				Gateways: []string{api.URL},
				HTTP:     ending{inner: api.Browser(), path: path, end: end},
			})

			challenge, err := client.Ask(ended, accounttest.Email)
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}

			before := len(api.Requests())

			if _, err = client.Answer(ended, challenge, accounttest.Code); !errors.Is(err, context.Canceled) {
				t.Errorf("Answer under a context that ended = %v, want context.Canceled", err)
			}

			for _, request := range api.Requests()[before:] {
				if request.Path == path || request.Path == accounttest.Login {
					t.Errorf("%s was asked after the context ended", request.Path)
				}
			}
		})
	}
}
