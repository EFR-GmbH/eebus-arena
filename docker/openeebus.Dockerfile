ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
ARG GO_IMAGE=golang:1.27.1-bookworm@sha256:a4f46dc39c6b0359a3e1ed86ef14d01b374cc808649679dd5fca2290e6d54202
FROM ${GO_IMAGE} AS certificates
WORKDIR /runtime
COPY .arena/generated/openeebus/runtime /runtime
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags='-s -w' -o /arena-supervisor ./cmd/arena-supervisor
FROM ${DEBIAN_IMAGE} AS build
ARG DEBIAN_SNAPSHOT
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN rm /etc/apt/sources.list.d/debian.sources && printf 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/%s/ bookworm main\n' "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list
RUN apt-get update && apt-get install -y --no-install-recommends \
    cmake=3.25.1-1 \
    make=4.3-4.1 \
    gcc=4:12.2.0-3 \
    pkg-config=1.8.1-1 \
    libcjson-dev=1.7.15-1+deb12u4 \
    libssl-dev=3.0.20-1~deb12u2 \
    libavahi-compat-libdnssd-dev=0.8-10+deb12u1 \
    ca-certificates=20230311+deb12u1 \
    zlib1g-dev=1:1.2.13.dfsg-1 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY .arena/generated/openeebus /src
RUN cmake -S libwebsockets -B lws-build -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_FLAGS_RELEASE="-O2 -DNDEBUG" -DCMAKE_EXPORT_COMPILE_COMMANDS=ON -DLWS_IPV6=ON -DLWS_WITHOUT_TESTAPPS=ON -DLWS_WITH_LIBUV=OFF -DLWS_WITH_GLIB=OFF -DLWS_WITH_LIBEV=OFF && cmake --build lws-build -j 4 && cmake --install lws-build --strip
RUN cmake -S upstream -B build -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_FLAGS_RELEASE="-O2 -DNDEBUG" -DOPTION_ASAN=OFF && cmake --build build --target arena-node -j 4 && strip --strip-unneeded build/arena-node
RUN dpkg-query -W > /dependencies.txt
FROM ${DEBIAN_IMAGE}
ARG DEBIAN_SNAPSHOT
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN rm /etc/apt/sources.list.d/debian.sources && printf 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/%s/ bookworm main\n' "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list
RUN apt-get update && apt-get install -y --no-install-recommends \
    libssl3=3.0.20-1~deb12u2 \
    zlib1g=1:1.2.13.dfsg-1 \
    libcjson1=1.7.15-1+deb12u4 \
    libavahi-compat-libdnssd1=0.8-10+deb12u1 \
    avahi-daemon=0.8-10+deb12u1 \
    dbus=1.14.10-1~deb12u1 \
    libnss-mdns=0.15.1-3 \
    && rm -rf /var/lib/apt/lists/* && mkdir -p /run/dbus
COPY --from=build /usr/local/lib/libwebsockets.so* /usr/local/lib/
RUN ldconfig && sed -i "s/^hosts:.*/hosts: files mdns_minimal [NOTFOUND=return] dns/" /etc/nsswitch.conf
COPY --from=build /src/build/arena-node /arena-node
COPY --from=certificates /arena-supervisor /arena-supervisor
COPY --from=build /src/build/compile_commands.json /build-info/openeebus-compile-commands.json
COPY --from=build /src/lws-build/compile_commands.json /build-info/libwebsockets-compile-commands.json
COPY --from=build /src/provenance.json /provenance.json
COPY --from=build /src/licenses /licenses
COPY --from=build /dependencies.txt /dependencies.txt
ENTRYPOINT ["/arena-supervisor", "/arena-node"]
