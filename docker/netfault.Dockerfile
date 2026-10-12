ARG GO_IMAGE=golang:1.27.1-bookworm@sha256:a4f46dc39c6b0359a3e1ed86ef14d01b374cc808649679dd5fca2290e6d54202
ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/arena-netfault ./cmd/arena-netfault
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags='-s -w' -o /arena-netfault ./cmd/arena-netfault
FROM ${DEBIAN_IMAGE}
ARG DEBIAN_SNAPSHOT
ARG PROVENANCE
LABEL org.eebus-arena.provenance=$PROVENANCE
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN rm /etc/apt/sources.list.d/debian.sources && printf 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/%s/ bookworm main\n' "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list
RUN apt-get update && apt-get install -y --no-install-recommends iproute2=6.1.0-3 && rm -rf /var/lib/apt/lists/*
COPY --from=build /arena-netfault /arena-netfault
ENTRYPOINT ["/arena-netfault"]
