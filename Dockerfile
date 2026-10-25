# Keep the Go version in step with mise.toml.
FROM golang:1.27.2 AS build
WORKDIR /src

COPY go.* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /edity-server ./cmd/edity-server

# The static image has no libc, so only a statically linked binary runs on it.
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /edity-server /edity-server

EXPOSE 8080
ENTRYPOINT ["/edity-server"]
