# ssh-agent-relay

Bridge a **host** SSH agent into a dev container over a loopback TCP hop.

Needed when the agent's unix socket can't be bind mounted straight into the
container. On rootless podman + macOS that's always the case: `virtiofs` on the
`applehv` VM does not pass `AF_UNIX` sockets through, so `stat($SSH_AUTH_SOCK)`
inside the VM returns `Operation not supported` and a bind mount yields an inert
file.

```
container process ──unix──▶ ssh-agent-relay connect ──TCP──▶ ssh-agent-relay serve ──unix──▶ host agent
     (SSH_AUTH_SOCK)              (in container)        (host.containers.internal)  (on the mac)   ($SSH_AUTH_SOCK)
```

No private key ever enters the container — it can request signatures, not read
keys. With a biometric agent (1Password, Secretive) every signature still needs
approval on the host.

## Install

### Host (macOS)

```sh
brew install clamoriniere/tap/ssh-agent-relay
brew services start ssh-agent-relay          # runs `serve`, self-exits after 30m idle
```

For a non-default agent (1Password, custom), make its socket visible to the
launchd session once:

```sh
launchctl setenv SSH_AUTH_SOCK "$SSH_AUTH_SOCK"
```

Or don't use `brew services` at all — let a dev container start the relay on
demand via `ssh-agent-relay-autostart` (installed by the formula), which resolves
the agent socket itself and calls `serve --idle-exit 30m`.

### Dev container

Use the devcontainer Feature — it runs this repo's `install.sh` and wires
`connect` + `SSH_AUTH_SOCK`:

```jsonc
"features": {
    "ghcr.io/clamoriniere/devcontainer-features/ssh-agent-relay:1": {}
}
```

Or install directly:

```sh
curl -fsSL https://raw.githubusercontent.com/clamoriniere/ssh-agent-relay/main/install.sh | sh
```

`install.sh` downloads the prebuilt binary for the platform from the GitHub
release (verifying `checksums.txt`), or falls back to `go install` when the
download is unavailable and Go is present.

## Modes

| Command | Runs | Does |
| --- | --- | --- |
| `serve` | host | accepts TCP, forwards each connection to `$SSH_AUTH_SOCK`. `--idle-exit 30m` quits once nothing has used it. |
| `connect` | container | creates a unix socket, forwards each connection to the host relay over TCP. Point `SSH_AUTH_SOCK` at it. |
| `version` | — | prints version/commit/date. |

### Token

A shared token gates the TCP port so other local processes on the host can't use
the agent through it. `--token-file <path>` (or `$SSH_AGENT_RELAY_TOKEN`), same
value on both sides. `ssh-agent-relay-autostart` mints one at
`~/.config/ssh-agent-relay/token` on first run. Omit it on both sides to disable.

### Manual

```sh
# host
ssh-agent-relay serve --listen 127.0.0.1:17890 \
  --token-file ~/.config/ssh-agent-relay/token --idle-exit 30m

# container
ssh-agent-relay connect --socket /tmp/ssh-agent-relay.sock \
  --upstream host.containers.internal:17890 \
  --token-file ~/.config/ssh-agent-relay/token --replace
export SSH_AUTH_SOCK=/tmp/ssh-agent-relay.sock
```

`contrib/com.clamoriniere.ssh-agent-relay.plist` is a launchd template for a
manual (non-Homebrew) host install.

## Verify

```sh
ssh-add -l                    # lists the host agent's keys
ssh -T git@github.com         # authenticates via the relayed agent
```

## Security

- No key material is copied. The container can request signatures, not read keys.
- The token is sent in cleartext over loopback — fine for that threat model, not
  for a real network. `serve` binds loopback only unless `--allow-remote`.
- The container-side socket is `chmod 0600`, owned by the container user.
- `host.containers.internal` must resolve to the host from the container; try
  `host.docker.internal` or the default-route gateway otherwise.
- Prefer a biometric agent so every relayed signature needs explicit approval.

## License

MIT
