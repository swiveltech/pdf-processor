package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/swiveltech/pdf-processor/processor"
)

const (
	testPDFPath = "test/pdfs/sample1.pdf"
	testBucket  = "test-bucket"
)

type testCase struct {
	name      string
	pageLimit string
	pdfPath   string
	wantErr   bool
}

func setupTest(t *testing.T, pdfPath string) (string, events.S3Event, func()) {
	t.Helper()
	
	// Convert to absolute path
	absPath, err := filepath.Abs(pdfPath)
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	// Check if file exists
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		t.Fatalf("test file does not exist: %v", err)
	}

	// Create a debug directory and copy the PDF there
	debugDir := "debug-images"
	if err := os.MkdirAll(debugDir, 0755); err != nil {
		t.Fatalf("failed to create debug directory: %v", err)
	}
	
	debugPDF := filepath.Join(debugDir, "input.pdf")
	input, err := os.ReadFile(absPath)
	if err == nil {
		if err := os.WriteFile(debugPDF, input, 0644); err != nil {
			t.Fatalf("failed to write debug PDF: %v", err)
		}
	}

	// Set environment variable for test mode
	os.Setenv("TEST_PDF_PATH", absPath)

	// Create a dummy S3 event
	s3Event := events.S3Event{
		Records: []events.S3EventRecord{
			{
				S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: testBucket},
					Object: events.S3Object{Key: filepath.Base(absPath)},
				},
			},
		},
	}

	cleanup := func() {
		os.Unsetenv("TEST_PDF_PATH")
		os.Unsetenv("PDF_PAGE_LIMIT")
		os.RemoveAll(debugDir)
	}

	return absPath, s3Event, cleanup
}

func TestPDFProcessing(t *testing.T) {
	tests := []testCase{
		{
			name:      "Default page limit (1 page)",
			pageLimit: "",
			pdfPath:   testPDFPath,
		},
		{
			name:      "Custom page limit (2 pages)",
			pageLimit: "2",
			pdfPath:   testPDFPath,
		},
		{
			name:      "Invalid page limit (should default to 1)",
			pageLimit: "invalid",
			pdfPath:   testPDFPath,
		},
		{
			name:      "Non-existent PDF",
			pdfPath:   "non-existent.pdf",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup test environment
			_, s3Event, cleanup := setupTest(t, tt.pdfPath)
			defer cleanup()

			// Set page limit if specified
			if tt.pageLimit != "" {
				os.Setenv("PDF_PAGE_LIMIT", tt.pageLimit)
			}

			// Process the PDF
			response, err := processor.HandleRequest(context.Background(), s3Event)
			
			// Check error cases
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Validate response
			if response.StatusCode != 200 {
				t.Errorf("got status code %d, want 200", response.StatusCode)
			}

			if response.Body == "" {
				t.Error("got empty response body, want non-empty")
			}

			// Parse and validate response body
			var responseBody processor.ResponseBody
			if err := json.Unmarshal([]byte(response.Body), &responseBody); err != nil {
				t.Fatalf("failed to parse response body: %v", err)
			}

			if len(responseBody.Barcodes) == 0 {
				t.Log("warning: no barcodes found in test PDF")
			}
		})
	}
}