ARG JDK_IMAGE=eclipse-temurin:21-jdk-jammy@sha256:e0c60c487345d1dc9d0fc7b6f0496f3cc941e5132e09296cc17a6decc71b902b
ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
ARG GO_IMAGE=golang:1.27.1-bookworm@sha256:a4f46dc39c6b0359a3e1ed86ef14d01b374cc808649679dd5fca2290e6d54202
FROM ${JDK_IMAGE} AS build
ADD --checksum=sha256:9d926787066a081739e8200858338b4a69e837c3a821a33aca9db09dd4a41026 https://services.gradle.org/distributions/gradle-8.5-bin.zip /tmp/gradle.zip
WORKDIR /opt/gradle
RUN jar xf /tmp/gradle.zip && chmod +x /opt/gradle/gradle-8.5/bin/gradle && rm /tmp/gradle.zip
WORKDIR /src
COPY .arena/generated/jeebus /src
WORKDIR /src/upstream
RUN /opt/gradle/gradle-8.5/bin/gradle --no-daemon :demo:installDist --dependency-verification strict \
    && /opt/java/openjdk/bin/jlink --add-modules java.base,java.logging,java.naming,java.management,java.xml,java.desktop,java.net.http,jdk.crypto.ec,jdk.unsupported,jdk.zipfs --strip-debug --no-header-files --no-man-pages --compress=zip-6 --output /opt/arena-jre
FROM ${GO_IMAGE} AS runtime
WORKDIR /runtime
COPY .arena/generated/jeebus/runtime /runtime
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags='-s -w' -o /arena-supervisor ./cmd/arena-supervisor
COPY --from=build /opt/arena-jre /opt/arena-jre
RUN strip --strip-unneeded /opt/arena-jre/bin/* /opt/arena-jre/lib/*.so /opt/arena-jre/lib/server/*.so
FROM ${DEBIAN_IMAGE}
COPY --from=runtime /arena-supervisor /arena-supervisor
COPY --from=runtime /opt/arena-jre /opt/java
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV JAVA_HOME=/opt/java PATH=/opt/java/bin:$PATH
COPY --from=build /src/upstream/projects/demo/build/install/demo /app
COPY --from=build /src/provenance.json /provenance.json
COPY --from=build /src/release-sources.json /build-info/jeebus-sources.json
COPY --from=build /src/licenses /licenses
COPY --from=build /src/upstream/gradle/verification-metadata.xml /verification-metadata.xml
ENV JAVA_OPTS="-Xms16m -Xmx128m -XX:MaxMetaspaceSize=128m -XX:ActiveProcessorCount=2 -XX:+UseSerialGC"
ENTRYPOINT ["/arena-supervisor", "/app/bin/demo"]
