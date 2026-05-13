# syntax=docker/dockerfile:1.7

# Stage 1: build with the same Go version as go.mod.
FROM golang:1.26-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
        -ldflags="-s -w \
            -X github.com/mamidevs/flotilla/internal/buildinfo.AppVersion=${VERSION} \
            -X github.com/mamidevs/flotilla/internal/buildinfo.CommitHash=${COMMIT} \
            -X github.com/mamidevs/flotilla/internal/buildinfo.BuildDate=${BUILD_DATE} \
            -X github.com/mamidevs/flotilla/internal/buildinfo.BuildId=docker \
            -X github.com/mamidevs/flotilla/internal/buildinfo.Production=1" \
        -o /out/flotilla ./cmd/flotilla

# Stage 2: distroless runtime. nonroot UID is 65532.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/flotilla /usr/local/bin/flotilla
VOLUME ["/state"]
EXPOSE 5040 5050 8080
ENTRYPOINT ["/usr/local/bin/flotilla"]
CMD ["run", "--config", "/etc/flotilla/flotilla.yaml"]
