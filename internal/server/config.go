package server

import (
	commonconfig "github.com/openjobspec/ojs-go-backend-common/config"
)

// Config holds server configuration from environment variables.
type Config struct {
	commonconfig.BaseConfig
	AWSRegion          string
	AWSEndpointURL     string
	LocalStackEndpoint string
	DynamoDBTable      string
	SQSQueuePrefix     string
	UseFIFO            bool
}

// LoadConfig reads configuration from environment variables with defaults.
func LoadConfig() Config {
	return Config{
		BaseConfig:         commonconfig.LoadBaseConfig(),
		AWSRegion:          commonconfig.GetEnv("AWS_REGION", "us-east-1"),
		AWSEndpointURL:     commonconfig.GetEnv("AWS_ENDPOINT_URL", ""),
		LocalStackEndpoint: commonconfig.GetEnv("OJS_LOCALSTACK_ENDPOINT", ""),
		DynamoDBTable:      commonconfig.GetEnv("DYNAMODB_TABLE", "ojs-jobs"),
		SQSQueuePrefix:     commonconfig.GetEnv("SQS_QUEUE_PREFIX", "ojs"),
		UseFIFO:            commonconfig.GetEnvBool("SQS_USE_FIFO", false),
	}
}
