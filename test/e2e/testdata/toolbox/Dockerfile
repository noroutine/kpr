# E2E toolbox: one container with every binary-only client the
# scenarios shell out to (oras, skopeo). Built once per package run
# (TestMain) from stock images; the build cache keeps rebuilds
# instant, and the container is terminated after the run.
#
# Go-library clients (ggcr, crane, regclient) stay in-process — same
# code, zero install. The docker CLI must stay host-bound: pushes
# originate inside the daemon, which cannot reach fixture ports from
# Docker Desktop's VM.
#
# The final stage is the skopeo image (not alpine): skopeo links
# system libraries that would not survive a bare-binary copy, while
# the statically built oras binary runs anywhere.
FROM ghcr.io/oras-project/oras:v1.3.4 AS oras
FROM quay.io/skopeo/stable:latest
COPY --from=oras /bin/oras /usr/local/bin/oras
CMD ["sleep", "infinity"]
