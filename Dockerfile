# Two stages: the first has the Go toolchain and builds the binary, the
# second is the image that ships and contains only the binary.

FROM golang:1.27 AS build
WORKDIR /src
# Copying go.mod and go.sum first lets Docker reuse the downloaded modules
# for every build in which only the source changed.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# CGO_ENABLED=0 produces a static binary with no dependency on system
# libraries, which is what allows the empty base image below.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /candledb ./cmd/candledb
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
