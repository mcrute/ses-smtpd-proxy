FROM golang:1.22 as build

WORKDIR /go/src/app
COPY . /go/src/app

# RUN go get -u && go mod tidy
RUN CGO_ENABLED=0 go build -o /go/bin/app

FROM gcr.io/distroless/static-debian12

COPY --from=build /go/bin/app /
CMD ["/app"]