package api

import (
	"fmt"
	"time"

	"github.com/bhav-07/pathpilot/internal/config"
	"github.com/bhav-07/pathpilot/internal/docker"
	"github.com/bhav-07/pathpilot/pkg/container"
	"github.com/docker/docker/client"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/monitor"
	"go.uber.org/zap"
)

func SetupPathPilotAPI(dockerClient *client.Client, lggr *zap.SugaredLogger, cfg *config.Config) *fiber.App {
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

		exists, err := container.CheckDockerImageExists(dockerClient, imageWithTag)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "Error checking image")
		}

		if !exists {
			if err := container.PullDockerImage(dockerClient, imageWithTag); err != nil {
				return fiber.NewError(fiber.StatusInternalServerError, "Error pulling image")
			}
		}

		containerName, redirectURL, err := container.RunDockerContainer(dockerClient, imageWithTag, lggr, cfg)
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
		info, exists := docker.GetContainerInfo(name)
		if !exists {
			return fiber.NewError(fiber.StatusNotFound, "Container not found")
		}
		return c.JSON(info)
	})

	return app
}
