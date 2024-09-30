# PathPilot

PathPilot is a dynamic reverse proxy and container management system built with Go. It allows users to easily deploy and access Docker containers through a simple API, automatically managing the routing and container lifecycle.

## Features

- Dynamic reverse proxy for Docker containers
- RESTful API for container management
- Automatic Docker image pulling and container creation
- Configurable logging system
- Graceful shutdown handling
- Health check endpoint
- Basic metrics endpoint
- Environment-based configuration

## Prerequisites

- Go 1.16 or later
- Docker

## Installation

1. Clone the repository:

   ```
   git clone https://github.com/yourusername/pathpilot.git
   cd pathpilot
   ```

2. Install dependencies:
   ```
   go mod tidy
   ```

## Configuration

PathPilot uses environment variables for configuration. Here are the available options with their default values:

- `LOG_DIR`: Directory for log files (default: "logs")
- `LOG_LEVEL`: Logging level (default: "INFO")
- `API_PORT`: Port for the PathPilot API (default: "8080")
- `PROXY_PORT`: Port for the reverse proxy (default: "8000")
- `MAX_LOG_FILE_SIZE`: Maximum size of the log file in bytes before it's deleted (default: 10MB)

## Usage

1. Start the PathPilot application:

   ```
   go run main.go
   ```

2. The application will start two servers:
   - PathPilot API on `http://localhost:8080`
   - Reverse Proxy on `http://localhost:8000`

### API Endpoints

- `POST /containers`: Create a new container

  ```json
  {
    "image": "nginx",
    "tag": "latest"
  }
  ```

- `GET /container/:name`: Get information about a specific container

- `GET /health`: Health check endpoint

- `GET /metrics`: Basic metrics endpoint

### Accessing Containers

After creating a container, you can access it through the reverse proxy using the following URL format:

```
http://{container_id}.localhost:8000
```

Replace `{container_id}` with the ID returned by the create container API.

## Logging

Logs are written to a file in the specified `LOG_DIR`. The log file is named `pathpilot.log`. When the log file exceeds the specified `MAX_LOG_FILE_SIZE`, it is deleted and a new file is created.

## Development

To contribute to PathPilot:

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/AmazingFeature`)
3. Commit your changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- [Fiber](https://github.com/gofiber/fiber) - The web framework used
- [Docker SDK for Go](https://github.com/docker/docker-ce/tree/master/components/engine/client) - Used for Docker interactions
- [Zap](https://github.com/uber-go/zap) - Logging library
