.PHONY: build static clean

# Default build with size optimizations
build:
	go build -trimpath -ldflags="-s -w" -o marianne .

# Static build with size optimizations
static:
	CGO_ENABLED=0 GOOS=linux go build -trimpath -a -ldflags '-extldflags "-static" -s -w' -o marianne .

# Clean build artifacts
clean:
	rm -f marianne