package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/your-org/pdf-processor/processor"
)

func TestLocalPDFProcessing(t *testing.T) {
	// Set test PDF path
	testPDFPath := filepath.Join("test", "pdfs", "sample2.pdf")

	// Convert to absolute path
	absPath, err := filepath.Abs(testPDFPath)
	if err != nil {
		t.Fatalf("Error getting absolute path: %v", err)
	}

	// Check if file exists
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		t.Fatalf("Test PDF file not found at %s", absPath)
	}

	// Set environment variable for test mode
	os.Setenv("TEST_PDF_PATH", absPath)
	defer os.Unsetenv("TEST_PDF_PATH")

	// Create a dummy S3 event
	s3Event := events.S3Event{
		Records: []events.S3EventRecord{
			{
				S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: "test-bucket"},
					Object: events.S3Object{Key: filepath.Base(absPath)},
				},
			},
		},
	}

	// Process the PDF
	response, err := processor.HandleRequest(context.Background(), s3Event)
	if err != nil {
		t.Fatalf("Error processing PDF: %v", err)
	}

	if response.StatusCode != 200 {
		t.Errorf("Expected status code 200, got %d", response.StatusCode)
	}

	if response.Body == "" {
		t.Error("Expected non-empty response body")
	}
}