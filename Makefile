GIT_HEAD = $(shell git rev-parse HEAD | head -c8)

build:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/omni_linux_amd64 -v wings.go
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/omni_linux_arm64 -v wings.go

debug:
	go build -o omni -ldflags="-X github.com/pterodactyl/wings/system.Version=$(GIT_HEAD)"
	sudo ./omni --debug --ignore-certificate-errors --config config.yml --pprof --pprof-block-rate 1

# Runs a remotely debuggable Omni session allowing an IDE to connect and target
# different breakpoints.
rmdebug:
	go build -o omni -gcflags "all=-N -l" -ldflags="-X github.com/pterodactyl/wings/system.Version=$(GIT_HEAD)" -race
	sudo dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./omni -- --debug --ignore-certificate-errors --config config.yml

cross-build: clean build compress

clean:
	rm -rf build/omni_*

.PHONY: all build compress clean
