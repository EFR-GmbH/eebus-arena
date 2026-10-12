ARG GO_IMAGE=golang:1.27.1-bookworm@sha256:a4f46dc39c6b0359a3e1ed86ef14d01b374cc808649679dd5fca2290e6d54202
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY .arena/generated/enbility /src
WORKDIR /src/adapter
ENV GOTOOLCHAIN=local
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags='-s -w' -o /arena-node . \
    && go list -mod=readonly -m -json all > /dependencies.json
WORKDIR /src/runtime
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags='-s -w' -o /arena-supervisor ./cmd/arena-supervisor
FROM scratch
COPY --from=build /arena-node /arena-node
COPY --from=build /arena-supervisor /arena-supervisor
COPY --from=build /src/provenance.json /provenance.json
COPY --from=build /src/licenses /licenses
COPY --from=build /dependencies.json /dependencies.json
ENTRYPOINT ["/arena-supervisor", "/arena-node"]
