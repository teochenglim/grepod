# --- build stage ---
FROM golang:1.26-alpine AS build
WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/grepod ./cmd/server

RUN mkdir -p /out/data && chown -R 65532:65532 /out/data

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/grepod /grepod
COPY --from=build --chown=65532:65532 /out/data /data

USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/grepod"]
