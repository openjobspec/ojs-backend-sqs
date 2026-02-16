variable "aws_region" {
  description = "AWS region"
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "Environment name (e.g., dev, staging, prod)"
  type        = string
  default     = "dev"
}

variable "queue_prefix" {
  description = "Prefix for SQS queue names"
  type        = string
  default     = "ojs"
}

variable "dynamodb_table_name" {
  description = "DynamoDB table name for job state"
  type        = string
  default     = "ojs-jobs"
}

variable "use_fifo" {
  description = "Whether to create FIFO queues"
  type        = bool
  default     = false
}

variable "default_queues" {
  description = "Default OJS queues to create"
  type        = list(string)
  default     = ["default"]
}
