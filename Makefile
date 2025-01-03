BINARY ?= ses-smtpd-proxy
DOCKER_REGISTRY ?= docker.crute.me	
DOCKER_IMAGE_NAME ?= ses-email-proxy
DOCKER_TAG ?= latest
DOCKER_IMAGE ?= ${DOCKER_REGISTRY}/${DOCKER_IMAGE_NAME}:${DOCKER_TAG}

$(BINARY): main.go go.sum smtpd/smtpd.go
	CGO_ENABLED=0 go build \
		-ldflags "-X main.version=$(shell git describe --long --tags --dirty --always)"  \
		-o $@ $<

.PHONY: docker
docker:
	docker build -t $(DOCKER_IMAGE) .

.PHONY: publish
publish: docker
	docker push $(DOCKER_IMAGE)

go.sum: go.mod
	go mod tidy

.PHONY: clean
clean:
	rm $(BINARY) || true
