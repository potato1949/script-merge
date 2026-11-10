FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /script-merge .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 1000 app && mkdir /data && chown app /data
COPY --from=build /script-merge /usr/local/bin/script-merge
USER app
ENV PORT=8080 DATA_DIR=/data
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["script-merge"]
