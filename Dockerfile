# Build the manager binary
FROM golang:1.15-alpine3.12 AS builder
ARG GOPROXY
WORKDIR /go/pixiu-autoscaler
COPY . .
RUN CGO_ENABLED=0 GOPROXY=${GOPROXY} go build -a -o pixiu-autoscaler-controller cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
#FROM gcr.io/distroless/static:nonroot
FROM jacky06/static:nonroot
WORKDIR /
COPY --from=builder /go/pixiu-autoscaler/pixiu-autoscaler-controller /usr/local/bin/pixiu-autoscaler-controller
USER 65532:65532
