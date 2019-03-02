FROM alpine:latest

RUN apk add --no-cache ca-certificates
ADD ses-smtpd-proxy /

CMD [ "/ses-smtpd-proxy" ]
