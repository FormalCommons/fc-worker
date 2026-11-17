# The agent image: hive-agent, plus gh, a git credential helper and a gh
# wrapper that fetch GitHub tokens from hive-broker, and elan for Lean.
# Build from the repository root: docker build -f image/Dockerfile image
ARG HIVE_AGENT=ghcr.io/formalcommons/hive-agent:sha-5cf0bfe
FROM ${HIVE_AGENT}

ARG GH_VERSION=2.103.3
ARG GH_SHA256=4c806978ffce1cec70853302a56d70ac6da4f19bae7adca4e1a8ab00633dd91d
ARG ELAN_VERSION=4.2.4
# elan publishes no checksum file; this is the release asset's SHA-256, as
# GitHub reports it.
ARG ELAN_SHA256=42b94d4244e8353142c456ec0e4ca6528fd898a6c604d4059f494e706e431f63

USER root
# elan lives in /opt/elan rather than the home directory, so that a cache
# of toolchains can be mounted at /opt/elan/toolchains.
ENV ELAN_HOME=/opt/elan PATH=/opt/elan/bin:$PATH
RUN set -eu; cd /tmp \
 && curl -fsSLo gh.tar.gz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" \
 && echo "${GH_SHA256}  gh.tar.gz" | sha256sum -c - \
 && mkdir /opt/gh && tar -xzf gh.tar.gz -C /opt/gh --strip-components=1 --no-same-owner \
 && curl -fsSLo elan.tar.gz "https://github.com/leanprover/elan/releases/download/v${ELAN_VERSION}/elan-x86_64-unknown-linux-gnu.tar.gz" \
 && echo "${ELAN_SHA256}  elan.tar.gz" | sha256sum -c - \
 && tar -xzf elan.tar.gz --no-same-owner \
 && ./elan-init -y --no-modify-path --default-toolchain none \
 && chown -R 1000:1000 /opt/elan \
 && install -d -o 1000 -g 1000 /home/node/.cache /home/node/.cache/mathlib \
 && rm gh.tar.gz elan.tar.gz elan-init
COPY fc-token git-credential-fc gh /usr/local/bin/
RUN git config --system credential.https://github.com.helper fc
USER 1000:1000
