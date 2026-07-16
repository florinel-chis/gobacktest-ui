# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26-alpine AS build
# git is needed when a module isn't in the Go proxy cache yet and go falls
# back to fetching it directly from GitHub.
RUN apk add --no-cache git
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gobacktest-ui .

# ---- runtime stage ----
# distroless/static ships CA certificates (needed for the live Yahoo/OANDA
# HTTPS calls) and nothing else; the app is a single static binary with the
# frontend embedded.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gobacktest-ui /gobacktest-ui

EXPOSE 8080
ENTRYPOINT ["/gobacktest-ui"]
# Inside a container the server must bind all interfaces, not the default
# loopback — reachability is controlled by how the port is published.
CMD ["-addr", ":8080"]
