FROM alpine:latest AS builder

RUN apk add --no-cache make go git 
COPY . ./ses-smtpd-proxy
WORKDIR /ses-smtpd-proxy
RUN make ses-smtpd-proxy

FROM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder ses-smtpd-proxy /

ENTRYPOINT [ "/ses-smtpd-proxy" ]
