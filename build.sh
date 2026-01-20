#!/bin/bash

# Build the Go binary
go mod download
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o authentik-bot .

if [ $? -eq 0 ]; then
    echo "✓ Build successful: authentik-bot"
else
    echo "✗ Build failed"
    exit 1
fi
