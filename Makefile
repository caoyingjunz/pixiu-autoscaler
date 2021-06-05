ORG ?= jacky06
TARGET_DIR ?= ./dist
BUILDX ?= false
PLATFORM ?= linux/amd64,linux/arm64
TAG ?= latest
OS ?= linux
ARCH ?= amd64
IMAGE ?= $(ORG)/kubez-autoscaler-controller:$(TAG)

.PHONY: build image push-image

build:
	CGO_ENABLED=0 GOOS=$(OS) GOARCH=$(ARCH) go build -o $(TARGET_DIR)/kubez-autoscaler-controller ./cmd

image:
ifeq ($(BUILDX), false)
	docker build \
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
