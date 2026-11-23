# go-roborock: the design

Research into Roborock robot vacuums, begun on 2026-11-15 from public sources, and what is
built from it. Built so far: the login to the owner's account, which gives the account's
way into Roborock's cloud and each device's key; and, through that cloud, a robot of the
Q7 family read and driven.

In short: most Roborock vacuums serve a TCP protocol on the LAN, and with the robot's
local key a client reads and drives one there with no cloud at run time. The key comes
from the owner's Roborock account, once. Roborock documents none of this. Some newer
models also speak Matter, which carries less. A few models have no local protocol at all:
the robot this module is first run against, a Q7 M5+, is one of them, and is reached
through Roborock's cloud alone.

## The device

From [Home Assistant's page](https://www.home-assistant.io/integrations/roborock/) and the
source of [python-roborock](https://github.com/Python-roborock/python-roborock), the
library behind it:

- A robot reaches Roborock's cloud over MQTT, on outbound port 8883.
- On the LAN it listens on TCP port 58867 for commands and broadcasts its presence on UDP
  port 58866.
- Which protocol a robot speaks is a property of the model, its protocol version.
  python-roborock's `docs/DEVICES.md` gives the table: version 1.0, "most vacuum robots",
  on MQTT and local TCP; A01, the Dyad and Zeo washers, and B01, the Q7 and Q10 series, on
  MQTT only.
- A vacuum set up in Xiaomi's Mi Home app is another case: it speaks Xiaomi's protocol
  with a token from Xiaomi's cloud.

## Ways in

### The robot's TCP protocol (local)

From python-roborock (`roborock/protocol.py`, `devices/transport/local_channel.py`,
`broadcast_protocol.py`) and the protocol notes of [roborocli](https://github.com/jwmoss/roborocli),
a Go implementation:

- Frames carry a version, a sequence number, a random number, a timestamp and a message
  type, then an encrypted payload and a checksum. A session opens with a hello, and is
  kept by a ping.
- Two encryptions are in use on local connections: the older one, AES under a key
  derived from the robot's local key and the frame's timestamp, and a newer one named
  L01, AES-GCM, that current firmware asks for. python-roborock tries the first and then
  the second.
- The payload is JSON: a method and its parameters. Read: status, battery, consumables,
  suction and mop settings, the rooms. Do: start, stop, return to the dock, clean rooms or
  zones.
- The local key, with the robot's id, comes from the account's "home data" in Roborock's
  cloud. It stays the same until the robot is set up again.
- Home Assistant's integration fetches maps through the cloud even when commands go
  locally.

### Roborock's cloud, as the app uses it (unofficial)

A login by a code sent by mail or by password, the home data, and an MQTT session that
carries the same commands. It is the only route for the A01 and B01 models. Home
Assistant's page warns that Roborock's cloud "may ban your IP address" for too many
requests.

The login, as python-roborock makes it (`roborock/web_api/`) and Home Assistant uses it:

- Roborock has a login server per region: the United States, Europe, China, Russia. Each
  is asked in turn where the account is; one that does not know it answers with no
  country.
- The region's server is asked to mail a one-time code, and the login is made with it.
  The login names the version of the user agreement the account accepted, which the
  server gives; an account that never accepted it in the app is refused.
- The login gives a token for the login server and, for the cloud, an id, two secrets and
  an HMAC key, with the addresses of the cloud's API and of its MQTT broker. Roborock
  issues new ones with every login.
- The login server gives the id of the account's home. The cloud's API gives the home:
  the account's devices and those shared with it, each with its id, its key, its protocol
  version and its product. The request is signed with the HMAC key, over the account's id,
  a nonce, the time and the digest of the path.

### Matter (local, official)

Roborock [announced Matter](https://newsroom.roborock.com/gl/news/roborock-releases-matter-protocol-update-leading-smart-home-integration)
by a firmware update in April 2025 for the S8 MaxV Ultra, the Saros Z70, the Saros 10 and
10R, the Qrevo Curv, Edge and Master. Matter's robot vacuum device type carries the run
mode, the cleaning mode, the operational state with pause, resume and return, and rooms
as service areas. It has nothing for maps, consumables or the dock.

### Replacement firmware

[Valetudo](https://valetudo.cloud/pages/general/supported-robots) replaces the cloud on
the robot itself. Its list for Roborock stops at older models, the S5 to the S7 and the
Q7 Max, and most need the robot taken apart.

## The ways in, compared

| | The robot's TCP protocol | Roborock's cloud | Matter |
|---|---|---|---|
| Supported by Roborock | no | no | yes, on the models named |
| Local at run time | yes | no | yes |
| Needs | the local key, fetched once from the account | the account | a Matter controller, a pairing code |
| Gives | status, consumables, rooms, every command | the same, and maps | modes, state, rooms |
| Risk | a firmware update can change the protocol, as L01 did | the login changing; request limits | less than the robot can do |

## What this module is

The cloud first, since the robot at hand has no other route; the robot's TCP protocol is
to follow for the models that have it.

### Shape

- `roborock`: what reaches a device, and a robot through the cloud. `Cloud` is an
  account's way into Roborock's cloud: the broker's address and what its user name and
  password are made of. `Identity` names one device and holds the key its messages are
  encrypted under. Both show no secret when formatted, logged or marshalled. `Connect`
  opens a `Session` with the cloud, one connection for the account; `Session.Robot` opens
  a `Robot` on it. `Robot.State` is what the robot is doing, each part nil when the robot
  did not say it; `Start`, `Pause`, `Stop`, `Dock` and `Locate` drive it; `Properties`
  and `Call` are the protocol by its own names. `Speaks` says a device is one this
  package speaks to.
- `roborock/roborocktest`: the cloud's broker, under TLS, with a robot behind it, for
  tests.
- `internal/wire`: a device's frame as the cloud carries it, and the B01 encryption.
- `internal/mqtt`: the little of MQTT 5 the broker is spoken to in.
- `roborock/account`: the login. `Ask` finds the account's region and has a code mailed;
  `Answer` logs in with it and gives a `Session`, whose `Cloud` is the account's;
  `Devices` gives the home's devices, each with its `Identity`. The standard library
  alone.
- `roborock/account/accounttest`: the login servers and the cloud's API in one, for tests.
- `cmd/roborock`: `account` logs in, with the code typed where it is asked for, and keeps
  the cloud's credentials and each device's key in the session file; `devices` prints the
  devices that file holds. No key is printed. `status` prints what the robot is doing;
  `start`, `pause`, `stop`, `dock` and `locate` drive it; `get` and `call` are the
  protocol by its own names.

### Decisions

- **The newer login, the one Home Assistant uses.** python-roborock has two generations
  of the code login; its own command line uses the older, Home Assistant the newer
  (`request_code_v4`, `code_login_v4`). The newer is the one used here, alone: the one the
  most installations exercise.
- **Only HTTPS, wherever the API sends the client.** A region's server names the server to
  log in to, and the login names the cloud's API. Each is a stranger's text that the next
  request, with the account's address or a signature, would go to: one that is not an
  HTTPS address with no user in it is refused, and nothing is sent.
- **A transport of its own, which speaks TLS 1.2.** Roborock's login servers complete a
  handshake at TLS 1.2 with a NIST curve, and at nothing later. A program that narrows the
  process's default transport to TLS 1.3, as the toolkit this command is built on does,
  reaches none of them through it. The client given no HTTP client makes its own, at TLS
  1.2 and above, and does not use the process's default.
- **A region that does not answer does not hide the account in another.** The next is
  asked. When none then knows the account, the error is that of the first that did not
  answer, with how many did not: the account may be its. A region that answers with a
  refusal ends the search.
- **An error names the server asked**, and nothing else of the address asked.
- **No error carries the address, the code or a key.** The message Roborock refuses with
  is not handed on, only its code; an HTTP client's error is handed on without the URL,
  whose query holds the address.
- **A session file, written by the command.** A login gives seven values that later
  commands need, and Roborock limits how often a code is asked for. The command keeps them
  in a file its owner alone reads, and prints none. The library writes nothing: where a
  login is kept is the program's business.
- **One device unread does not hide the others.** A device the home lists with no id or
  no key is left out and said.
- **The client names itself as the app does.** The login is taken from the app: the
  version, system and phone model the open implementations send are sent here.
- **No limiter of its own.** python-roborock holds itself to a few logins an hour. Here
  every `Ask` is a person typing a command; Roborock's refusal is told apart as too often.

- **An MQTT client of this module's own.** The broker wants a session opened with a user
  name and a password, a subscription and messages at most once. The Go clients there are
  carry the Eclipse licence, which the licence lane here refuses, or all of MQTT. Some
  seven hundred and fifty lines on the standard library do what is needed, and are held to the bytes
  the reference implementation's client writes.
- **Under TLS, or not at all.** The broker's address comes from the login. One that is not
  `ssl`, `tls` or `mqtts` is refused: the credentials would go in clear.
- **One connection for the account**, which every robot is spoken to through, as the
  reference implementation does to spare the broker.
- **Nothing reconnects.** A session lost is over, and says so; whoever holds it opens
  another. The cloud refuses an account asked too much with the reason it refuses bad
  credentials with, and a client that reconnects on its own is how that is reached.
- **A state is asked for, never told.** The reference implementation reads no push of a
  Q7's properties: its shape is not known. A robot polled every three seconds through a
  clean of a hundred minutes was reported to take it.
- **A reading is never a default.** Each part of a state is nil when the robot did not say
  it, or said it in a number there is no word for.
- **A fault is not always a failure.** A robot back from a clean reports 2103 for as long
  as it sits on its dock, and 2102 on its way there; the robot at hand, charged on its
  dock, reported 2105, which the reference implementation has no name for.
  `Fault.Failure` is true for the numbers that implementation names as something wrong,
  and for no other: a number with no name is given as it is, and not called a failure.
- **The Q7 family alone.** The protocol B01 has two families that speak differently; the
  other, the Q10s, is refused as unsupported, with the devices of the other protocols.
- **An answer is found by its number**, among everything a robot says on its topic: maps,
  answers to other sessions of the account, frames that do not open. A request is padded
  twice, as the reference implementation pads it; an answer is read padded once or twice.

### Next

- The robot's TCP protocol, for the models that have one: frames and both encryptions are
  written and held back until a robot that speaks them is at hand.
- Rooms, maps, consumables and the settings: the protocol has them, and nothing here reads
  them yet but `get`.

Go implementations exist and are young: [roborocli](https://github.com/jwmoss/roborocli)
does the local protocol and the cloud, [go-roborock](https://github.com/portpowered/go-roborock)
the cloud only.

## Observed

- A Q7 M5+ set up in the Roborock app broadcasts nothing on UDP port 58866: ten seconds
  of listening on its network heard no robot, as its protocol version says.
- Asked on 2026-11-22 about an address that is no account, the login server of the United
  States answered success with no country, as the open implementations expect, and with
  a warning beside it: "This endpoint is deprecated and will be removed." What replaces
  it is not known. The command, run with such an address, asked the four regions and said
  that none knows the account; it wrote nothing.
- The same server refused the same question with code 9002 after a handful of requests
  within a minute.
- The login servers of the United States and of Europe completed a handshake at TLS 1.2
  with P-256, and closed the connection on one offered at TLS 1.3 alone, or with X25519
  and its post-quantum hybrid as the only key exchanges.
- `account` was run on 2026-11-23 against a real account in the United States: the code
  was mailed and taken, the login made, and the home, asked for under the signature,
  listed one device. It is the Q7 M5+, shared with the account by another: the model
  `roborock.vacuum.sc05`, the protocol B01, the firmware 03.01.80. The session file was
  written, and `devices` printed the same from it. The cloud's credentials and the
  device's key it holds were then taken by Roborock's broker and by the robot.

- `status` and `locate` were run on 2026-11-23 against the real Q7 M5+, through the
  session file `account` wrote. The broker took the credentials derived from the login,
  under TLS; the robot answered a request written as JSON with no spaces, under a number
  of twelve digits, padded twice. `status` printed it charging at 100%, its suction
  balanced, its water medium, its mode vacuum, its dock idle, and its fault 2105.
  `locate` was taken, and the robot answered.

## Open questions

- Whether maps and rooms are wanted, or starting, stopping and docking are enough.
- What a client asks in place of the region lookup once Roborock removes it.

## Not verified

- Beyond [Observed](#observed), the login was run against this module's stand-in only,
  which was written from the same sources as the client.
- A region other than the United States, a home with devices of the account's own or
  with several devices, an account that has not accepted the user agreement, and every
  refusal but the one observed.
- Held by no test: that the client's own transport speaks TLS 1.2, which only Roborock's
  servers show; the bound on an answer's size; the command's timeout.
- How long the cloud's credentials of a login stay good, and whether a new login ends an
  older one's.
- Against the real robot: `start`, `pause`, `stop` and `dock`, every activity but
  charging, a fault that is one, a dock that empties, a robot that refuses, a session
  kept for longer than one command and its pings, and a session the broker ends. Those
  were run against this module's own cloud only, which was written from the same notes
  as the client. What holds them to the reference implementation is its recorded session
  of a Q7: the MQTT client writes that session's packets byte for byte, the frame layer
  makes its frames byte for byte from their content, and the test cloud takes the
  credentials it derives.
- What the fault 2105 means.
- Whether a robot takes a request padded once, or a number shorter than twelve digits.
- Whether `Start` goes on with a paused clean or begins another: the protocol has no
  resume.
- Which number a Q7 gives for its activity while it vacuums without mopping.
- Held by no test: that a packet of MQTT is never numbered zero.
- Of the device and the ways in described above, nothing was observed on a robot but
  what [Observed](#observed) records.
- Which protocol version the newest models speak: the table read names series, not every
  model.
- Whether maps can be fetched over the local connection.
- Whether Roborock's Matter carries rooms and battery on a given model.
- The Mi Home protocol's port and which current models can still be set up there.
- The cloud's host names: they are given to a session at login.

## Sources

- [Home Assistant, Roborock](https://www.home-assistant.io/integrations/roborock/): the
  ports, local and cloud together, maps through the cloud, the warning on request limits.
- [python-roborock](https://github.com/Python-roborock/python-roborock): the protocol,
  which models have a local one, where the local key comes from. The login was read at
  [186bab8](https://github.com/Python-roborock/python-roborock/tree/186bab8350289080b80a515bc1cd9dadf9379d26/roborock/web_api).
- [Home Assistant's Roborock login](https://github.com/home-assistant/core/blob/dev/homeassistant/components/roborock/config_flow.py):
  which generation of the login it uses.
- [roborocli](https://github.com/jwmoss/roborocli): a Go implementation, and its notes on
  the newer encryption, read at
  [0a4afa2](https://github.com/jwmoss/roborocli/tree/0a4afa2642691f4577e22260eef27e845b27d5b1).
- [go-roborock](https://github.com/portpowered/go-roborock): a Go client of the cloud.
- [Roborock, Matter protocol update](https://newsroom.roborock.com/gl/news/roborock-releases-matter-protocol-update-leading-smart-home-integration):
  the models and firmware with Matter.
- [connectedhomeip](https://github.com/project-chip/connectedhomeip): Matter's robot
  vacuum device type.
- [Valetudo, supported robots](https://valetudo.cloud/pages/general/supported-robots):
  replacement firmware and the models it covers.
- [python-miio](https://github.com/rytilahti/python-miio): the Mi Home protocol.
