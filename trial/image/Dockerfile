# MMC trial image: Ubuntu 24.04 arm64 + GitHub Actions runner.
FROM ubuntu:24.04@sha256:73b999393d07f2bf65a40373778a44dc9a6d54ad7718d29247d8ff9413c4d6a8

ARG RUNNER_VERSION=2.338.2
ARG RUNNER_SHA256=670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl git \
 && useradd -m -s /bin/bash runner \
 && curl -fsSL -o /tmp/runner.tgz "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-arm64-${RUNNER_VERSION}.tar.gz" \
 && echo "${RUNNER_SHA256}  /tmp/runner.tgz" | sha256sum -c - \
 && tar -xzf /tmp/runner.tgz -C /home/runner \
 && rm /tmp/runner.tgz \
 && /home/runner/bin/installdependencies.sh \
 && chown -R runner:runner /home/runner \
 && rm -rf /var/lib/apt/lists/*

COPY --chmod=755 mmc-jit-files /usr/local/bin/mmc-jit-files
COPY --chmod=755 entrypoint.sh /usr/local/bin/mmc-runner

LABEL mmc.runner.version="${RUNNER_VERSION}"
USER runner
WORKDIR /home/runner
ENTRYPOINT ["/usr/local/bin/mmc-runner"]
