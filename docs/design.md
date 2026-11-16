# go-roborock: the design

Research into Roborock robot vacuums, done on 2026-11-15 from public sources, to decide
what a Go module for them would be. Nothing is built and nothing here was run against a
robot. The decisions at the end are open.

In short: most Roborock vacuums serve a TCP protocol on the LAN, and with the robot's
local key a client reads and drives one there with no cloud at run time. The key comes
from the owner's Roborock account, once. Roborock documents none of this. Some newer
models also speak Matter, which carries less. A few models have no local protocol at all.

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

## What this module would be

Not decided. The options, most local first:

1. **A client of the robot's TCP protocol**: the framing, both encryptions, the hello
   and the ping, the commands; discovery by the UDP broadcast or a given address. The key
   and the robot's id are configuration, fetched once by a login subcommand.
2. **The cloud's MQTT**, the same commands over another transport: for a model with no
   local protocol, and for maps.
3. **Matter**, as a client of a Matter controller, where the model has it and the
   smaller set is enough.

Go implementations exist and are young: [roborocli](https://github.com/jwmoss/roborocli)
does the local protocol and the cloud, [go-roborock](https://github.com/portpowered/go-roborock)
the cloud only.

## Open questions

- The robot's model, and its model code in the app's device information: it gives the
  protocol version, and so whether a local route exists.
- Whether it is set up in the Roborock app or in Mi Home.
- Whether maps and rooms are wanted, or starting, stopping and docking are enough.

## Not verified

- Nothing above was observed on a robot.
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
  which models have a local one, where the local key comes from.
- [roborocli](https://github.com/jwmoss/roborocli): a Go implementation, and its notes on
  the newer encryption.
- [go-roborock](https://github.com/portpowered/go-roborock): a Go client of the cloud.
- [Roborock, Matter protocol update](https://newsroom.roborock.com/gl/news/roborock-releases-matter-protocol-update-leading-smart-home-integration):
  the models and firmware with Matter.
- [connectedhomeip](https://github.com/project-chip/connectedhomeip): Matter's robot
  vacuum device type.
- [Valetudo, supported robots](https://valetudo.cloud/pages/general/supported-robots):
  replacement firmware and the models it covers.
- [python-miio](https://github.com/rytilahti/python-miio): the Mi Home protocol.
