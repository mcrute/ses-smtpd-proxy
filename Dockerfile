FROM golang:1.18 as build

WORKDIR /go/src/ses-smtpd-proxy
COPY . .

RUN go mod download
RUN go vet -v
RUN go test -v

RUN CGO_ENABLED=0 go build -o ses-smtpd-proxy

FROM gcr.io/distroless/static-debian11

ADD ses-smtpd-proxy /

CMD [ "/ses-smtpd-proxy" ]
