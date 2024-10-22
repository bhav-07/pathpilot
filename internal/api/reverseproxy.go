package api

import (
	"fmt"
	"strings"

	"github.com/bhav-07/pathpilot/internal/config"
	"github.com/bhav-07/pathpilot/internal/docker"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/proxy"
	"go.uber.org/zap"
)

func SetupReverseProxy(lggr *zap.SugaredLogger, cfg *config.Config) *fiber.App {
	app := fiber.New()

	app.Use(func(c *fiber.Ctx) error {
		hostname := c.Hostname()
		subdomain := strings.Split(hostname, ".")[0]

		info, exists := docker.GetContainerInfo(subdomain)
		if !exists {
			lggr.Warnw("Container not found", "subdomain", subdomain)
			return fiber.NewError(fiber.StatusNotFound, "Container not found")
		}

		proxyURL := fmt.Sprintf("http://%s:%s", info.IPAddress, info.DefaultPort)
		lggr.Infow("Forwarding request",
			"from", fmt.Sprintf(hostname),
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
