FROM golang:1.16 as builder
COPY . /workdir
WORKDIR /workdir
RUN GOPROXY=https://goproxy.cn go build -o bin/minio-cosi-driver ./cmd/minio-cosi-driver

FROM build-harbor.alauda.cn/ops/centos:7.9.2009
LABEL maintainers="Kubernetes COSI Authors"
LABEL description="MinIO COSI driver"

COPY --from=builder /workdir/bin/minio-cosi-driver /minio-cosi-driver
ENTRYPOINT ["/minio-cosi-driver"]
