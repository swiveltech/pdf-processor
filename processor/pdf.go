package processor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sort"
	"time"

	
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/oned"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

type Response struct {
	StatusCode int    `json:"statusCode"`
	Body       string `json:"body"`
}

type ResponseBody struct {
	Bucket   string   `json:"bucket"`
	Key      string   `json:"key"`
	Barcodes []string `json:"barcodes"`
}

type BarcodeData struct {
	S3Key        string   `json:"s3_key"`
	BarcodeArray []string `json:"barcode_array"`
}

func getFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func getS3Client() (*s3.Client, error) {
	if os.Getenv("TEST_PDF_PATH") != "" {
		return nil, nil
	}
	
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRetryMaxAttempts(3),
		config.WithRetryMode(aws.RetryModeStandard),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %v", err)
	}
	return s3.NewFromConfig(cfg), nil
}

func preprocessImage(img image.Image) image.Image {
	// Convert to grayscale and apply contrast enhancement
	bounds := img.Bounds()
	gray := image.NewGray(bounds)

	// First pass: calculate min and max values
	var min, max uint8 = 255, 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			// Convert to grayscale using luminance formula
			grayVal := uint8((0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)))
			if grayVal < min {
				min = grayVal
			}
			if grayVal > max {
				max = grayVal
			}
			gray.Set(x, y, color.Gray{Y: grayVal})
		}
	}

	log.Printf("Image preprocessing - Min value: %d, Max value: %d, Contrast: %d", min, max, max-min)

	// Always apply contrast enhancement for small images that might contain barcodes
	if bounds.Dx() < 100 || bounds.Dy() < 100 {
		log.Printf("Small image detected (%dx%d), applying aggressive contrast enhancement", bounds.Dx(), bounds.Dy())
		min = 0 // Force full range contrast
		max = 255
		log.Printf("Forcing full contrast range: min=%d, max=%d", min, max)
	} else if max-min < 30 {
		log.Printf("Not enough contrast, using original grayscale")
		return gray
	}

	// Second pass: apply contrast stretching
	enhanced := image.NewGray(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			original := gray.GrayAt(x, y).Y
			// Apply contrast stretching
			normalized := uint8((float64(original-min) / float64(max-min)) * 255)
			// Apply thresholding for better barcode detection
			if normalized > 128 {
				normalized = 255
			} else {
				normalized = 0
			}
			enhanced.Set(x, y, color.Gray{Y: normalized})
		}
	}

	return enhanced
}

func extractBarcodeFromImage(img image.Image) (string, error) {
	// Log image dimensions for debugging
	bounds := img.Bounds()
	log.Printf("Processing image with dimensions: %dx%d", bounds.Dx(), bounds.Dy())

	// Preprocess image
	processedImg := preprocessImage(img)

	// Create binary bitmap
	bmp, err := gozxing.NewBinaryBitmapFromImage(processedImg)
	if err != nil {
		return "", fmt.Errorf("error creating binary bitmap: %v", err)
	}

	// Create hints map
	hints := map[gozxing.DecodeHintType]interface{}{
		gozxing.DecodeHintType_TRY_HARDER: true,
		gozxing.DecodeHintType_PURE_BARCODE: true,
	}

	// Try different barcode formats
	readers := []struct {
		name   string
		reader gozxing.Reader
	}{
		{"UPC/EAN", oned.NewMultiFormatUPCEANReader(hints)},
		{"Code128", oned.NewCode128Reader()},
		{"Code39", oned.NewCode39Reader()},
		{"Code93", oned.NewCode93Reader()},
		{"ITF", oned.NewITFReader()},
		{"CodaBar", oned.NewCodaBarReader()},
	}

	var lastErr error
	for _, r := range readers {
		// Try normal orientation
		result, err := r.reader.Decode(bmp, hints)
		if err == nil {
			format := result.GetBarcodeFormat().String()
			log.Printf("Found %s barcode using %s reader: %s", format, r.name, result.GetText())
			return result.GetText(), nil
		}
		lastErr = err
		log.Printf("Attempt with %s reader failed: %v", r.name, err)
	}

	return "", fmt.Errorf("no barcode found with any reader, last error: %v", lastErr)
}

func getWebhookURL() string {
	url := os.Getenv("WEBHOOK_URL")
	if url == "" {
		log.Printf("Warning: WEBHOOK_URL not set")
	}
	return url
}

func getWebhookToken() (string, error) {
	token := os.Getenv("WEBHOOK_TOKEN")
	if token == "" {
		return "", fmt.Errorf("WEBHOOK_TOKEN environment variable not set")
	}
	return token, nil
}

func makeWebhookRequest(method, url string, payload io.Reader) (*http.Response, error) {
	// Create custom client with TLS skip verification if needed
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: os.Getenv("SKIP_TLS_VERIFY") == "true",
			},
		},
		// Handle redirects properly
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	// Create request
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	// Set headers
	token, err := getWebhookToken()
	if err != nil {
		return nil, fmt.Errorf("webhook token error: %v", err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	// Debug logging if enabled
	if os.Getenv("DEBUG") == "true" {
		log.Printf("Making request to: %s\n", url)
		log.Printf("Headers: %v\n", req.Header)
	}

	// Make the request
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error making request: %v\nTry setting SKIP_TLS_VERIFY=true if having TLS issues", err)
	}

	return res, nil
}

func callRubyEndpoint(data BarcodeData) error {
	url := getWebhookURL()
	if url == "" {
		return fmt.Errorf("WEBHOOK_URL environment variable not set")
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("error marshaling JSON: %v", err)
	}

	res, err := makeWebhookRequest("POST", url, bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("error making webhook request: %v", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("error reading response body: %v", err)
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("webhook returned non-200 status: %d, body: %s", res.StatusCode, string(body))
	}

	log.Printf("Successfully sent barcode data to webhook: %v", data.BarcodeArray)
	return nil
}

func HandleRequest(ctx context.Context, s3Event events.S3Event) (Response, error) {
	// Create debug directory if in test mode
	if os.Getenv("TEST_DEBUG") == "true" {
		os.MkdirAll("debug-images", 0755)
	}
	// Get page limit from environment variable, default to processing first page only if not set
	pageLimit := 1 // Default to scanning only first page
	if limitStr := os.Getenv("PDF_PAGE_LIMIT"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err == nil && limit > 0 {
			pageLimit = limit
		}
	}

	var pdfBytes []byte
	var err error
	var bucket, key string

	if testPath := os.Getenv("TEST_PDF_PATH"); testPath != "" {
		// Local testing mode - read file directly
		pdfBytes, err = os.ReadFile(testPath)
		if err != nil {
			return Response{StatusCode: 500, Body: "Error reading test PDF"}, err
		}
		key = testPath
		bucket = "test-bucket"
	} else {
		// Get the S3 bucket and key
		if len(s3Event.Records) == 0 {
			return Response{StatusCode: 400, Body: "No S3 event records"}, fmt.Errorf("no S3 event records")
		}
		
		record := s3Event.Records[0]
		bucket = record.S3.Bucket.Name
		key = record.S3.Object.Key

		// Validate bucket and key
		if bucket == "" || key == "" {
			return Response{StatusCode: 400, Body: "Invalid S3 event: missing bucket or key"}, 
                   fmt.Errorf("invalid S3 event: bucket=%q, key=%q", bucket, key)
		}

		// Log the attempt
		log.Printf("Attempting to get object from S3 - Bucket: %s, Key: %s", bucket, key)

		// Initialize S3 client
		s3Client, err := getS3Client()
		if err != nil {
			return Response{StatusCode: 500, Body: fmt.Sprintf("Failed to initialize S3 client: %v", err)}, err
		}

		// Get the PDF directly from S3
		input := &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}
		
		result, err := s3Client.GetObject(ctx, input)
		if err != nil {
			// Log detailed error for debugging
			log.Printf("S3 GetObject error - Bucket: %s, Key: %s, Error: %v", bucket, key, err)
			return Response{
				StatusCode: 500,
				Body: fmt.Sprintf("Failed to get object from S3 (bucket: %s, key: %s): %v", 
                       bucket, key, err),
			}, err
		}
		defer result.Body.Close()

		// Create a buffer with reasonable size
		buf := bytes.NewBuffer(make([]byte, 0, 1024*1024)) // 1MB initial capacity
		
		// Copy with timeout
		done := make(chan error, 1)
		go func() {
			_, err := io.Copy(buf, result.Body)
			done <- err
		}()

		select {
		case <-time.After(30 * time.Second):
			return Response{StatusCode: 500, Body: "Timeout reading PDF from S3"}, fmt.Errorf("timeout reading PDF")
		case err := <-done:
			if err != nil {
				return Response{StatusCode: 500, Body: "Error reading PDF from S3"}, err
			}
		}
		
		pdfBytes = buf.Bytes()
		
		// Validate PDF size
		if len(pdfBytes) == 0 {
			return Response{StatusCode: 400, Body: "Empty PDF file from S3"}, 
                   fmt.Errorf("empty PDF file from S3: bucket=%s, key=%s", bucket, key)
		}
	}

	log.Printf("Read PDF file: %s (size: %d bytes)", key, len(pdfBytes))
	
	// Validate PDF contents
	if len(pdfBytes) == 0 {
		return Response{StatusCode: 400, Body: "Empty PDF file"}, fmt.Errorf("empty PDF file")
	}
	
	// Check if it's a valid PDF (starts with %PDF)
	if len(pdfBytes) < 4 || string(pdfBytes[0:4]) != "%PDF" {
		return Response{StatusCode: 400, Body: "Invalid PDF format"}, fmt.Errorf("invalid PDF format")
	}

	// Create a temporary directory for extracted images
	tmpDir, err := os.MkdirTemp("", "pdf-images-*")
	if err != nil {
		return Response{StatusCode: 500, Body: "Error creating temp directory"}, err
	}
	defer os.RemoveAll(tmpDir)

	// Write PDF to temporary file
	tmpPDF := filepath.Join(tmpDir, "input.pdf")
	if err := os.WriteFile(tmpPDF, pdfBytes, 0644); err != nil {
		return Response{StatusCode: 500, Body: "Error writing temporary PDF"}, err
	}

	// Get page limit from environment variable
	pageRange := os.Getenv("PDF_PAGE_LIMIT")
	if pageRange == "" {
		pageRange = "1" // Default to 1 page
	}

	// Configure PDF processing
	config := model.NewDefaultConfiguration()
	// Set validation mode to relaxed
	config.ValidationMode = model.ValidationRelaxed

	// Create a directory for processed pages
	tmpPagesDir := filepath.Join(tmpDir, "pages")
	if err := os.MkdirAll(tmpPagesDir, 0755); err != nil {
		return Response{StatusCode: 500, Body: "Error creating pages directory"}, err
	}

	// Convert page limit to integer for splitting
	pageLimit, err = strconv.Atoi(pageRange)
	if err != nil {
		log.Printf("Invalid page limit %s, defaulting to 1", pageRange)
		pageLimit = 1
	}

	// Extract images from the PDF
	log.Printf("Extracting images from PDF %s to %s", tmpPDF, tmpDir)
	if err := api.ExtractImagesFile(tmpPDF, tmpDir, nil, config); err != nil {
		log.Printf("Error extracting images from PDF: %v", err)
		return Response{StatusCode: 500, Body: "Error extracting images from PDF"}, err
	}

	// Save extracted images to debug directory if in test mode
	if os.Getenv("TEST_DEBUG") == "true" {
		debugDir := "/tmp/pdf-debug"
		files, err := os.ReadDir(tmpDir)
		if err == nil {
			for _, file := range files {
				if !file.IsDir() && filepath.Ext(file.Name()) != ".pdf" {
					src := filepath.Join(tmpDir, file.Name())
					dst := filepath.Join(debugDir, file.Name())
					input, err := os.ReadFile(src)
					if err == nil {
						os.WriteFile(dst, input, 0644)
					}
				}
			}
		}
	}
	
	// List extracted files
	extractedFiles, err := os.ReadDir(tmpDir)
	if err != nil {
		log.Printf("Error reading temp dir: %v", err)
	} else {
		log.Printf("Extracted files in %s:", tmpDir)
		for _, f := range extractedFiles {
			log.Printf("  - %s (size: %d bytes)", f.Name(), getFileSize(filepath.Join(tmpDir, f.Name())))
		}
	}

	// Read extracted images from temp directory and only process up to pageLimit
	files, err := os.ReadDir(tmpDir)
	if err != nil {
		return Response{StatusCode: 500, Body: "Error reading extracted images"}, err
	}

	// Get all image files from pages within the limit
	sortedFiles := make([]string, 0)
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) != ".pdf" {
			// Check if image is from a page within our limit
			parts := strings.Split(file.Name(), "_")
			if len(parts) >= 2 {
				pageNum, err := strconv.Atoi(parts[1])
				if err == nil && pageNum <= pageLimit {
					sortedFiles = append(sortedFiles, file.Name())
				}
			}
		}
	}
	// Sort files for consistent processing order
	sort.Strings(sortedFiles)
	// Process all images from the selected pages
	processFiles := sortedFiles

	// Process each selected image file and collect barcodes
	var foundBarcodes []string
	for i, fileName := range processFiles {
		imgPath := filepath.Join(tmpDir, fileName)
		imgFile, err := os.Open(imgPath)
		if err != nil {
			log.Printf("Error opening image %s: %v", fileName, err)
			continue
		}

		img, _, err := image.Decode(imgFile)
		imgFile.Close()
		if err != nil {
			log.Printf("Error decoding image %s: %v", fileName, err)
			continue
		}

		log.Printf("Processing image %d: %s (dimensions: %dx%d)", i+1, fileName, img.Bounds().Dx(), img.Bounds().Dy())
		// Try to detect barcode
		barcode, err := extractBarcodeFromImage(img)
		if err != nil {
			log.Printf("Failed to extract barcode from image %s: %v", fileName, err)
			// Don't continue, try next image
		} else if barcode != "" {
			log.Printf("Found barcode in image %s: %s", fileName, barcode)
			foundBarcodes = append(foundBarcodes, barcode)
			data := BarcodeData{
				S3Key:        key,
				BarcodeArray: []string{barcode},
			}
			if err := callRubyEndpoint(data); err != nil {
				log.Printf("Error sending barcode data to API: %v", err)
			}
		}
	}

	// Process all found barcodes
	if len(foundBarcodes) > 0 {
		// Send all found barcodes in a single webhook call
		data := BarcodeData{
			S3Key:        key,
			BarcodeArray: foundBarcodes,
		}
		if err := callRubyEndpoint(data); err != nil {
			log.Printf("Error sending barcode data to API: %v", err)
		}

		// Return success response with found barcodes
		jsonBody, _ := json.Marshal(ResponseBody{
			Bucket:   bucket,
			Key:      key,
			Barcodes: foundBarcodes,
		})
		return Response{
			StatusCode: 200,
			Body:       string(jsonBody),
		}, nil
	}

	// Call webhook with empty barcode array if no barcodes found
	data := BarcodeData{
		S3Key:        key,
		BarcodeArray: []string{},
	}
	if err := callRubyEndpoint(data); err != nil {
		log.Printf("Error sending empty barcode data to API: %v", err)
	}

	// Return success response with empty barcode array
	jsonBody, _ := json.Marshal(ResponseBody{
		Bucket:   bucket,
		Key:      key,
		Barcodes: []string{},
	})
	return Response{
		StatusCode: 200,
		Body:       string(jsonBody),
	}, nil
}