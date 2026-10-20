FROM golang:1.27-alpine AS build
# gcc and musl-dev: go-sqlite3 needs cgo.
RUN apk add --no-cache gcc musl-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/leave .

# alpine rather than distroless/static: the cgo binary links musl. sqlite is
# included so an operator can inspect /data/leave.db with docker exec.
FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata sqlite \
 && adduser -D -H -u 10001 leave \
 && mkdir /data && chown leave /data
COPY --from=build /out/leave /usr/local/bin/leave
USER leave
ENV DB_PATH=/data/leave.db LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["leave"]
