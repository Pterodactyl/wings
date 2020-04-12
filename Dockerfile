# ----------------------------------
# Pterodactyl Panel Dockerfile
# ----------------------------------

FROM golang:1.14-alpine
COPY . /go/wings/
WORKDIR /go/wings/
RUN go build

FROM alpine:latest
COPY --from=0 /go/wings/wings /usr/bin/
CMD ["wings", "--config", "/srv/daemon/config.yml"]