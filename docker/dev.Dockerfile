# Keep the formatter runtime separate from the release images.
FROM golang:1.27.1-bookworm@sha256:a4f46dc39c6b0359a3e1ed86ef14d01b374cc808649679dd5fca2290e6d54202 AS go
FROM eclipse-temurin:21-jdk-jammy@sha256:e0c60c487345d1dc9d0fc7b6f0496f3cc941e5132e09296cc17a6decc71b902b AS java
FROM python:3.14.8-slim-bookworm@sha256:48b13b003dda20b16f9442b8475aa05fe21bf6579a8c881db92ffb4d8fd20f83

# Debian fixes the clang-format major version; Ruff and Java formatting are
# pinned because changing a formatter can otherwise rewrite the whole tree.
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends \
    clang-format-14 cppcheck gcc git libc6-dev make cmake pkg-config \
    libcjson-dev libssl-dev libavahi-compat-libdnssd-dev zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir ruff==0.16.10
COPY --from=go /usr/local/go /usr/local/go
COPY --from=java /opt/java/openjdk /opt/java/openjdk
ADD --chmod=644 --checksum=sha256:32342e7c1b4600f80df3471da46aee8012d3e1445d5ea1be1fb71289b07cc735 https://repo.maven.apache.org/maven2/com/google/googlejavaformat/google-java-format/1.28.0/google-java-format-1.28.0-all-deps.jar /opt/google-java-format.jar
ADD --chmod=644 --checksum=sha256:9d926787066a081739e8200858338b4a69e837c3a821a33aca9db09dd4a41026 https://services.gradle.org/distributions/gradle-8.5-bin.zip /tmp/gradle.zip
WORKDIR /opt/gradle
RUN /opt/java/openjdk/bin/jar xf /tmp/gradle.zip \
    && chmod +x /opt/gradle/gradle-8.5/bin/gradle && rm /tmp/gradle.zip
ENV PATH="/usr/local/go/bin:/opt/java/openjdk/bin:${PATH}" \
    JAVA_HOME=/opt/java/openjdk GOTOOLCHAIN=local \
    GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-modules RUFF_CACHE_DIR=/tmp/ruff-cache
# VS Code remaps this named user's UID/GID to the host account on Linux. Keep
# the image non-root too, so terminals and lifecycle commands share that user.
RUN groupadd --gid 1000 vscode \
    && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash vscode
USER 1000:1000
WORKDIR /workspace
CMD ["python3", "scripts/dev.py", "check"]
