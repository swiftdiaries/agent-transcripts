# syntax=docker/dockerfile:1.7
# Pin image digests; renovate/dependabot should update them deliberately.
FROM --platform=$BUILDPLATFORM golang:1.23.12-alpine@sha256:de28dfe8febbe1e0a9c80abe083380f1a9ac07d532307bbf3e6a3d1c32fad17f AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/agent-transcripts ./cmd/agent-transcripts

FROM --platform=$TARGETPLATFORM gcr.io/distroless/static-debian12:nonroot@sha256:c7742da01aa7ee169d59e58a91c35da9c13e67f555dcd8b2ada15887aa619e6c

COPY --from=build /out/agent-transcripts /agent-transcripts
USER nonroot:nonroot
ENTRYPOINT ["/agent-transcripts"]
