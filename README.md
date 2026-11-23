# go-roborock

A Go module for Roborock robot vacuums.

Built so far: the login to the owner's Roborock account, which gives the account's way into
Roborock's cloud and the key of each of its devices; and, through that cloud, a robot of the
Q7 family read and driven. A Q7 has no protocol on the LAN.

- `roborock`: a session with the cloud, a robot on it, what it is doing, and starting,
  pausing, stopping, docking and locating it.
- `roborock/account`: the login, by a one-time code Roborock mails, and the devices of the
  account's home.
- `cmd/roborock`: the command.

```sh
export ROBOROCK_EMAIL='owner@example.com'      # the account's address
export ROBOROCK_SESSION="$HOME/.config/roborock/session.json"
just roborock account   # asks for the code Roborock mails; keeps the login in the file
just roborock devices   # the devices the file holds
just roborock status    # what the robot is doing
just roborock locate    # the robot says where it is; also start, pause, stop, dock
```

The session file holds what opens the devices: keep it as a password is kept.

[docs/design.md](docs/design.md) is the research this starts from, the decisions made, and
what was and was not verified.
