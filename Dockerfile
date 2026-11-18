# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go tool templ generate \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/yt-subscriptions . \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/yt-subscriptions /app/yt-subscriptions
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV HOST=0.0.0.0 \
    PORT=8080 \
    DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/app/yt-subscriptions"]
