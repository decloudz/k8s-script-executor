# Kubernetes Script Executor

A lightweight HTTP server that executes predefined scripts inside Kubernetes pods. This tool provides a simple API to run maintenance and utility scripts in your Kubernetes cluster without direct cluster access.

## Features

- Execute predefined scripts inside Kubernetes pods
- Support for both Kubernetes and OpenShift clusters
- Configurable pod selection via label selectors
- Environment variable configuration
- RESTful API interface
- Docker container support

## Installation

### Using Docker

```bash
docker pull ghcr.io/alvdevcl/k8s-script-executor:latest
```

### Building from Source

```bash
git clone https://github.com/alvdevcl/k8s-script-executor.git
cd k8s-script-executor
go build -o k8s-script-executor
```

## Configuration

The application can be configured using environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `SCRIPTS_PATH` | Path to the scripts JSON file | `/scripts/scripts.json` |
| `POD_LABEL_SELECTOR` | Label selector for target pods | `app=query-server` |
| `NAMESPACE` | Kubernetes namespace | `default` |

### Scripts Configuration

Create a `scripts.json` file with your predefined scripts:

```json
[
  {
    "name": "check-logs",
    "command": "tail -n 100 /var/log/application.log"
  },
  {
    "name": "health-check",
    "command": "curl -f http://localhost:8080/health"
  }
]
```

## Usage

### Running the Container

```bash
docker run -d \
  -p 8080:8080 \
  -v /path/to/scripts.json:/scripts/scripts.json \
  -e NAMESPACE=my-namespace \
  -e POD_LABEL_SELECTOR="app=my-app" \
  ghcr.io/alvdevcl/k8s-script-executor:latest
```

### API Endpoints

#### List Available Scripts

```bash
curl http://localhost:8080/scripts
```

Response:
```json
[
  {
    "name": "check-logs",
    "command": "tail -n 100 /var/log/application.log"
  }
]
```

#### Execute a Script

```bash
curl -X POST http://localhost:8080/execute \
  -H "Content-Type: application/json" \
  -d '{"script_name": "check-logs"}'
```

Response:
```json
{
  "script_name": "check-logs",
  "output": "log output here..."
}
```

## Development

### Prerequisites

- Go 1.21 or later
- Docker
- kubectl
- oc (OpenShift CLI)

### Building

```bash
go build -o k8s-script-executor
```

### Running Tests

```bash
go test ./...
```

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Security

- The application requires proper Kubernetes RBAC configuration
- Ensure the service account has minimal required permissions
- Use network policies to restrict access to the API
- Consider using HTTPS for API communication

## Support

For support, please open an issue in the GitHub repository. 