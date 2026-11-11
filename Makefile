.PHONY: build build-linux css test dev postgres clean
build:
	mkdir -p bin
	go build -trimpath -o bin/srk ./cmd/srk
build-linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/srk-linux-amd64 ./cmd/srk
css:
	npm ci --ignore-scripts --cache .local/npm-cache
	npm run build:css
test:
	go test -race ./...
	go vet ./...
postgres:
	sh scripts/dev-postgres.sh
dev: build postgres
	sh scripts/dev-services.sh
clean:
	rm -rf bin

.PHONY: rescue-iso
rescue-iso:
	sh scripts/build-rescue.sh

.PHONY: altlinux
altlinux:
	sh scripts/build-altlinux.sh

.PHONY: linux-packages
linux-packages:
	sh scripts/build-linux-packages.sh
