FROM golang:1.26-alpine AS build
WORKDIR /app
COPY main.go go.mod ./
RUN go build -o /handler .

FROM alpine:3.23
COPY --from=build /handler /handler
CMD ["/handler"]
