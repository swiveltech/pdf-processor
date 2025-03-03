package main

import (
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/your-org/pdf-processor/processor"
)

func main() {
	lambda.Start(processor.HandleRequest)
}