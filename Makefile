BINARY ?= ses-smtpd-proxy
DOCKER_REGISTRY ?= docker.crute.me	
DOCKER_IMAGE_NAME ?= ses-email-proxy
DOCKER_TAG ?= latest
DOCKER_IMAGE ?= ${DOCKER_REGISTRY}/${DOCKER_IMAGE_NAME}:${DOCKER_TAG}
VERSION ?= $(shell git describe --long --tags --dirty --always)

$(BINARY): main.go go.sum smtpd/smtpd.go
	CGO_ENABLED=0 go build \
		-ldflags "-X main.version=$(VERSION)"  \
		-o $@ $<

.PHONY: docker
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE) .

.PHONY: publish
publish: docker
	docker push $(DOCKER_IMAGE)

go.sum: go.mod
	go mod tidy

.PHONY: clean
clean:
	rm $(BINARY) || true
