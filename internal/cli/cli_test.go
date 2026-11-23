package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/duncanite/go-roborock/internal/cli"
	"github.com/duncanite/go-roborock/roborock/account"
	"github.com/duncanite/go-roborock/roborock/account/accounttest"
)

// ran is one run of the command.
type ran struct {
	status int
	out    string
	err    string
}

// served is an API and the options that reach it.
func served(t *testing.T) (*accounttest.API, account.Options) {
	t.Helper()

	api := accounttest.New()
	t.Cleanup(api.Close)

	return api, account.Options{Gateways: []string{api.URL}, HTTP: api.Browser()}
}

// run runs the command against an API, with what is typed and the
// environment's address.
func run(t *testing.T, api account.Options, typed io.Reader, email string, args ...string) ran {
	t.Helper()

	var out, err bytes.Buffer

	status := cli.Run(t.Context(), cli.Environment{
		Args: args, Out: &out, Err: &err, In: typed, Account: api,
		Getenv: func(name string) string {
			if name == cli.EnvEmail {
				return email
			}

			return ""
		},
	})

	return ran{status: status, out: out.String(), err: err.String()}
}

const table = "" +
	"NAME        MODEL                 PROTOCOL  FIRMWARE  SHARED  ID\n" +
	"Downstairs  roborock.vacuum.sc01  B01       02.10.20  no      duid-own\n" +
	"Garage      roborock.vacuum.a15   1.0       02.56.02  yes     duid-shared\n"

// TestAccount: the login, with the code typed, keeps the account's way
// into the cloud and each device's key in the session file, which its
// owner alone reads, in a directory made the same; it prints the devices
// and no key. devices then prints them from the file, asking Roborock
// nothing.
func TestAccount(t *testing.T) {
	t.Parallel()

	api, options := served(t)
	file := filepath.Join(t.TempDir(), "made", "session.json")

	got := run(
		t,
		options,
		strings.NewReader(" "+accounttest.Code+" \n"),
		accounttest.Email,
		"--session",
		file,
		"account",
	)

	if got.status != cli.ExitOK || got.out != table {
		t.Fatalf("account = %d, printing\n%s\nand\n%s\nwant\n%s", got.status, got.out, got.err, table)
	}

	if !strings.Contains(got.err, "The code: ") || !strings.Contains(got.err, "Kept in "+file) {
		t.Errorf("account says %q, want the code asked for and where the login is kept", got.err)
	}

	for _, secret := range []string{
		accounttest.Token, accounttest.Secret, accounttest.Mac, accounttest.Key, accounttest.User,
		"key-of-the-own", "key-of-the-shared", accounttest.Email,
	} {
		if strings.Contains(got.out+got.err, secret) {
			t.Errorf("account prints %q", secret)
		}
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the session file: %v", err)
	}

	var kept map[string]any
	if err = json.Unmarshal(raw, &kept); err != nil {
		t.Fatalf("the session file is not JSON: %v", err)
	}

	for _, held := range []string{
		accounttest.Broker, accounttest.User, accounttest.Secret, accounttest.Key,
		"duid-own", "key-of-the-own", "duid-shared", "key-of-the-shared",
	} {
		if !strings.Contains(string(raw), `"`+held+`"`) {
			t.Errorf("the session file does not hold %q", held)
		}
	}

	for _, left := range []string{accounttest.Token, accounttest.Mac, accounttest.Email} {
		if strings.Contains(string(raw), left) {
			t.Errorf("the session file holds %q, which reaches no device", left)
		}
	}

	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{file: 0o600, filepath.Dir(file): 0o700} {
			info, statErr := os.Stat(name)
			if statErr != nil {
				t.Fatalf("%s: %v", name, statErr)
			}

			if info.Mode().Perm() != want {
				t.Errorf("%s is %v, want %v", name, info.Mode().Perm(), want)
			}
		}
	}

	asked := len(api.Requests())

	listed := run(t, options, nil, "", "--session", file, "devices")
	if listed.status != cli.ExitOK || listed.out != table || listed.err != "" {
		t.Errorf("devices = %d, printing\n%s\nand\n%s\nwant\n%s", listed.status, listed.out, listed.err, table)
	}

	if len(api.Requests()) != asked {
		t.Error("devices asked Roborock for something")
	}
}

// TestAccountNotRun: what the login cannot run with is said, before
// Roborock is asked for a code.
func TestAccountNotRun(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "session.json")

	for name, attempt := range map[string]struct {
		email string
		typed io.Reader
		args  []string
		says  string
	}{
		"no address":      {"", strings.NewReader("1\n"), []string{"--session", file, "account"}, cli.EnvEmail},
		"no session file": {accounttest.Email, strings.NewReader("1\n"), []string{"account"}, "--session"},
		"nowhere to type": {accounttest.Email, nil, []string{"--session", file, "account"}, "nowhere to read the code"},
		"no time":         {accounttest.Email, strings.NewReader("1\n"), []string{"--session", file, "--timeout", "0s", "account"}, "--timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			api, options := served(t)

			got := run(t, options, attempt.typed, attempt.email, attempt.args...)

			if got.status != cli.ExitError || !strings.Contains(got.err, attempt.says) || got.out != "" {
				t.Errorf("account = %d, saying %q, want an error that names %q", got.status, got.err, attempt.says)
			}

			if len(api.Requests()) != 0 {
				t.Error("Roborock was asked")
			}
		})
	}
}

// TestAccountRefused: a login that does not end keeps nothing: no code
// typed, more typed than a code is, a code Roborock refuses, an address
// it does not know.
func TestAccountRefused(t *testing.T) {
	t.Parallel()

	for name, attempt := range map[string]struct {
		email string
		typed string
		says  string
	}{
		"nothing typed":    {accounttest.Email, "", "no code was typed"},
		"an empty line":    {accounttest.Email, "\n", "no code was typed"},
		"too much typed":   {accounttest.Email, strings.Repeat("1", 300) + "\n", "more than 256 characters"},
		"a wrong code":     {accounttest.Email, "000000\n", "refused the code"},
		"no such account":  {"nobody@example.test", "1\n", "no roborock account by that address"},
		"a code not ended": {accounttest.Email, "000000", "refused the code"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, options := served(t)
			file := filepath.Join(t.TempDir(), "session.json")

			got := run(t, options, strings.NewReader(attempt.typed), attempt.email, "--session", file, "account")

			if got.status != cli.ExitError || !strings.Contains(got.err, attempt.says) || got.out != "" {
				t.Errorf("account = %d, saying %q, want an error with %q", got.status, got.err, attempt.says)
			}

			if _, err := os.Stat(file); err == nil {
				t.Error("a session file was written")
			}

			if strings.Contains(got.err, attempt.email) {
				t.Error("the error names the address")
			}
		})
	}
}

// TestAccountNoDevices: a login whose devices Roborock then refuses keeps
// nothing: a session with no device in it reaches none.
func TestAccountNoDevices(t *testing.T) {
	t.Parallel()

	api, options := served(t)
	api.Refuse = map[string]int{accounttest.Detail: 2010}

	file := filepath.Join(t.TempDir(), "session.json")

	got := run(t, options, strings.NewReader(accounttest.Code+"\n"), accounttest.Email, "--session", file, "account")

	if got.status != cli.ExitError || !strings.Contains(got.err, "code 2010") || got.out != "" {
		t.Errorf("account = %d, printing %q and saying %q", got.status, got.out, got.err)
	}

	if _, err := os.Stat(file); err == nil {
		t.Error("a session file was written")
	}
}

// TestAccountUnread: a device that could not be read is said, as a
// failure, after the others are kept and printed.
func TestAccountUnread(t *testing.T) {
	t.Parallel()

	api, options := served(t)
	api.Own = append(api.Own, accounttest.Device{ID: "duid-keyless", Name: "Keyless"})

	file := filepath.Join(t.TempDir(), "session.json")

	got := run(t, options, strings.NewReader(accounttest.Code+"\n"), accounttest.Email, "--session", file, "account")

	if got.status != cli.ExitError || got.out != table || !strings.Contains(got.err, "1 of the 3") {
		t.Errorf("account = %d, printing\n%s\nand saying %q", got.status, got.out, got.err)
	}

	if listed := run(t, options, nil, "", "--session", file, "devices"); listed.out != table {
		t.Errorf("devices prints\n%s\nwant the two that read", listed.out)
	}
}

// TestDevices: what is no session file is said to be none, with nothing
// of what it holds; a session with no device says so; and a device's
// text is printed so that a terminal obeys none of it.
func TestDevices(t *testing.T) {
	t.Parallel()

	_, options := served(t)
	dir := t.TempDir()

	write := func(name, content string) string {
		t.Helper()

		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		return file
	}

	const cloud = `"cloud":{"broker":"ssl://b:1","user":"u","secret":"held-secret","key":"k"}`

	for name, attempt := range map[string]struct {
		args   []string
		status int
		out    string
		says   string
	}{
		"no file named": {[]string{"devices"}, cli.ExitError, "", "--session"},
		"no such file": {
			[]string{"--session", filepath.Join(dir, "none.json"), "devices"}, cli.ExitError, "", "the session file",
		},
		"no JSON": {
			[]string{"--session", write("text", "held-secret")}, cli.ExitError, "", "is not a session file",
		},
		"another JSON": {
			[]string{"--session", write("other.json", `{"held-secret":1}`)}, cli.ExitError, "", "is not a session file",
		},
		"a session and something else": {
			[]string{"--session", write("more.json", `{`+cloud+`,"devices":[],"more":"held-secret"}`)},
			cli.ExitError, "", "is not a session file",
		},
		"no cloud": {
			[]string{"--session", write("nocloud.json", `{"devices":[]}`)}, cli.ExitError, "", "is not a session file",
		},
		"no device": {
			[]string{"--session", write("empty.json", `{`+cloud+`,"devices":[]}`)},
			cli.ExitOK,
			"The account has no device.\n", "",
		},
		"a device that writes to the terminal": {
			[]string{"--session", write("hostile.json", `{`+cloud+
				`,"devices":[{"name":"a\u001b[2Jb","id":"i\nd","protocol":"","model":"","firmware":"","shared":false,"key":"held-secret"}]}`)},
			cli.ExitOK,
			"NAME       MODEL  PROTOCOL  FIRMWARE  SHARED  ID\n" + `a\x1b[2Jb  -      -         -         no      i\nd` + "\n",
			"",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			args := attempt.args
			if args[len(args)-1] != "devices" {
				args = append(args, "devices")
			}

			got := run(t, options, nil, "", args...)

			if got.status != attempt.status || got.out != attempt.out || !strings.Contains(got.err, attempt.says) {
				t.Errorf("devices = %d, printing %q and saying %q; want %d, %q and %q",
					got.status, got.out, got.err, attempt.status, attempt.out, attempt.says)
			}

			if strings.Contains(got.out+got.err, "held-secret") {
				t.Error("what the file holds is printed")
			}
		})
	}
}

// TestHelp: the command says what it does and what it reads.
func TestHelp(t *testing.T) {
	t.Parallel()

	_, options := served(t)

	got := run(t, options, nil, "", "--help")

	for _, said := range []string{"account", "devices", "--session", "ROBOROCK_SESSION", cli.EnvEmail} {
		if !strings.Contains(got.out, said) {
			t.Errorf("--help does not say %q", said)
		}
	}

	if got.status != cli.ExitOK {
		t.Errorf("--help = %d", got.status)
	}
}
