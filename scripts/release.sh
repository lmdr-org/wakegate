#!/bin/sh
# Builds the release archives into dist/: one per architecture, each holding the static
# binary, install.sh, deploy/, README.md and LICENSE, plus SHA256SUMS.
#
#   scripts/release.sh v0.1.0
set -eu
cd "$(dirname "$0")/.."
version=${1:?usage: scripts/release.sh vX.Y.Z}
rm -rf dist
mkdir -p dist
for target in amd64 arm64 arm:7 arm:6; do
	arch=${target%%:*}
	arm=
	name=wakegate-linux-$arch
	if [ "$arch" = arm ]; then
		arm=${target#arm:}
		name=wakegate-linux-armv$arm
	fi
	mkdir -p "dist/$name"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOARM=$arm \
		go build -trimpath -ldflags "-s -w -X main.version=$version" -o "dist/$name/wakegate" .
	cp -r install.sh deploy README.md LICENSE "dist/$name/"
	tar -czf "dist/$name.tar.gz" -C dist --owner=0 --group=0 "$name"
	rm -rf "dist/$name"
done
(cd dist && sha256sum ./*.tar.gz | sed 's| \./| |' >SHA256SUMS)
cat dist/SHA256SUMS
