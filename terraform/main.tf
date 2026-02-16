terraform {
  required_version = ">= 1.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

# DynamoDB table for job state (single-table design)
resource "aws_dynamodb_table" "ojs_jobs" {
  name         = var.dynamodb_table_name
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "PK"
  range_key    = "SK"

  attribute {
    name = "PK"
    type = "S"
  }

  attribute {
    name = "SK"
    type = "S"
  }

  attribute {
    name = "GSI1PK"
    type = "S"
  }

  attribute {
    name = "GSI1SK"
    type = "S"
  }

  attribute {
    name = "GSI2PK"
    type = "S"
  }

  attribute {
    name = "GSI2SK"
    type = "S"
  }

  attribute {
    name = "GSI3PK"
    type = "S"
  }

  attribute {
    name = "GSI3SK"
    type = "N"
  }

  # GSI1: Query jobs by queue + state
  global_secondary_index {
    name            = "GSI1"
    hash_key        = "GSI1PK"
    range_key       = "GSI1SK"
    projection_type = "ALL"
  }

  # GSI2: Query jobs by state
  global_secondary_index {
    name            = "GSI2"
    hash_key        = "GSI2PK"
    range_key       = "GSI2SK"
    projection_type = "ALL"
  }

  # GSI3: Query due scheduled/retry jobs by due timestamp
  global_secondary_index {
    name            = "GSI3"
    hash_key        = "GSI3PK"
    range_key       = "GSI3SK"
    projection_type = "ALL"
  }

  # TTL for automatic cleanup of completed/expired items
  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  tags = {
    Environment = var.environment
    Service     = "ojs-backend-sqs"
  }
}

# SQS queues for each default queue
resource "aws_sqs_queue" "ojs_dlq" {
  for_each = toset(var.default_queues)

  name = var.use_fifo ? "${var.queue_prefix}-${each.value}-dlq.fifo" : "${var.queue_prefix}-${each.value}-dlq"

  message_retention_seconds = 1209600 # 14 days

  fifo_queue                  = var.use_fifo
  content_based_deduplication = var.use_fifo

  tags = {
    Environment = var.environment
    Service     = "ojs-backend-sqs"
    OJSQueue    = each.value
    Type        = "dlq"
  }
}

resource "aws_sqs_queue" "ojs_queue" {
  for_each = toset(var.default_queues)

  name = var.use_fifo ? "${var.queue_prefix}-${each.value}.fifo" : "${var.queue_prefix}-${each.value}"

  visibility_timeout_seconds  = 30
  message_retention_seconds   = 1209600 # 14 days
  receive_wait_time_seconds   = 20      # Long polling

  fifo_queue                  = var.use_fifo
  content_based_deduplication = var.use_fifo

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.ojs_dlq[each.value].arn
    maxReceiveCount     = 3
  })

  tags = {
    Environment = var.environment
    Service     = "ojs-backend-sqs"
    OJSQueue    = each.value
    Type        = "main"
  }
}

output "dynamodb_table_name" {
  value = aws_dynamodb_table.ojs_jobs.name
}

output "dynamodb_table_arn" {
  value = aws_dynamodb_table.ojs_jobs.arn
}

output "sqs_queue_urls" {
  value = { for k, v in aws_sqs_queue.ojs_queue : k => v.url }
}

output "sqs_dlq_urls" {
  value = { for k, v in aws_sqs_queue.ojs_dlq : k => v.url }
}
