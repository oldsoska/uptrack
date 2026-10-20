# 1. Build a static binary
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off: no libc dependency, so the binary runs on an image without one
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/uptrack ./cmd/uptrack

# 2. Runtime: distroless has CA certificates and tzdata but no shell or package manager
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/uptrack /uptrack
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/uptrack", "healthcheck"]
ENTRYPOINT ["/uptrack"]
