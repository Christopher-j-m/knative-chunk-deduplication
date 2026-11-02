# Container image that contains the nydus setup we need on every node
FROM alpine:3.23
COPY deploy/local/bin/containerd-nydus-grpc /payload/containerd-nydus-grpc
COPY deploy/local/bin/nydusd /payload/nydusd
COPY deploy/local/bin/nydus-image /payload/nydus-image
COPY deploy/aks/installer/config.toml /payload/config.toml
COPY deploy/aks/installer/nydusd-config.json /payload/nydusd-config.json
COPY deploy/aks/installer/nydus-snapshotter.service /payload/nydus-snapshotter.service
COPY deploy/aks/installer/install.sh /payload/install.sh
