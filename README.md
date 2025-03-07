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

### Using Helm Chart

The script executor can be installed on Kubernetes using our Helm chart:

#### Add the Helm Repository

```bash
helm repo add k8s-script-executor https://decloudz.github.io/k8s-script-executor
helm repo update
```

#### Install the Chart

```bash
helm install script-executor k8s-script-executor/k8s-script-executor \
  --namespace your-namespace \
  --create-namespace \
  --set env[0].name=NAMESPACE \
  --set env[0].value=your-namespace \
  --set env[1].name=POD_LABEL_SELECTOR \
  --set env[1].value="app=your-app"
```

#### Configuration Values

The following table lists the configurable parameters for the chart:

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Image repository | `ghcr.io/alvdevcl/k8s-script-executor` |
| `image.tag` | Image tag | `latest` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `service.type` | Kubernetes service type | `ClusterIP` |
| `service.port` | Service port | `80` |
| `env` | Environment variables | `[]` |
| `resources` | CPU/Memory resource requests/limits | `{}` |
| `nodeSelector` | Node labels for pod assignment | `{}` |
| `tolerations` | Tolerations for pod assignment | `[]` |
| `affinity` | Node/Pod affinities | `{}` |
| `config.scripts` | List of predefined scripts | See values.yaml |

#### Using a Custom Values File

Create a `values.yaml` file with your custom configuration:

```yaml
image:
  tag: "1.0.0"

env:
  - name: NAMESPACE
    value: "production"
  - name: POD_LABEL_SELECTOR
    value: "app=database"

config:
  scripts:
    - name: "check-disk-space"
      command: "df -h"
    - name: "check-memory"
      command: "free -m"
```

Then install the chart with:

```bash
helm install script-executor k8s-script-executor/k8s-script-executor \
  -f values.yaml \
  --namespace your-namespace \
  --create-namespace
```

#### Upgrading

To upgrade your deployment:

```bash
helm upgrade script-executor k8s-script-executor/k8s-script-executor \
  --namespace your-namespace \
  -f values.yaml
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