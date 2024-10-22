package container

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/bhav-07/pathpilot/internal/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

func CheckDockerImageExists(cli *client.Client, imageName string) (bool, error) {
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

func PullDockerImage(cli *client.Client, imageName string) error {
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

func RunDockerContainer(cli *client.Client, imageName string, lggr *zap.SugaredLogger, cfg *config.Config) (string, string, error) {
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
