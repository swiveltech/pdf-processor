package processor

import (
	"context"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

func TestPreprocessImage(t *testing.T) {
	tests := []struct {
		name     string
		imgSize  image.Rectangle
		contrast bool
	}{
		{
			name:     "Small image with aggressive contrast",
			imgSize:  image.Rect(0, 0, 50, 50),
			contrast: true,
		},
		{
			name:     "Normal image with sufficient contrast",
			imgSize:  image.Rect(0, 0, 200, 200),
			contrast: true,
		},
		{
			name:     "Normal image with low contrast",
			imgSize:  image.Rect(0, 0, 200, 200),
			contrast: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test image
			img := image.NewRGBA(tt.imgSize)
			if tt.contrast {
				// Add high contrast pattern
				for y := 0; y < tt.imgSize.Max.Y; y++ {
					for x := 0; x < tt.imgSize.Max.X; x++ {
						if (x+y)%2 == 0 {
							img.Set(x, y, color.White)
						} else {
							img.Set(x, y, color.Black)
						}
					}
				}
			} else {
				// Add low contrast pattern
				gray := color.Gray{Y: 128}
				for y := 0; y < tt.imgSize.Max.Y; y++ {
					for x := 0; x < tt.imgSize.Max.X; x++ {
						img.Set(x, y, gray)
					}
				}
			}

			// Process image
			processed := preprocessImage(img)
			
			// Verify the processed image is not nil
			if processed == nil {
				t.Error("preprocessImage returned nil")
			}
			
			// Verify dimensions are preserved
			if processed.Bounds() != tt.imgSize {
				t.Errorf("processed image size %v, want %v", processed.Bounds(), tt.imgSize)
			}
		})
	}
}

func TestExtractBarcodeFromImage(t *testing.T) {
	// Skip if no test images available
	testDir := "../test/pdfs"
	if _, err := os.Stat(testDir); os.IsNotExist(err) {
		t.Skip("test directory not found")
	}

	tests := []struct {
		name     string
		imgPath  string
		wantCode string
		wantErr  bool
	}{
		{
			name:     "Valid barcode image",
			imgPath:  filepath.Join(testDir, "sample1.pdf"),
			wantCode: "*29581200051216188000014453", // Update this with your expected barcode
			wantErr:  false,
		},
		{
			name:    "Invalid image",
			imgPath: "nonexistent.pdf",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Skip if test file doesn't exist (except for error test cases)
			if _, err := os.Stat(tt.imgPath); os.IsNotExist(err) && !tt.wantErr {
				t.Skipf("test file %s not found", tt.imgPath)
			}

			// Create test image for valid cases
			var img image.Image
			var err error
			if !tt.wantErr {
				// Here you would load your test image
				// For now, we'll create a simple test image
				img = image.NewRGBA(image.Rect(0, 0, 100, 100))
			}

			code, err := extractBarcodeFromImage(img)
			
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}
			
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if code != tt.wantCode {
				t.Errorf("got barcode %q, want %q", code, tt.wantCode)
			}
		})
	}
}

func TestWebhookIntegration(t *testing.T) {
	tests := []struct {
		name       string
		setupEnv   func()
		cleanupEnv func()
		data       BarcodeData
		wantErr    bool
	}{
		{
			name: "Missing webhook URL",
			setupEnv: func() {
				os.Unsetenv("WEBHOOK_URL")
				os.Setenv("WEBHOOK_TOKEN", "test-token")
			},
			cleanupEnv: func() {
				os.Unsetenv("WEBHOOK_TOKEN")
			},
			data: BarcodeData{
				S3Key:        "test.pdf",
				BarcodeArray: []string{"test-barcode"},
			},
			wantErr: true,
		},
		{
			name: "Missing webhook token",
			setupEnv: func() {
				os.Setenv("WEBHOOK_URL", "https://localhost:3000")
				os.Unsetenv("WEBHOOK_TOKEN")
			},
			cleanupEnv: func() {
				os.Unsetenv("WEBHOOK_URL")
			},
			data: BarcodeData{
				S3Key:        "test.pdf",
				BarcodeArray: []string{"test-barcode"},
			},
			wantErr: true,
		},
		{
			name: "Valid configuration",
			setupEnv: func() {
				os.Setenv("WEBHOOK_URL", "https://localhost:3000")
				os.Setenv("WEBHOOK_TOKEN", "test-token")
			},
			cleanupEnv: func() {
				os.Unsetenv("WEBHOOK_URL")
				os.Unsetenv("WEBHOOK_TOKEN")
			},
			data: BarcodeData{
				S3Key:        "test.pdf",
				BarcodeArray: []string{"test-barcode"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup test environment
			if tt.setupEnv != nil {
				tt.setupEnv()
			}
			// Ensure cleanup
			if tt.cleanupEnv != nil {
				defer tt.cleanupEnv()
			}

			err := callWebhook(tt.data)
			
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}
			
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestGetS3Client(t *testing.T) {
	tests := []struct {
		name       string
		setupEnv   func()
		cleanupEnv func()
		wantNil    bool
		wantErr    bool
	}{
		{
			name: "Test mode",
			setupEnv: func() {
				os.Setenv("TEST_PDF_PATH", "test.pdf")
			},
			cleanupEnv: func() {
				os.Unsetenv("TEST_PDF_PATH")
			},
			wantNil: true,
			wantErr: false,
		},
		{
			name:    "Production mode",
			wantNil: false,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupEnv != nil {
				tt.setupEnv()
			}
			if tt.cleanupEnv != nil {
				defer tt.cleanupEnv()
			}

			client, err := getS3Client()
			
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}
			
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantNil && client != nil {
				t.Error("expected nil client but got non-nil")
			}
			if !tt.wantNil && client == nil {
				t.Error("expected non-nil client but got nil")
			}
		})
	}
}