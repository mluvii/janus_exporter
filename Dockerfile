ARG ARCH="amd64"
ARG OS="linux"

FROM golang as builder
WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o janus_exporter .

FROM quay.io/prometheus/busybox-${OS}-${ARCH}:glibc

COPY --from=builder /build/janus_exporter /bin/

USER nobody
ENTRYPOINT ["/bin/janus_exporter"]
