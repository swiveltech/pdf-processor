package main

import (
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/swiveltech/pdf-processor/processor"
)

func main() {
	lambda.Start(processor.HandleRequest)
}