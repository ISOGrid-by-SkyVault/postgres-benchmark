# Production image: a static binary in a distroless base, running as non-root.
# The binary is PID 1, so when it exits the container exits with its status.

# Stage 1: build
FROM golang:1.27-alpine AS build

WORKDIR /src

# Download dependencies first so this layer is cached until go.mod changes
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Stage 2: run
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server

ENV PORT=8080
EXPOSE 8080

# The image has no shell or curl: the binary probes its own /healthz.
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=3 CMD ["/server", "healthcheck"]

ENTRYPOINT ["/server"]
