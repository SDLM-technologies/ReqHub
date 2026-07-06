# Stage 1: Build the Go app
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY main.go .
# Compilation avec cgo désactivé, ciblant linux/amd64 (ou arm64 selon votre TrueNAS)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o reqhub main.go

# Stage 2: Image finale ultra-légère
FROM alpine:latest

# Installation des certificats racine pour les requêtes HTTPS vers iTunes API
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Création du dossier pour les données persistantes
RUN mkdir -p /app/data

# Copie du binaire et du fichier HTML
COPY --from=builder /app/reqhub .
COPY index.html .

EXPOSE 8080

CMD ["./reqhub"]
