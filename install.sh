#!/usr/bin/env bash

set -e

echo "Installing sqd-go..."

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "ERROR: 'go' could not be found."
    echo "sqd-go requires Go to be installed both for the CLI to run and"
    echo "for generating/running the indexer code."
    echo ""
    echo "Please install Go (https://go.dev/doc/install) and ensure it's in your PATH."
    exit 1
fi

GO_VERSION=$(go version | awk '{print $3}')
echo "Found $GO_VERSION"

# We use go install to download and build the binary
echo "Running: GOPROXY=direct go install github.com/franz101/sqd-go@latest"
if ! GOPROXY=direct go install github.com/franz101/sqd-go@latest; then
    echo "Fallback: cloning repository to build from source..."
    TMP_DIR=$(mktemp -d)
    git clone https://github.com/franz101/sqd-go.git "$TMP_DIR"
    cd "$TMP_DIR"
    go install .
    cd - > /dev/null
    rm -rf "$TMP_DIR"
fi

# Determine GOPATH/bin
GOPATH=$(go env GOPATH)
GOBIN=$(go env GOBIN)
if [ -z "$GOBIN" ]; then
    GOBIN="$GOPATH/bin"
fi

echo ""
echo "Installation complete!"
echo "The 'sqd-go' binary has been installed to: $GOBIN"

# Check if GOBIN is in PATH
if [[ ":$PATH:" != *":$GOBIN:"* ]]; then
    echo ""
    echo "WARNING: $GOBIN is not in your PATH."
    echo "You may need to add the following line to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    echo ""
    echo "  export PATH=\$PATH:$GOBIN"
    echo ""
fi

echo "To verify installation, open a new terminal or update your PATH, then run:"
echo "  sqd-go help"
