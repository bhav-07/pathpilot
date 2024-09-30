package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/proxy"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type ContainerInfo struct {
	Name        string
	IPAddress   string
	DefaultPort string
}

const (
	PROXY_PORT     = "8000"
	MaxLogFileSize = 10 * 1024 * 1024
)

var (
	containerMap = make(map[string]ContainerInfo)
	mapMutex     sync.RWMutex
	logger       *zap.SugaredLogger
	logFile      *os.File
)

func init() {
	var err error
	logFile, err = setupLogFile()
	if err != nil {
		panic(fmt.Sprintf("Failed to set up log file: %v", err))
	}

	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(logFile),
		getLogLevel(),
	)

	baseLogger := zap.New(core)
	logger = baseLogger.Sugar()

	logger.Infow("Logger initialized", "logfile", logFile.Name())
}

func setupLogFile() (*os.File, error) {
	logDir := os.Getenv("LOG_DIR")
	if logDir == "" {
		logDir = "logs"
	}

	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	logPath := filepath.Join(logDir, "pathpilot.log")

	// Check if the file exists and its size
	if info, err := os.Stat(logPath); err == nil {
		if info.Size() > MaxLogFileSize {
			// Delete the old log file if it's too big
			if err := os.Remove(logPath); err != nil {
				return nil, fmt.Errorf("failed to delete old log file: %w", err)
			}
		}
	}

	// Open the log file in append mode, create it if it doesn't exist
	return os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

func getLogLevel() zapcore.Level {
	level := os.Getenv("LOG_LEVEL")
	switch strings.ToUpper(level) {
	case "DEBUG":
		return zapcore.DebugLevel
	case "INFO":
		return zapcore.InfoLevel
	case "WARN":
		return zapcore.WarnLevel
	case "ERROR":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

func main() {
	defer logger.Sync()
	defer logFile.Close()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Fatalf("Failed to create Docker client: %v", err)
	}

	go listenDockerEvents(dockerClient)

	pathpilotAPI := fiber.New()
	setupPathPilotAPI(pathpilotAPI, dockerClient)

	reverseProxy := fiber.New()
	setupReverseProxy(reverseProxy)

	go func() {
		logger.Infof("PathPilot API is running on PORT 8080")
		if err := pathpilotAPI.Listen(":8080"); err != nil {
			logger.Fatalf("Failed to start PathPilot API: %v", err)
		}
	}()

	logger.Infof("PathPilot is running on PORT %s", PROXY_PORT)
	if err := reverseProxy.Listen(":" + PROXY_PORT); err != nil {
		logger.Fatalf("Failed to start reverse proxy: %v", err)
	}
}

func setupPathPilotAPI(app *fiber.App, dockerClient *client.Client) {
	app.Post("/containers", func(c *fiber.Ctx) error {
		type RequestBody struct {
			Image string `json:"image"`
			Tag   string `json:"tag"`
		}

		var body RequestBody

		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).SendString("Invalid request body")
		}
		if body.Tag == "" {
			body.Tag = "latest"
		}

		imageWithTag := fmt.Sprintf("%s:%s", body.Image, body.Tag)

		exists, err := checkDockerImageExists(dockerClient, imageWithTag)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("Error checking image")
		}

		if !exists {
			if err := pullDockerImage(dockerClient, imageWithTag); err != nil {
				return c.Status(fiber.StatusInternalServerError).SendString("Error pulling image")
			}
		}

		resp, err := runDockerContainer(dockerClient, imageWithTag)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("Error running container")
		}

		logger.Infow("Container created",
			"image", imageWithTag,
			"containerID", resp.ID)

		return c.JSON(fiber.Map{
			"status":    "success",
			"container": fmt.Sprintf("%s.localhost:%s", resp.ID, PROXY_PORT),
		})
	})

	app.Get("/container/:name", func(c *fiber.Ctx) error {
		name := c.Params("name")
		info, exists := getContainerInfo(name)
		if !exists {
			return c.Status(404).SendString("Container not found")
		}
		return c.JSON(info)
	})
}

func setupReverseProxy(app *fiber.App) {
	app.Use(func(c *fiber.Ctx) error {
		hostname := c.Hostname()
		subdomain := strings.Split(hostname, ".")[0]

		info, exists := getContainerInfo(subdomain)
		if !exists {
			logger.Warnw("Container not found", "subdomain", subdomain)
			return c.Status(404).SendString("Container not found")
		}

		proxyURL := fmt.Sprintf("http://%s:%s", info.IPAddress, info.DefaultPort)
		logger.Infow("Forwarding request",
			"from", fmt.Sprintf("%s:%s", hostname, PROXY_PORT),
			"to", proxyURL)

		if err := proxy.Do(c, proxyURL); err != nil {
			logger.Errorw("Proxy request failed",
				"error", err,
				"from", fmt.Sprintf("%s:%s", hostname, PROXY_PORT),
				"to", proxyURL)
			return err
		}

		return nil
	})
}

func initializeContainerMap(cli *client.Client) error {
	containers, err := cli.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range containers {
		containerInfo, err := cli.ContainerInspect(context.Background(), c.ID)
		if err != nil {
			logger.Errorw("Error inspecting container",
				"containerID", c.ID,
				"error", err)
			continue
		}

		name := strings.TrimPrefix(containerInfo.Name, "/")
		ipAddress := containerInfo.NetworkSettings.IPAddress

		exposedPorts := containerInfo.Config.ExposedPorts
		var defaultPort string

		if len(exposedPorts) > 0 {
			for port := range exposedPorts {
				portParts := strings.Split(string(port), "/")
				if len(portParts) == 2 && portParts[1] == "tcp" {
					defaultPort = portParts[0]
					break
				}
			}
		}

		if defaultPort == "" {
			defaultPort = "80"
		}

		mapMutex.Lock()
		containerMap[name] = ContainerInfo{
			Name:        name,
			IPAddress:   ipAddress,
			DefaultPort: defaultPort,
		}
		mapMutex.Unlock()

		logger.Infow("Initialized container",
			"name", name,
			"proxyAddress", fmt.Sprintf("%s.localhost:%s", name, PROXY_PORT),
			"containerAddress", fmt.Sprintf("http://%s:%s", ipAddress, defaultPort))
	}

	return nil
}

func checkDockerImageExists(cli *client.Client, imageName string) (bool, error) {
	images, err := cli.ImageList(context.Background(), image.ListOptions{})
	if err != nil {
		return false, err
	}

	for _, image := range images {
		for _, tag := range image.RepoTags {
			if tag == imageName || strings.Contains(tag, imageName) {
				return true, nil
			}
		}
	}
	return false, nil
}

func pullDockerImage(cli *client.Client, imageName string) error {
	reader, err := cli.ImagePull(context.Background(), imageName, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()

	// Stream the pull progress
	decoder := json.NewDecoder(reader)
	var message map[string]interface{}

	for {
		if err := decoder.Decode(&message); err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		// Print status updates to the console (can be logged)
		if status, ok := message["status"].(string); ok {
			fmt.Println(status)
		}
	}

	return nil
}

func runDockerContainer(cli *client.Client, imageName string) (container.CreateResponse, error) {
	containerConfig := &container.Config{
		Image: imageName,
		// Tty:   false,
	}

	hostConfig := &container.HostConfig{
		AutoRemove: true,
	}

	// Create the container
	resp, err := cli.ContainerCreate(context.Background(), containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return container.CreateResponse{}, err
	}

	// Start the container
	if err := cli.ContainerStart(context.Background(), resp.ID, container.StartOptions{}); err != nil {
		return container.CreateResponse{}, err
	}

	log.Printf("Container %s started", resp.ID)
	return resp, nil
}

func listenDockerEvents(cli *client.Client) {
	messages, errors := cli.Events(context.Background(), events.ListOptions{})

	for {
		select {
		case err := <-errors:
			log.Printf("Error in getting events: %v", err)
			return
		case msg := <-messages:
			// Process the event message
			processEvent(cli, msg)
		}
	}
}

func processEvent(cli *client.Client, event events.Message) {
	if event.ID == "" {
		return
	}

	if event.Type == "container" && event.Action == "start" {
		containerInfo, err := cli.ContainerInspect(context.Background(), event.ID)
		if err != nil {
			log.Printf("Error inspecting container: %v", err)
			return
		}

		containerName := containerInfo.Name[1:]
		ipAddress := containerInfo.NetworkSettings.IPAddress

		exposedPorts := containerInfo.Config.ExposedPorts
		var defaultPort string

		if len(exposedPorts) > 0 {
			for port := range exposedPorts {
				portParts := strings.Split(string(port), "/")
				if len(portParts) == 2 && portParts[1] == "tcp" {
					defaultPort = portParts[0]
					break
				}
			}
		}

		if defaultPort == "" {
			defaultPort = "80"
		}

		mapMutex.Lock()
		containerMap[containerName] = ContainerInfo{
			Name:        containerName,
			IPAddress:   ipAddress,
			DefaultPort: defaultPort,
		}
		mapMutex.Unlock()

		log.Printf("Registering %s.localhost:%s --> http://%s:%s",
			containerName, PROXY_PORT, ipAddress, defaultPort)

		fmt.Printf("Event: %s, ID: %s, Action: %s\n", event.Status, event.ID, event.Action)
		logger.Infow("Container started",
			"name", containerName,
			"proxyAddress", fmt.Sprintf("%s.localhost:%s", containerName, PROXY_PORT),
			"containerAddress", fmt.Sprintf("http://%s:%s", ipAddress, defaultPort))
	} else if event.Type == "container" && event.Action == "die" {

		containerName := event.Actor.Attributes["name"]
		mapMutex.Lock()
		delete(containerMap, containerName)
		mapMutex.Unlock()

		logger.Infow("Container stopped and removed from map",
			"name", containerName)
	}
}

func getContainerInfo(name string) (ContainerInfo, bool) {
	mapMutex.RLock()
	defer mapMutex.RUnlock()
	info, exists := containerMap[name]
	return info, exists
}
