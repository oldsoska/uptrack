// Package config reads settings from environment variables (12-factor style).
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port                string
	DatabaseURL         string
	AdminToken          string
	BlockPrivateTargets bool
	MaxConcurrentChecks int
	ResultRetention     time.Duration
	TelegramBotToken    string
	TelegramChatID      string
	WebhookURL          string
}

func Load() (Config, error) {
	c := Config{
		Port:             getenv("PORT", "8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		AdminToken:       os.Getenv("ADMIN_TOKEN"),
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:   os.Getenv("TELEGRAM_CHAT_ID"),
		WebhookURL:       os.Getenv("WEBHOOK_URL"),
	}
	var errs []error
	var err error
	if c.BlockPrivateTargets, err = strconv.ParseBool(getenv("BLOCK_PRIVATE_TARGETS", "true")); err != nil {
		errs = append(errs, fmt.Errorf("BLOCK_PRIVATE_TARGETS: %w", err))
	}
	if c.MaxConcurrentChecks, err = strconv.Atoi(getenv("MAX_CONCURRENT_CHECKS", "20")); err != nil || c.MaxConcurrentChecks < 1 {
		errs = append(errs, errors.New("MAX_CONCURRENT_CHECKS must be a positive integer"))
	}
	if c.ResultRetention, err = time.ParseDuration(getenv("RESULT_RETENTION", "720h")); err != nil || c.ResultRetention < time.Hour {
		errs = append(errs, errors.New("RESULT_RETENTION must be a duration of at least 1h, e.g. 720h"))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	// The token guards the whole admin API; a short one could be brute-forced
	if len(c.AdminToken) < 32 {
		errs = append(errs, errors.New("ADMIN_TOKEN must be at least 32 characters"))
	}
	if (c.TelegramBotToken == "") != (c.TelegramChatID == "") {
		errs = append(errs, errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set together"))
	}
	return c, errors.Join(errs...)
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
