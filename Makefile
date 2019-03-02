DOCKER_REGISTRY ?= docker.crute.me
DOCKER_IMAGE_NAME ?= ses-email-proxy
DOCKER_TAG ?= latest

DOCKER_IMAGE_SPEC = $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)

.PHONY: docker
docker: ses-smtpd-proxy
	docker build -t $(DOCKER_IMAGE_SPEC) .

.PHONY: publish
publish: docker
	docker push $(DOCKER_IMAGE_SPEC)

ses-smtpd-proxy: main.go
	CGO_ENABLED=0 go build -o $@ $<
