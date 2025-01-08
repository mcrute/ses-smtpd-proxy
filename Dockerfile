FROM golang:1.23-alpine AS builder
ARG VERSION
RUN apk add --no-cache make 
COPY . .
RUN VERSION=$VERSION make ses-smtpd-proxy

FROM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder /go/ses-smtpd-proxy /

ENTRYPOINT [ "/ses-smtpd-proxy" ]
