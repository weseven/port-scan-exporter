# Build the exporter binary
FROM golang:1.20 as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -a -o exporter main.go

# Use distroless as minimal base image to package the exporter binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/exporter .
USER 65532:65532

ENTRYPOINT ["/exporter"]
