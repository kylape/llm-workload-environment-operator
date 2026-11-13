FROM registry.access.redhat.com/ubi9/go-toolset:1.25 AS builder
USER root
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOMAXPROCS=2 GOFLAGS=-p=1 CGO_ENABLED=0 go build -o /manager ./cmd/manager

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.6 AS helm
COPY third_party/helm-v3.19.0-linux-amd64.tar.gz /tmp/helm.tar.gz
RUN microdnf install -y tar gzip && \
    echo "a7f81ce08007091b86d8bd696eb4d86b8d0f2e1b9f6c714be62f82f96a594496  /tmp/helm.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/helm.tar.gz -C /tmp && \
    install -m 0755 /tmp/linux-amd64/helm /usr/local/bin/helm && \
    mkdir -p /charts && \
    helm pull oci://quay.io/klape/charts/vcluster --version 0.0.1-kueue-gates.37b50bb1c --destination /charts && \
    echo "40998dcac8a9c84bfdb1216bb5154a7f9a4974c6c18f3dc84ab4839846693c7f  /charts/vcluster-0.0.1-kueue-gates.37b50bb1c.tgz" | sha256sum -c -

FROM registry.access.redhat.com/ubi9/ubi-micro:9.6
WORKDIR /
COPY --from=builder /manager /manager
COPY --from=helm /usr/local/bin/helm /helm
COPY --from=helm /charts/vcluster-0.0.1-kueue-gates.37b50bb1c.tgz /charts/vcluster-0.0.1-kueue-gates.37b50bb1c.tgz
USER 65532:65532
ENTRYPOINT ["/manager"]
