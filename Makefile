ORG ?= jacky06
TARGET_DIR ?= ./dist
BUILDX ?= false
PLATFORM ?= linux/amd64,linux/arm64
TAG ?= latest
OS ?= linux
ARCH ?= amd64
IMAGE ?= $(ORG)/pixiu-autoscaler-controller:$(TAG)
GOPROXY ?=

.PHONY: build image push-image

build:
	CGO_ENABLED=0 GOOS=$(OS) GOARCH=$(ARCH) GOPROXY=$(GOPROXY) go build -o $(TARGET_DIR)/pixiu-autoscaler-controller ./cmd

image:
ifeq ($(BUILDX), false)
	docker build \
		--build-arg GOPROXY=$(GOPROXY) \
		--force-rm \
		--no-cache \
		-t $(IMAGE) .
else
	docker buildx build \
		--force-rm \
		--no-cache \
		--platform $(PLATFORM) \
		--push \
		-t $(IMAGE) .
endif

push-image:
	docker push $(IMAGE)

clean:
	rm -rf $(TARGET_DIR)
