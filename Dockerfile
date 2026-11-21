FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/leave .

# alpine rather than distroless/static: sqlite is included so an operator can
# inspect /data/leave.db and take `.backup` copies with kamal app exec.
FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata sqlite \
 && adduser -D -H -u 10001 leave \
 && mkdir /data && chown leave /data
COPY --from=build /out/leave /usr/local/bin/leave
USER leave
ENV DB_PATH=/data/leave.db LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["leave"]
