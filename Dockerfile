# https://medium.com/code-beyond/dockerizing-golang-apps-a-step-by-step-guide-to-reducing-docker-image-size-306898e7359e
# Use the official Golang image to build the application
FROM golang:1.23.6-alpine AS builder

# Set the Current Working Directory inside the container
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies. Dependencies will be cached if the go.mod and go.sum files are not changed
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the container
COPY . .

# Disables C dependencies for portability.
ENV CGO_ENABLED=0  
# y using the build flags -s and -w, we remove the symbol table and DWARF debugging information, which shrinks the size of the binary.
RUN go build -ldflags="-s -w" -o main .

RUN adduser -D scratchuser

# Start a new stage from scratch https://hub.docker.com/_/scratch
FROM scratch

COPY --from=builder /etc/passwd /etc/passwd

USER scratchuser

WORKDIR /app

# Copy the Pre-built binary file from the previous stage
COPY --from=builder /app/main .

# Expose port 8080 to the outside world
EXPOSE 8080

# Command to run the executable
CMD ["./main"]