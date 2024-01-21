ARG GO_VERSION=1.20
ARG BATS_VERSION=v1.9.0
ARG LIBSECCOMP_VERSION=2.5.4

FROM golang:${GO_VERSION}-bullseye
ARG DEBIAN_FRONTEND=noninteractive
ARG CRIU_REPO=https://download.opensuse.org/repositories/devel:/tools:/criu/Debian_11

RUN KEYFILE=/usr/share/keyrings/criu-repo-keyring.gpg; \
    wget -nv $CRIU_REPO/Release.key -O- | gpg --dearmor > "$KEYFILE" \
    && echo "deb [signed-by=$KEYFILE] $CRIU_REPO/ /" > /etc/apt/sources.list.d/criu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        gnupg \
        gnupg2 \
        build-essential \
        gcc \
        curl \
        gawk \
        gperf \
        iptables \
        jq \
        kmod \
        pkg-config \
        python3-minimal \
        sshfs \
        sudo \
        uidmap \
        iproute2 \
    && apt-get install -y --no-install-recommends \
        gcc-aarch64-linux-gnu libc-dev-arm64-cross \
    && apt-get clean \
    && rm -rf /var/cache/apt /var/lib/apt/lists/* /etc/apt/sources.list.d/*.list

# Add a dummy user for the rootless integration tests. While runC does
# not require an entry in /etc/passwd to operate, one of the tests uses
# `git clone` -- and `git clone` does not allow you to clone a
# repository if the current uid does not have an entry in /etc/passwd.
RUN useradd -u1000 -m -d/home/rootless -s/bin/bash rootless

# install bats
ARG BATS_VERSION
RUN cd /tmp \
    && git clone https://github.com/bats-core/bats-core.git \
    && cd bats-core \
    && git reset --hard "${BATS_VERSION}" \
    && ./install.sh /usr/local \
    && rm -rf /tmp/bats-core

# install libseccomp
ARG LIBSECCOMP_VERSION
COPY script/seccomp.sh script/lib.sh /tmp/script/
RUN mkdir -p /opt/libseccomp \
    && /tmp/script/seccomp.sh "$LIBSECCOMP_VERSION" /opt/libseccomp arm64
ENV LIBSECCOMP_VERSION=$LIBSECCOMP_VERSION
ENV LD_LIBRARY_PATH=/opt/libseccomp/lib
ENV PKG_CONFIG_PATH=/opt/libseccomp/lib/pkgconfig

# Prevent the "fatal: detected dubious ownership in repository" git complain during build.
RUN git config --global --add safe.directory /go/src/github.com/szcdx/runc

WORKDIR /go/src/github.com/szcdx/runc

# Fixup for cgroup v2.
COPY script/prepare-cgroup-v2.sh /
ENTRYPOINT [ "/prepare-cgroup-v2.sh" ]
