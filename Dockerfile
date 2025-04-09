# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o main .

# Final stage
FROM alpine:latest

WORKDIR /app

# Install required packages and tools
RUN apk add --no-cache \
    bash \
    curl \
    jq \
    && curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
    && chmod +x kubectl \
    && mv kubectl /usr/local/bin/ \
    && curl -LO https://mirror.openshift.com/pub/openshift-v4/clients/oc/latest/linux/oc.tar.gz \
    && tar xzf oc.tar.gz \
    && rm oc.tar.gz \
    && mv oc /usr/local/bin/

# Copy the binary from builder
COPY --from=builder /app/main .

# Create directory for scripts
RUN mkdir -p /scripts

# Set environment variables with defaults
ENV SCRIPTS_PATH=/scripts/scripts.json
ENV POD_LABEL_SELECTOR=app=query-server
ENV NAMESPACE=default

# Expose port
EXPOSE 8080

# Run the application
CMD ["./main"]
