package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/monitor"
	"github.com/gofiber/fiber/v2/middleware/proxy"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Config struct {
	LogDir         string
	LogLevel       string
	APIPort        string
	ProxyPort      string
	MaxLogFileSize int64
}

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
	cfg          Config
)

func init() {
	cfg = loadConfig()
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

func loadConfig() Config {
	return Config{
		LogDir:         getEnv("LOG_DIR", "logs"),
		LogLevel:       getEnv("LOG_LEVEL", "INFO"),
		APIPort:        getEnv("API_PORT", "8080"),
		ProxyPort:      getEnv("PROXY_PORT", "8000"),
		MaxLogFileSize: getEnvAsInt64("MAX_LOG_FILE_SIZE", 10*1024*1024),
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvAsInt64(key string, fallback int64) int64 {
	strValue := getEnv(key, "")
	if value, err := strconv.ParseInt(strValue, 10, 64); err == nil {
		return value
	}
	return fallback
}

func setupLogFile() (*os.File, error) {
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	logPath := filepath.Join(cfg.LogDir, "pathpilot.log")

	if info, err := os.Stat(logPath); err == nil {
		if info.Size() > cfg.MaxLogFileSize {
			if err := os.Remove(logPath); err != nil {
				return nil, fmt.Errorf("failed to delete old log file: %w", err)
			}
		}
	}
	return os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

func getLogLevel() zapcore.Level {
	switch strings.ToUpper(cfg.LogLevel) {
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

	if err := initializeContainerMap(dockerClient); err != nil {
		logger.Errorf("Error initializing container map: %v", err)
	}

	go listenDockerEvents(dockerClient)

	pathpilotAPI := setupPathPilotAPI(dockerClient)
	reverseProxy := setupReverseProxy()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		logger.Info("Gracefully shutting down...")
		_ = pathpilotAPI.Shutdown()
		_ = reverseProxy.Shutdown()
	}()

	go func() {
		logger.Infof("PathPilot API is running on PORT %s", cfg.APIPort)
		if err := pathpilotAPI.Listen(":" + cfg.APIPort); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("Failed to start PathPilot API: %v", err)
		}
	}()

	logger.Infof("PathPilot is running on PORT %s", cfg.ProxyPort)
	if err := reverseProxy.Listen(":" + cfg.ProxyPort); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("Failed to start reverse proxy: %v", err)
	}
}

func setupPathPilotAPI(dockerClient *client.Client) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			logger.Errorw("Request error",
				"path", c.Path(),
				"error", err,
				"status", code)
			return c.Status(code).JSON(fiber.Map{
				"error": err.Error(),
			})
		},
	})

	app.Use(func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		logger.Infow("Request processed",
			"method", c.Method(),
			"path", c.Path(),
			"status", c.Response().StatusCode(),
			"duration", time.Since(start))
		return err
	})

	app.Get("/metrics", monitor.New())

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.SendString("OK")
	})

	app.Post("/containers", func(c *fiber.Ctx) error {
		var body struct {
			Image string `json:"image"`
			Tag   string `json:"tag"`
		}

		if err := c.BodyParser(&body); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "Invalid request body")
		}
		if body.Image == "" {
			return fiber.NewError(fiber.StatusBadRequest, "Image is required")
		}
		if body.Tag == "" {
			body.Tag = "latest"
		}

		imageWithTag := fmt.Sprintf("%s:%s", body.Image, body.Tag)

		exists, err := checkDockerImageExists(dockerClient, imageWithTag)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "Error checking image")
		}

		if !exists {
			if err := pullDockerImage(dockerClient, imageWithTag); err != nil {
				return fiber.NewError(fiber.StatusInternalServerError, "Error pulling image")
			}
		}

		resp, err := runDockerContainer(dockerClient, imageWithTag)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "Error running container")
		}

		logger.Infow("Container created",
			"image", imageWithTag,
			"containerID", resp.ID)

		return c.JSON(fiber.Map{
			"status":    "success",
			"container": fmt.Sprintf("%s.localhost:%s", resp.ID, cfg.ProxyPort),
		})
	})

	app.Get("/container/:name", func(c *fiber.Ctx) error {
		name := c.Params("name")
		info, exists := getContainerInfo(name)
		if !exists {
			return fiber.NewError(fiber.StatusNotFound, "Container not found")
		}
		return c.JSON(info)
	})

	return app
}

func setupReverseProxy() *fiber.App {
	app := fiber.New()

	app.Use(func(c *fiber.Ctx) error {
		hostname := c.Hostname()
		subdomain := strings.Split(hostname, ".")[0]

		info, exists := getContainerInfo(subdomain)
		if !exists {
			logger.Warnw("Container not found", "subdomain", subdomain)
			return fiber.NewError(fiber.StatusNotFound, "Container not found")
		}

		proxyURL := fmt.Sprintf("http://%s:%s", info.IPAddress, info.DefaultPort)
		logger.Infow("Forwarding request",
			"from", fmt.Sprintf("%s:%s", hostname, cfg.ProxyPort),
			"to", proxyURL)

		if err := proxy.Do(c, proxyURL); err != nil {
			logger.Errorw("Proxy request failed",
				"error", err,
				"from", fmt.Sprintf("%s:%s", hostname, cfg.ProxyPort),
				"to", proxyURL)
			return err
		}

		return nil
	})

	return app
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

	decoder := json.NewDecoder(reader)
	var message map[string]interface{}

	for {
		if err := decoder.Decode(&message); err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		if status, ok := message["status"].(string); ok {
			fmt.Println(status)
		}
	}

	return nil
}

func runDockerContainer(cli *client.Client, imageName string) (container.CreateResponse, error) {
	containerConfig := &container.Config{
		Image: imageName,
		Tty:   false,
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
