# Use a minimal Go image
FROM golang:1.19 as builder

# Set working directory
WORKDIR /app

# Copy go mod and download dependencies
COPY go.mod go.sum ./
RUN go mod tidy

# Copy source files
COPY . .

# Build the application
RUN go build -o server

# Create a minimal final image
FROM alpine:latest

# Install required utilities
RUN apk add --no-cache bash kubectl jq curl

# Set working directory
WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/server /app/

# Expose port 8080
EXPOSE 8080

# Run the API server
CMD ["/app/server"]
