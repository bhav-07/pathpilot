package docker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/bhav-07/pathpilot/internal/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

type ContainerInfo struct {
	Name        string
	IPAddress   string
	DefaultPort string
}

var (
	containerMap = make(map[string]ContainerInfo)
	mapMutex     sync.RWMutex
)

func InitializeContainerMap(cli *client.Client, lggr *zap.SugaredLogger, cfg *config.Config) error {
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
			"proxyAddress", fmt.Sprintf("%s.localhost:%s", name, cfg.ProxyPort),
			"containerAddress", fmt.Sprintf("http://%s:%s", ipAddress, defaultPort))
	}

	return nil
}

func ListenDockerEvents(cli *client.Client, lggr *zap.SugaredLogger, cfg *config.Config) {
	messages, errors := cli.Events(context.Background(), events.ListOptions{})

	for {
		select {
		case err := <-errors:
			log.Printf("Error in getting events: %v", err)
			return
		case msg := <-messages:
			ProcessEvent(cli, msg, lggr, cfg)
		}
	}
}

func ProcessEvent(cli *client.Client, event events.Message, lggr *zap.SugaredLogger, cfg *config.Config) {
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
			containerName, cfg.ProxyPort, ipAddress, defaultPort)

		fmt.Printf("Event: %s, ID: %s, Action: %s\n", event.Status, event.ID, event.Action)
		lggr.Infow("Container started",
			"name", containerName,
			"proxyAddress", fmt.Sprintf("%s.localhost:%s", containerName, cfg.ProxyPort),
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

func GetContainerInfo(name string) (ContainerInfo, bool) {
	mapMutex.RLock()
	defer mapMutex.RUnlock()
	info, exists := containerMap[name]
	return info, exists
}

// func addContainerToMap(ctx context.Context, cli *client.Client, containerID string) error {
// 	containerInfo, err := cli.ContainerInspect(ctx, containerID)
// 	if err != nil {
// 		return fmt.Errorf("error inspecting container: %w", err)
// 	}

// 	name := strings.TrimPrefix(containerInfo.Name, "/")
// 	ipAddress := containerInfo.NetworkSettings.IPAddress
// 	defaultPort := getDefaultPort(containerInfo)

// 	containerMap.Store(name, ContainerInfo{
// 		Name:        name,
// 		IPAddress:   ipAddress,
// 		DefaultPort: defaultPort,
// 	})

// 	zap.L().Info("Container added to map",
// 		zap.String("name", name),
// 		zap.String("ipAddress", ipAddress),
// 		zap.String("defaultPort", defaultPort))

// 	return nil
// }

// func getDefaultPort(containerInfo types.ContainerJSON) string {
// 	for port := range containerInfo.Config.ExposedPorts {
// 		return strings.Split(string(port), "/")[0]
// 	}
// 	return "80" // Default to 80 if no port is exposed
// }

// // GetContainerInfo retrieves container information from the map
// func GetContainerInfo(name string) (ContainerInfo, bool) {
// 	info, exists := containerMap.Load(name)
// 	if !exists {
// 		return ContainerInfo{}, false
// 	}
// 	return info.(ContainerInfo), true
// }
