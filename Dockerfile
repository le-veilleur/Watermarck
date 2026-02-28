FROM golang:1.26.0-alpine3.23 AS build

ARG SERVICE_DIR
ARG CMD_PATH

WORKDIR /usr/src/${SERVICE_DIR}

COPY ${SERVICE_DIR}/go.mod ${SERVICE_DIR}/go.sum ./

RUN go mod download
COPY ${SERVICE_DIR}/ .

RUN CGO_ENABLED=0 go build -o /usr/local/bin/service .

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/local/bin/service /usr/local/bin/service

CMD ["/usr/local/bin/service"]
