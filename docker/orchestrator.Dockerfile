ARG GO_IMAGE=golang:1.27.1-bookworm@sha256:a4f46dc39c6b0359a3e1ed86ef14d01b374cc808649679dd5fca2290e6d54202
ARG NODE_IMAGE=node:24.21.0-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20
ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
ARG DOCKER_IMAGE=docker:29.7.2-cli@sha256:3f4743208d2338c934d7b8bcfbe1bb54c0b2355c510ad5e0f31c0c4a54bd704e
ARG COMPOSE_IMAGE=docker/compose-bin:v5.4.0@sha256:b03e46987ca4ebb41ca31b765ad7ba957388003f1ef1255fd68333a7b838d632
FROM ${NODE_IMAGE} AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /internal/webui/static ./internal/webui/static
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /arena ./cmd/arena
FROM ${DOCKER_IMAGE} AS docker
FROM ${COMPOSE_IMAGE} AS compose
FROM ${GO_IMAGE} AS tools
COPY --from=docker /usr/local/bin/docker /tools/docker
COPY --from=compose /docker-compose /tools/docker-compose
RUN strip --strip-unneeded /tools/docker /tools/docker-compose
FROM ${DEBIAN_IMAGE}
COPY --from=tools /tools/docker /usr/local/bin/docker
COPY --from=tools /tools/docker-compose /usr/local/lib/docker/cli-plugins/docker-compose
COPY --from=build /arena /arena
ENV ARENA_DATA=/data ARENA_LISTEN=:8080
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/arena"]
