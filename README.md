# go-roborock

A Go module for Roborock robot vacuums.

Built so far: the login to the owner's Roborock account, which gives the account's way into
Roborock's cloud and the key of each of its devices. Nothing speaks to a robot yet.

- `roborock`: what reaches a device, an account's `Cloud` and a device's `Identity`.
- `roborock/account`: the login, by a one-time code Roborock mails, and the devices of the
  account's home.
- `cmd/roborock`: the command.

```sh
export ROBOROCK_EMAIL='owner@example.com'      # the account's address
export ROBOROCK_SESSION="$HOME/.config/roborock/session.json"
just roborock account   # asks for the code Roborock mails; keeps the login in the file
just roborock devices   # the devices the file holds
```

The session file holds what opens the devices: keep it as a password is kept.

[docs/design.md](docs/design.md) is the research this starts from, the decisions made, and
what was and was not verified.
