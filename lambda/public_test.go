package lambda_test

import (
	"log/slog"
	"net/http"

	ojslambda "github.com/openjobspec/ojs-backend-sqs/lambda"
)

var (
	_ http.Handler     = (*ojslambda.Adapter)(nil)
	_ ojslambda.Option = ojslambda.WithLogger(slog.Default())
)
