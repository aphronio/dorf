FROM debian:13.6-slim@sha256:9bb8a3626890e084ab54e888fdd7c4b6d2f119071cd4c5dc5fecb4d73062aa5f
ARG DORF_RELEASE
ARG DORF_BINARY_SHA256
LABEL org.opencontainers.image.title="Dorf"
LABEL org.opencontainers.image.version="${DORF_RELEASE}"
LABEL dev.dorf.binary-sha256="${DORF_BINARY_SHA256}"
RUN sed -i \
        -e 's|http://deb.debian.org/debian-security|http://snapshot.debian.org/archive/debian-security/20260713T000000Z|g' \
        -e 's|http://deb.debian.org/debian|http://snapshot.debian.org/archive/debian/20260713T000000Z|g' \
        /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::Check-Valid-Until=false update \
    && apt-get install --yes --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/dorf \
    && chmod 0755 /var/lib/dorf
COPY --chmod=0555 dorf /usr/local/bin/dorf
ENV HOME=/var/lib/dorf
ENTRYPOINT ["/usr/local/bin/dorf"]
