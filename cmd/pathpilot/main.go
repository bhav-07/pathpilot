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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bhav-07/pathpilot/internal/config"
	"github.com/bhav-07/pathpilot/internal/logger"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/monitor"
	"github.com/gofiber/fiber/v2/middleware/proxy"
	"go.uber.org/zap"
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
	lggr         *zap.SugaredLogger
	// logFile      *os.File
	cfg config.Config
)

func main() {
	cfg, _ := config.Load()
	lggr := logger.New(cfg)
	defer lggr.Sync()
	// defer logFile.Close()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		lggr.Fatalf("Failed to create Docker client: %v", err)
	}

	if err := initializeContainerMap(dockerClient); err != nil {
		lggr.Errorf("Error initializing container map: %v", err)
	}

	go listenDockerEvents(dockerClient)

	pathpilotAPI := setupPathPilotAPI(dockerClient)
	reverseProxy := setupReverseProxy()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		lggr.Info("Gracefully shutting down...")
		_ = pathpilotAPI.Shutdown()
		_ = reverseProxy.Shutdown()
	}()

	go func() {
		lggr.Infof("PathPilot API is running on PORT %s", cfg.APIPort)
		if err := pathpilotAPI.Listen(":" + cfg.APIPort); err != nil && err != http.ErrServerClosed {
			lggr.Fatalf("Failed to start PathPilot API: %v", err)
		}
	}()

	lggr.Infof("PathPilot is running on PORT %s", cfg.ProxyPort)
	if err := reverseProxy.Listen(":" + cfg.ProxyPort); err != nil && err != http.ErrServerClosed {
		lggr.Fatalf("Failed to start reverse proxy: %v", err)
	}
}

func setupPathPilotAPI(dockerClient *client.Client) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			lggr.Errorw("Request error",
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
		lggr.Infow("Request processed",
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

		containerName, redirectURL, err := runDockerContainer(dockerClient, imageWithTag)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "Error running container")
		}
		lggr.Infow("Container created",
			"image", imageWithTag,
			"containerName", containerName, "redirestUrl", redirectURL)

		return c.JSON(fiber.Map{
			"status":        "success",
			"containerName": containerName,
			"redirectURL":   redirectURL,
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
			lggr.Warnw("Container not found", "subdomain", subdomain)
			return fiber.NewError(fiber.StatusNotFound, "Container not found")
		}

		proxyURL := fmt.Sprintf("http://%s:%s", info.IPAddress, info.DefaultPort)
		lggr.Infow("Forwarding request",
			"from", fmt.Sprintf("%s:%s", hostname, cfg.ProxyPort),
			"to", proxyURL)

		if err := proxy.Do(c, proxyURL); err != nil {
			lggr.Errorw("Proxy request failed",
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
			lggr.Errorw("Error inspecting container",
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

		lggr.Infow("Initialized container",
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

func runDockerContainer(cli *client.Client, imageName string) (string, string, error) {
	containerConfig := &container.Config{
		Image: imageName,
		Tty:   false,
	}

	hostConfig := &container.HostConfig{
		AutoRemove: true,
	}

	resp, err := cli.ContainerCreate(context.Background(), containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return "", "", err
	}

	if err := cli.ContainerStart(context.Background(), resp.ID, container.StartOptions{}); err != nil {
		return "", "", err
	}

	containerInfo, err := cli.ContainerInspect(context.Background(), resp.ID)
	if err != nil {
		return "", "", err
	}

	containerName := strings.TrimPrefix(containerInfo.Name, "/")

	redirectURL := fmt.Sprintf("%s.localhost:%s", containerName, cfg.ProxyPort)

	lggr.Infow("Container started",
		"id", resp.ID,
		"name", containerName,
		"redirectURL", redirectURL)

	return containerName, redirectURL, nil
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
		lggr.Infow("Container started",
			"name", containerName,
			"proxyAddress", fmt.Sprintf("%s.localhost:%s", containerName, PROXY_PORT),
			"containerAddress", fmt.Sprintf("http://%s:%s", ipAddress, defaultPort))
	} else if event.Type == "container" && event.Action == "die" {

		containerName := event.Actor.Attributes["name"]
		mapMutex.Lock()
		delete(containerMap, containerName)
		mapMutex.Unlock()

		lggr.Infow("Container stopped and removed from map",
			"name", containerName)
	}
}

func getContainerInfo(name string) (ContainerInfo, bool) {
	mapMutex.RLock()
	defer mapMutex.RUnlock()
	info, exists := containerMap[name]
	return info, exists
}
