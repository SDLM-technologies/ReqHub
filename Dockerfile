# Stage 1: Build the Go app
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY main.go .
# Compile with cgo disabled, targeting linux/amd64 (or arm64 depending on your TrueNAS)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o reqhub main.go

# Stage 2: Ultra-lightweight final image
FROM alpine:latest

# Install root certificates for HTTPS requests to iTunes API
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Create folder for persistent data
RUN mkdir -p /app/data

# Copy the binary and the HTML file
COPY --from=builder /app/reqhub .
COPY index.html .

EXPOSE 8080

CMD ["./reqhub"]
