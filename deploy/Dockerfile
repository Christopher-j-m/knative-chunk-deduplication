FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/cache-service ./cmd/cache-service
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/cache-load-generator ./cmd/cache-load-generator

FROM alpine:3.23
RUN addgroup -S cache && adduser -S -G cache cache
COPY --from=build /out/cache-service /usr/local/bin/cache-service
COPY --from=build /out/cache-load-generator /usr/local/bin/cache-load-generator
USER cache
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cache-service"]
