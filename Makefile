# The gateway checkout this builds against: providerkit, and the linter.
GATEWAY ?= ../../mcplocker
VERSION ?= $(shell sed -n 's/^version: //p' mcplocker-provider.yaml)

.PHONY: test lint build image smoke

test:
	go vet ./...
	go test ./...

lint:
	cd $(GATEWAY) && go run ./cmd/gateway -lint-provider $(CURDIR)

build:
	for arch in amd64 arm64; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/provider-google-linux-$$arch . || exit 1; \
	done

# A local image for the host's architecture, tagged :dev.
image: build
	docker build --build-arg TARGETARCH=$$(go env GOARCH) -t provider-google:dev .

# Start the image and check it answers as the gateway expects.
smoke: image
	./scripts/smoke.sh provider-google:dev
