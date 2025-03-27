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
	BarcodeScanLogID int      `json:"barcode_scan_log_id"`
	BarcodeArray     []string `json:"barcode_array,omitempty"`
	ErrorInfo        string   `json:"error_info,omitempty"`
	Status           string   `json:"status,omitempty"`
}

type SQSMessageBody struct {
	S3Key            string `json:"s3_key"`
	FileName         string `json:"file_name"`
	BucketName       string `json:"bucket_name"`
	Region           string `json:"region"`
	BarcodeScanLogID int    `json:"barcode_scan_log_id"`
}

func getFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func getS3Client(region string) (*s3.Client, error) {
	if os.Getenv("TEST_PDF_PATH") != "" {
		return nil, nil
	}
	
	if region == "" {
		region = "us-east-1"  // Fallback default region
	}
	
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRetryMaxAttempts(3),
		config.WithRetryMode(aws.RetryModeStandard),
		config.WithRegion(region),
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

	log.Printf("[ID:%d] Image preprocessing - Min value: %d, Max value: %d, Contrast: %d", 0, min, max, max-min)

	// Always apply contrast enhancement for small images that might contain barcodes
	if bounds.Dx() < 100 || bounds.Dy() < 100 {
		log.Printf("[ID:%d] Small image detected (%dx%d), applying aggressive contrast enhancement", 0, bounds.Dx(), bounds.Dy())
		min = 0 // Force full range contrast
		max = 255
		log.Printf("[ID:%d] Forcing full contrast range: min=%d, max=%d", 0, min, max)
	} else if max-min < 30 {
		log.Printf("[ID:%d] Not enough contrast, using original grayscale", 0)
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
	log.Printf("[ID:%d] Processing image with dimensions: %dx%d", 0, bounds.Dx(), bounds.Dy())

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
			log.Printf("[ID:%d] Found %s barcode using %s reader: %s", 0, format, r.name, result.GetText())
			return result.GetText(), nil
		}
		lastErr = err
		log.Printf("[ID:%d] Attempt with %s reader failed: %v", 0, r.name, err)
	}

	return "", fmt.Errorf("no barcode found with any reader, last error: %v", lastErr)
}

func getWebhookURL() string {
	url := os.Getenv("WEBHOOK_URL")
	if url == "" {
		log.Printf("[ID:%d] Warning: WEBHOOK_URL not set", 0)
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
		log.Printf("[ID:%d] Making request to: %s\n", 0, url)
		log.Printf("[ID:%d] Headers: %v\n", 0, req.Header)
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

	log.Printf("[ID:%d] Successfully sent barcode data to webhook: %v", data.BarcodeScanLogID, data.BarcodeArray)
	return nil
}

func HandleRequest(ctx context.Context, sqsEvent events.SQSEvent) (Response, error) {
	// Validate SQS event
	if len(sqsEvent.Records) == 0 {
		return Response{StatusCode: 400, Body: "No SQS event records"}, fmt.Errorf("no SQS event records")
	}

	// Process first record (we handle one message at a time)
	record := sqsEvent.Records[0]
    
	// Parse message body
	var messageBody SQSMessageBody
	if err := json.Unmarshal([]byte(record.Body), &messageBody); err != nil {
		return Response{StatusCode: 400, Body: "Invalid message format"}, fmt.Errorf("failed to parse message: %v", err)
	}

	startTime := time.Now()
	log.Printf("[ID:%d] Started processing PDF file: %s", messageBody.BarcodeScanLogID, messageBody.FileName)

	// Validate required fields
	if messageBody.S3Key == "" || messageBody.BucketName == "" {
		log.Printf("[ID:%d] Missing required fields: s3_key=%q, bucket_name=%q", 
			messageBody.BarcodeScanLogID, messageBody.S3Key, messageBody.BucketName)
		return Response{StatusCode: 400, Body: "Missing s3_key or bucket_name in message"}, 
			fmt.Errorf("missing required fields")
	}

	var pdfBytes []byte
	var err error
	key := messageBody.S3Key
	bucket := messageBody.BucketName

	if testPath := os.Getenv("TEST_PDF_PATH"); testPath != "" {
		// Local testing mode - read file directly
		pdfBytes, err = os.ReadFile(testPath)
		if err != nil {
			log.Printf("[ID:%d] Error reading test PDF: %v", messageBody.BarcodeScanLogID, err)
			return Response{StatusCode: 500, Body: "Error reading test PDF"}, err
		}
		key = testPath
		bucket = "test-bucket"
	} else {
		// Initialize S3 client with region from SQS message
		s3Client, err := getS3Client(messageBody.Region)
		if err != nil {
			log.Printf("[ID:%d] Failed to initialize S3 client: %v", messageBody.BarcodeScanLogID, err)
			return Response{StatusCode: 500, Body: fmt.Sprintf("Failed to initialize S3 client: %v", err)}, err
		}

		log.Printf("[ID:%d] Downloading PDF from S3: bucket=%s, key=%s", messageBody.BarcodeScanLogID, bucket, key)
		
		// Get the PDF directly from S3
		input := &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}
		
		result, err := s3Client.GetObject(ctx, input)
		if err != nil {
			log.Printf("[ID:%d] Failed to download PDF: %v", messageBody.BarcodeScanLogID, err)
			return Response{StatusCode: 500, Body: "Failed to get object from S3"}, err
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
			log.Printf("[ID:%d] Timeout downloading PDF", messageBody.BarcodeScanLogID)
			return Response{StatusCode: 500, Body: "Timeout reading PDF from S3"}, fmt.Errorf("timeout reading PDF")
		case err := <-done:
			if err != nil {
				log.Printf("[ID:%d] Error downloading PDF: %v", messageBody.BarcodeScanLogID, err)
				return Response{StatusCode: 500, Body: "Error reading PDF from S3"}, err
			}
		}
		
		pdfBytes = buf.Bytes()
		log.Printf("[ID:%d] Successfully downloaded PDF (size: %d bytes)", messageBody.BarcodeScanLogID, len(pdfBytes))
	}
	
	// Create a temporary directory for extracted images
	tmpDir, err := os.MkdirTemp("", "pdf-images-*")
	if err != nil {
		log.Printf("[ID:%d] Error creating temp directory: %v", messageBody.BarcodeScanLogID, err)
		return Response{StatusCode: 500, Body: "Error creating temp directory"}, err
	}
	defer os.RemoveAll(tmpDir)

	// Write PDF to temporary file
	tmpPDF := filepath.Join(tmpDir, "input.pdf")
	if err := os.WriteFile(tmpPDF, pdfBytes, 0644); err != nil {
		log.Printf("[ID:%d] Error writing temporary PDF: %v", messageBody.BarcodeScanLogID, err)
		return Response{StatusCode: 500, Body: "Error writing temporary PDF"}, err
	}

	// Send processing status callback
	processingData := BarcodeData{
		BarcodeScanLogID: messageBody.BarcodeScanLogID,
		Status:           "processing",
	}
	if err := callRubyEndpoint(processingData); err != nil {
		log.Printf("[ID:%d] Error sending processing status: %v", messageBody.BarcodeScanLogID, err)
		// Continue processing even if callback fails
	} else {
		log.Printf("[ID:%d] Successfully sent processing status", messageBody.BarcodeScanLogID)
	}

	// Configure PDF processing
	config := model.NewDefaultConfiguration()
	config.ValidationMode = model.ValidationRelaxed

	// Extract images from the PDF
	log.Printf("[ID:%d] Extracting images from PDF", messageBody.BarcodeScanLogID)
	if err := api.ExtractImagesFile(tmpPDF, tmpDir, nil, config); err != nil {
		log.Printf("[ID:%d] Error extracting images: %v", messageBody.BarcodeScanLogID, err)
		return Response{StatusCode: 500, Body: "Error extracting images from PDF"}, err
	}

	// Process extracted images
	files, err := os.ReadDir(tmpDir)
	if err != nil {
		log.Printf("[ID:%d] Error reading extracted images: %v", messageBody.BarcodeScanLogID, err)
		return Response{StatusCode: 500, Body: "Error reading extracted images"}, err
	}

	// Get all image files from pages within the limit
	var processFiles []string
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) != ".pdf" {
			processFiles = append(processFiles, file.Name())
		}
	}
	sort.Strings(processFiles)

	// Process each image file and collect barcodes
	var foundBarcodes []string
	log.Printf("[ID:%d] Processing %d extracted images", messageBody.BarcodeScanLogID, len(processFiles))

	for _, fileName := range processFiles {
		imgPath := filepath.Join(tmpDir, fileName)
		imgFile, err := os.Open(imgPath)
		if err != nil {
			log.Printf("[ID:%d] Error opening image %s: %v", messageBody.BarcodeScanLogID, fileName, err)
			continue
		}

		img, _, err := image.Decode(imgFile)
		imgFile.Close()
		if err != nil {
			log.Printf("[ID:%d] Error decoding image %s: %v", messageBody.BarcodeScanLogID, fileName, err)
			continue
		}

		if barcode, err := extractBarcodeFromImage(img); err == nil && barcode != "" {
			foundBarcodes = append(foundBarcodes, barcode)
		}
	}

	// Log results
	duration := time.Since(startTime)
	log.Printf("[ID:%d] Processing completed in %v", messageBody.BarcodeScanLogID, duration)
	log.Printf("[ID:%d] Found %d barcodes: %v", messageBody.BarcodeScanLogID, len(foundBarcodes), foundBarcodes)

	// Send results
	data := BarcodeData{
		BarcodeScanLogID: messageBody.BarcodeScanLogID,
		BarcodeArray:     foundBarcodes,
	}

	payloadJSON, _ := json.Marshal(data)
	log.Printf("[ID:%d] Sending callback payload: %s", messageBody.BarcodeScanLogID, string(payloadJSON))

	if err := callRubyEndpoint(data); err != nil {
		log.Printf("[ID:%d] Error sending callback: %v", messageBody.BarcodeScanLogID, err)
	} else {
		log.Printf("[ID:%d] Successfully sent callback", messageBody.BarcodeScanLogID)
	}

	// Return response
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