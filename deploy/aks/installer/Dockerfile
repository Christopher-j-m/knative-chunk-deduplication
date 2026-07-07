# Carries the patched static nydus binaries + node configs + installer script
# to AKS nodes. The main container is a sleep loop used as the host-exec
# vehicle by the config switcher, reset, and benchmark scripts.
# Build context = repo root; run scripts/fetch-nydus-binaries.sh first so
# deploy/local/bin is populated (its containerd-nydus-grpc carries the
# REQUIRED http-proxy backend patch — never substitute the stock release).
FROM alpine:3.23
COPY deploy/local/bin/containerd-nydus-grpc /payload/containerd-nydus-grpc
COPY deploy/local/bin/nydusd /payload/nydusd
COPY deploy/local/bin/nydus-image /payload/nydus-image
COPY deploy/aks/installer/config.toml /payload/config.toml
COPY deploy/aks/installer/nydusd-config.json /payload/nydusd-config.json
COPY deploy/aks/installer/nydus-snapshotter.service /payload/nydus-snapshotter.service
COPY deploy/aks/installer/install.sh /payload/install.sh
