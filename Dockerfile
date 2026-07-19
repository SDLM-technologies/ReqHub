# Stage 1: Build the Go app
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go .
# Compile with cgo disabled, targeting linux/amd64 (or arm64 depending on your TrueNAS)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o reqhub .

# Stage 2: Ultra-lightweight final image
FROM alpine:latest

# Install root certificates, timezone data, ffmpeg, yt-dlp, and rsgain from edge community repo
RUN apk add --no-cache ca-certificates tzdata ffmpeg yt-dlp \
    && apk add --no-cache rsgain --repository=http://dl-cdn.alpinelinux.org/alpine/edge/community

WORKDIR /app

# Create folder for persistent data
RUN mkdir -p /app/data

# Copy the binary and the HTML file
COPY --from=builder /app/reqhub .
COPY index.html .

EXPOSE 8080

CMD ["./reqhub"]
