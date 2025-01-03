FROM alpine:latest AS builder

RUN apk add --no-cache make go git 
COPY . ./app
WORKDIR /app
RUN make ses-smtpd-proxy

FROM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder /app/ses-smtpd-proxy /

ENTRYPOINT [ "/ses-smtpd-proxy" ]
