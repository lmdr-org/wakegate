# The proxy as a container image. It needs the host's network: magic packets are
# broadcasts, and services listen on the host's ports.
#
#   docker run -d --name wakegate --network host --restart unless-stopped \
#     -v ./proxy.yaml:/etc/wakegate/proxy.yaml:ro \
#     -v ./agent.token:/etc/wakegate/agent.token:ro \
#     -v wakegate-state:/var/lib/wakegate \
#     ghcr.io/clauer-org/wakegate
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# timetzdata: the awake window follows TZ, and the image has no time zone files.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -tags timetzdata -ldflags "-s -w -X main.version=$VERSION" -o /wakegate . \
 && mkdir -p /state

FROM scratch
COPY --from=build /wakegate /wakegate
COPY --from=build --chown=65534:65534 /state /var/lib/wakegate
USER 65534:65534
ENTRYPOINT ["/wakegate"]
CMD ["proxy", "-config", "/etc/wakegate/proxy.yaml"]
