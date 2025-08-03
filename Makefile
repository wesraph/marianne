.PHONY: build static clean

# Default build
build:
	go build -o marianne .

# Static build
static:
	CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o marianne .

# Clean build artifacts
clean:
	rm -f marianne