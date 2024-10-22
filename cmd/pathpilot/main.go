package main

import (
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/bhav-07/pathpilot/internal/api"
	"github.com/bhav-07/pathpilot/internal/config"
	"github.com/bhav-07/pathpilot/internal/docker"
	"github.com/bhav-07/pathpilot/internal/logger"
	"github.com/docker/docker/client"
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

	if err := docker.InitializeContainerMap(dockerClient, lggr, cfg); err != nil {
		lggr.Errorf("Error initializing container map: %v", err)
	}

	go docker.ListenDockerEvents(dockerClient, lggr, cfg)

	pathpilotAPI := api.SetupPathPilotAPI(dockerClient, lggr, cfg)
	reverseProxy := api.SetupReverseProxy(lggr, cfg)

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
