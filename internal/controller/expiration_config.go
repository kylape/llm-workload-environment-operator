package controller

import (
	"fmt"
	"os"
	"time"
)

const (
	DefaultMinimumEnvironmentDuration = 15 * time.Minute
	DefaultMaximumEnvironmentDuration = 72 * time.Hour
	ExpirationReviewInterval          = time.Minute
)

// ExpirationConfig bounds the lifetime users can request for an environment.
type ExpirationConfig struct {
	Minimum time.Duration
	Maximum time.Duration
}

func ExpirationConfigFromEnv(getenv func(string) string) (ExpirationConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	config := ExpirationConfig{
		Minimum: DefaultMinimumEnvironmentDuration,
		Maximum: DefaultMaximumEnvironmentDuration,
	}
	if value := getenv("LWE_MIN_DURATION"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return ExpirationConfig{}, fmt.Errorf("LWE_MIN_DURATION must be a positive Go duration, for example 15m: %q", value)
		}
		config.Minimum = duration
	}
	if value := getenv("LWE_MAX_DURATION"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return ExpirationConfig{}, fmt.Errorf("LWE_MAX_DURATION must be a positive Go duration, for example 72h: %q", value)
		}
		config.Maximum = duration
	}
	if config.Minimum > config.Maximum {
		return ExpirationConfig{}, fmt.Errorf("LWE_MIN_DURATION (%s) cannot exceed LWE_MAX_DURATION (%s)", config.Minimum, config.Maximum)
	}
	return config, nil
}

func (c ExpirationConfig) durationFor(requested string) (time.Duration, error) {
	duration := c.Minimum
	if requested != "" {
		parsed, err := time.ParseDuration(requested)
		if err != nil {
			return 0, fmt.Errorf("duration %q is not a valid Go duration: %w", requested, err)
		}
		duration = parsed
	}
	if duration < c.Minimum || duration > c.Maximum {
		return 0, fmt.Errorf("duration %q is outside the allowed range %s to %s", requested, c.Minimum, c.Maximum)
	}
	return duration, nil
}
