#!/bin/sh
# Installs, updates or removes wakegate on a Linux machine with systemd.
#
#   install.sh agent                     on the computer that sleeps
#   install.sh proxy [--agent-token T]   on the machine that is always on
#   install.sh uninstall [--purge]       removes either; --purge also deletes
#                                        /etc/wakegate and /var/lib/wakegate
#
# Next to a built `wakegate` (a release archive, or a clone after `go build`), it
# installs that build. Piped from curl, it downloads the release for this machine and
# checks its checksum first:
#
#   curl -fsSL https://raw.githubusercontent.com/lmdr-org/wakegate/main/install.sh | sudo sh -s -- agent
#
# --version vX.Y.Z picks a release (the latest by default). Configuration files and the
# agent token are kept when it runs again: it updates the binary, the systemd unit and
# the polkit rule, and restarts the service if it was running.
set -eu

repo=lmdr-org/wakegate
bin=/usr/local/bin/wakegate
etc=/etc/wakegate
units=/etc/systemd/system
rules=/etc/polkit-1/rules.d/50-wakegate.rules
pkla=/etc/polkit-1/localauthority/50-local.d/50-wakegate.pkla

die() {
	echo "install.sh: $*" >&2
	exit 1
}

role=
version=latest
agent_token=
purge=
while [ $# -gt 0 ]; do
	case $1 in
	proxy | agent | uninstall) role=$1 ;;
	--version)
		[ $# -ge 2 ] || die "--version needs a value, such as v0.1.0"
		version=$2
		shift
		;;
	--agent-token)
		[ $# -ge 2 ] || die "--agent-token needs the token printed by the agent's installation"
		agent_token=$2
		shift
		;;
	--purge) purge=1 ;;
	*) die "unknown argument: $1 (agent, proxy or uninstall)" ;;
	esac
	shift
done
[ -n "$role" ] || die "say what to install: agent (on the computer that sleeps) or proxy (on the one always on)"
[ "$(id -u)" -eq 0 ] || die "run it as root (sudo)"
[ "$(uname -s)" = Linux ] || die "Linux only"
[ -d /run/systemd/system ] || die "systemd is not running on this machine"

uninstall() {
	for r in proxy agent; do
		if [ -e "$units/wakegate-$r.service" ]; then
			systemctl disable --now "wakegate-$r" 2>/dev/null || true
			rm -f "$units/wakegate-$r.service"
		fi
	done
	systemctl daemon-reload
	rm -f "$bin" "$rules" "$pkla"
	if [ -n "$purge" ]; then
		rm -rf "$etc" /var/lib/wakegate
		if id wakegate >/dev/null 2>&1; then
			userdel wakegate 2>/dev/null || deluser wakegate 2>/dev/null || true
		fi
		echo "wakegate removed, with its configuration and state."
	else
		echo "wakegate removed. Kept: $etc, /var/lib/wakegate and the wakegate user (--purge removes them)."
	fi
	exit 0
}
[ "$role" = uninstall ] && uninstall

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1" || die "download failed: $1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1" || die "download failed: $1"
	else
		die "curl or wget is needed to download the release"
	fi
}

# The build next to this script, or a downloaded release.
src=
case $0 in
*install.sh)
	dir=$(cd "$(dirname "$0")" && pwd)
	if [ -x "$dir/wakegate" ] && [ -d "$dir/deploy" ]; then
		src=$dir
	fi
	;;
esac
if [ -z "$src" ]; then
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7l | armv8l) arch=armv7 ;;
	armv6l) arch=armv6 ;;
	*) die "no release for $(uname -m); build it from source (see the README)" ;;
	esac
	name=wakegate-linux-$arch
	if [ "$version" = latest ]; then
		base=https://github.com/$repo/releases/latest/download
	else
		base=https://github.com/$repo/releases/download/$version
	fi
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	echo "Downloading $name ($version)…"
	fetch "$base/$name.tar.gz" "$tmp/$name.tar.gz"
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"
	(cd "$tmp" && grep " $name.tar.gz\$" SHA256SUMS | sha256sum -c - >/dev/null) ||
		die "checksum mismatch for $name.tar.gz"
	tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
	src=$tmp/$name
fi

install -m 0755 "$src/wakegate" "$bin"
installed=$("$bin" version)

if ! id wakegate >/dev/null 2>&1; then
	nologin=$(command -v nologin || echo /bin/false)
	if command -v useradd >/dev/null 2>&1; then
		useradd --system --user-group --no-create-home --home-dir /nonexistent --shell "$nologin" wakegate
	else
		adduser --system --group --no-create-home --shell "$nologin" wakegate
	fi
fi

install -d -m 0755 "$etc"
new_config=
if [ ! -e "$etc/$role.yaml" ]; then
	install -m 0644 "$src/deploy/$role.yaml" "$etc/$role.yaml"
	new_config=1
fi
token=$etc/agent.token
write_token() { # value
	(umask 077 && printf '%s\n' "$1" >"$token")
	chown root:wakegate "$token"
	chmod 0640 "$token"
}

if [ "$role" = agent ]; then
	[ -e "$token" ] || write_token "$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
	polkit=
	if [ -d /usr/share/polkit-1/rules.d ] || [ -d /etc/polkit-1/rules.d ]; then
		install -d -m 0755 /etc/polkit-1/rules.d
		install -m 0644 "$src/deploy/50-wakegate.rules" "$rules"
		polkit=1
	fi
	if [ -d /etc/polkit-1/localauthority ]; then
		install -d -m 0755 "$(dirname "$pkla")"
		install -m 0644 "$src/deploy/50-wakegate.pkla" "$pkla"
		polkit=1
	fi
elif [ -n "$agent_token" ]; then
	write_token "$agent_token"
fi

install -m 0644 "$src/deploy/wakegate-$role.service" "$units/wakegate-$role.service"
systemctl daemon-reload
restarted=
if systemctl is-active --quiet "wakegate-$role"; then
	systemctl restart "wakegate-$role"
	restarted=1
fi

echo
echo "wakegate $installed installed: $bin, $units/wakegate-$role.service."
if [ -n "$restarted" ]; then
	echo "wakegate-$role restarted."
	exit 0
fi
echo
if [ "$role" = agent ]; then
	if [ -z "$polkit" ]; then
		echo "Warning: polkit is not installed, so the agent cannot put this computer to sleep."
		echo "Install it (the polkitd or polkit package), then run this script again."
		echo
	fi
	echo "Next:"
	echo "  1. Check that Wake-on-LAN works before the agent puts this computer to sleep:"
	echo "     https://github.com/$repo/blob/main/docs/target.md"
	if [ -n "$new_config" ]; then
		echo "  2. Review $etc/agent.yaml: by default, suspend after 30 min without requests."
	else
		echo "  2. Review $etc/agent.yaml, kept from the previous installation."
	fi
	echo "  3. Start it:  sudo systemctl enable --now wakegate-agent"
	echo "  4. Install the proxy with this agent's token:"
	echo "     curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo sh -s -- proxy --agent-token $(cat "$token")"
else
	echo "Next:"
	echo "  1. Edit $etc/proxy.yaml: the target's address, MAC address and services."
	if [ ! -e "$token" ]; then
		echo "  2. Without an agent, comment out target.agent. With one, run this installation"
		echo "     again with --agent-token and the token the agent's installation printed."
	else
		echo "  2. The agent token is in $token."
	fi
	echo "  3. Check the configuration:  wakegate proxy -check"
	echo "  4. Start it:  sudo systemctl enable --now wakegate-proxy"
fi
