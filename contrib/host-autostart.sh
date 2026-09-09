#!/bin/sh
# Idempotently start `ssh-agent-relay serve` on the macOS host.
#
# Meant to be called from a devcontainer's initializeCommand (e.g. the dcp
# ssh-relay profile), so the host relay comes up whenever a container starts and
# self-exits when idle (--idle-exit). Safe to run repeatedly and from several
# containers at once — a no-op if something is already listening on the port.
#
# Never fails the caller: any problem logs to stderr and exits 0 so the
# container start is not blocked.
#
# Installed by Homebrew as `ssh-agent-relay-autostart`.
set -eu

BIN="${SSH_AGENT_RELAY_BIN:-}"
LISTEN="${SSH_AGENT_RELAY_LISTEN:-127.0.0.1:17890}"
TOKEN_FILE="${SSH_AGENT_RELAY_TOKEN_FILE:-$HOME/.config/ssh-agent-relay/token}"
LOG="${SSH_AGENT_RELAY_LOG:-$HOME/Library/Logs/ssh-agent-relay.log}"
IDLE_EXIT="${SSH_AGENT_RELAY_IDLE_EXIT:-30m}"

port=${LISTEN##*:}

# Already up? Done.
if nc -z 127.0.0.1 "$port" 2>/dev/null; then
	exit 0
fi

# Locate the binary: explicit override, PATH, common install dirs.
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
	BIN="$(command -v ssh-agent-relay 2>/dev/null || true)"
fi
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
	for c in \
		/opt/homebrew/bin/ssh-agent-relay \
		/usr/local/bin/ssh-agent-relay \
		"$HOME/.local/bin/ssh-agent-relay"; do
		[ -x "$c" ] && BIN=$c && break
	done
fi
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
	echo "ssh-agent-relay autostart: binary not found — brew install clamoriniere/tap/ssh-agent-relay" >&2
	exit 0
fi

# Mint the shared token on first use.
if [ ! -s "$TOKEN_FILE" ]; then
	mkdir -p "$(dirname "$TOKEN_FILE")"
	head -c 32 /dev/urandom | base64 | tr -d '\n' >"$TOKEN_FILE"
	chmod 600 "$TOKEN_FILE"
fi

# Resolve the agent socket: prefer the inherited env, then known 1Password paths.
sock="${SSH_AUTH_SOCK:-}"
if [ -z "$sock" ] || [ ! -S "$sock" ]; then
	for c in \
		"$HOME/.1password/agent.sock" \
		"$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"; do
		if [ -S "$c" ]; then
			sock=$c
			break
		fi
	done
fi
if [ -z "$sock" ] || [ ! -S "$sock" ]; then
	echo "ssh-agent-relay autostart: no SSH agent socket (set SSH_AUTH_SOCK or launchctl setenv it)" >&2
	exit 0
fi

mkdir -p "$(dirname "$LOG")"
nohup "$BIN" serve \
	--listen "$LISTEN" \
	--agent "$sock" \
	--token-file "$TOKEN_FILE" \
	--idle-exit "$IDLE_EXIT" \
	>>"$LOG" 2>&1 &

# Give it a moment so the very next `connect` from the container finds it.
i=0
while [ "$i" -lt 20 ]; do
	if nc -z 127.0.0.1 "$port" 2>/dev/null; then
		exit 0
	fi
	sleep 0.1
	i=$((i + 1))
done
echo "ssh-agent-relay autostart: started but port $port not yet accepting (see $LOG)" >&2
exit 0
