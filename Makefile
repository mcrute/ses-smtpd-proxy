BINARY ?= ses-smtpd-proxy
DOCKER_IMAGE ?= docker.crute.me/ses-email-proxy:latest

$(BINARY): main.go go.sum smtpd/smtpd.go
	CGO_ENABLED=0 go build \
		-ldflags "-X main.version=$(shell git describe --long --tags --dirty --always)"  \
		-o $@ $<

.PHONY: docker
docker: $(BINARY)
	docker build -t $(DOCKER_IMAGE) .

.PHONY: publish
publish: docker
	docker push $(DOCKER_IMAGE)

go.sum: go.mod
	go mod tidy

.PHONY: clean
clean:
	rm $(BINARY) || true
