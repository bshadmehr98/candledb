# Two stages: the first has the Go toolchain and builds the binary, the
# second is the image that ships and contains only the binary.

# --platform=$BUILDPLATFORM runs this stage on the machine's own
# architecture even when the image is for another one. Go cross-compiles
# (see GOOS and GOARCH below), which is far faster than emulating a CPU.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
# Copying go.mod and go.sum first lets Docker reuse the downloaded modules
# for every build in which only the source changed.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# Docker sets these two to the platform the image is being built for.
ARG TARGETOS TARGETARCH
# CGO_ENABLED=0 produces a static binary with no dependency on system
# libraries, which is what allows the empty base image below.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /candledb ./cmd/candledb
RUN mkdir /data

# distroless/static has no shell and no package manager: nothing to exploit,
# nothing to update. "nonroot" runs the server as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /candledb /candledb
COPY --from=build --chown=nonroot:nonroot /data /data
ENV CANDLEDB_DATA_DIR=/data
VOLUME /data
EXPOSE 8090
ENTRYPOINT ["/candledb"]
CMD ["serve"]

# Labels that registries read: they link the image to its source and licence.
LABEL org.opencontainers.image.source="https://github.com/bshadmehr98/candledb" \
      org.opencontainers.image.description="A lightweight realtime document database" \
      org.opencontainers.image.licenses="MIT"
