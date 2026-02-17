package server

import (
	"os"
	"strconv"
	"time"
)

// Config holds server configuration from environment variables.
type Config struct {
	Port            string
	GRPCPort        string
	AWSRegion       string
	AWSEndpointURL  string // For LocalStack
	DynamoDBTable   string
	SQSQueuePrefix  string
	UseFIFO         bool
	APIKey          string

	// HTTP server timeouts
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// Shutdown drain timeout
	ShutdownTimeout time.Duration
}

// LoadConfig reads configuration from environment variables with defaults.
func LoadConfig() Config {
	return Config{
		Port:           getEnv("OJS_PORT", "8080"),
		GRPCPort:       getEnv("OJS_GRPC_PORT", "9090"),
		AWSRegion:      getEnv("AWS_REGION", "us-east-1"),
		AWSEndpointURL: getEnv("AWS_ENDPOINT_URL", ""), // Empty = real AWS
		DynamoDBTable:  getEnv("DYNAMODB_TABLE", "ojs-jobs"),
		SQSQueuePrefix: getEnv("SQS_QUEUE_PREFIX", "ojs"),
		UseFIFO:        getEnvBool("SQS_USE_FIFO", false),
		APIKey:         getEnv("OJS_API_KEY", ""),

		ReadTimeout:  getDurationEnv("OJS_READ_TIMEOUT", 30*time.Second),
		WriteTimeout: getDurationEnv("OJS_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:  getDurationEnv("OJS_IDLE_TIMEOUT", 120*time.Second),

		ShutdownTimeout: getDurationEnv("OJS_SHUTDOWN_TIMEOUT", 30*time.Second),
	}
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return defaultVal
}

func getDurationEnv(key string, defaultVal time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return defaultVal
}
