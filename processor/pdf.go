package processor

import (
	"context"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	Bucket  string `json:"bucket"`
	Key     string `json:"key"`
	Barcode string `json:"barcode"`
}

type BarcodeData struct {
	Barcode   string `json:"barcode"`
	Filename  string `json:"filename"`
	Bucket    string `json:"bucket"`
}

func getS3Client() (*s3.Client, error) {
	if os.Getenv("TEST_PDF_PATH") != "" {
		return nil, nil
	}
	
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg), nil
}

func extractBarcodeFromImage(img image.Image) (string, error) {
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", err
	}

	// Try different barcode formats
	readers := []gozxing.Reader{
		oned.NewMultiFormatUPCEANReader(nil),
		oned.NewCode128Reader(),
		oned.NewCode39Reader(),
	}

	for _, reader := range readers {
		result, err := reader.Decode(bmp, nil)
		if err == nil {
			format := result.GetBarcodeFormat().String()
			log.Printf("Found %s barcode: %s", format, result.GetText())
			return result.GetText(), nil
		}
	}

	return "", nil // Return nil error when no barcode is found
}

func callRubyEndpoint(data BarcodeData) error {
	apiEndpoint := os.Getenv("API_ENDPOINT")
	if apiEndpoint == "" {
		return fmt.Errorf("API_ENDPOINT environment variable not set")
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("error marshaling JSON: %v", err)
	}

	req, err := http.NewRequest("POST", apiEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
    
    // Create HTTP client with timeout
    client := &http.Client{Timeout: 10 * time.Second}
    
    resp, err := client.Do(req)
    if err != nil {
        return fmt.Errorf("error making request: %v", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("API returned non-200 status: %d", resp.StatusCode)
    }

    log.Printf("Successfully sent barcode data to API: %s", data.Barcode)
    return nil
}

func HandleRequest(ctx context.Context, s3Event events.S3Event) (Response, error) {
	var pdfBytes []byte
	var err error
	var filename, bucket string

	if testPath := os.Getenv("TEST_PDF_PATH"); testPath != "" {
		// Local testing mode - read file directly
		pdfBytes, err = os.ReadFile(testPath)
		if err != nil {
			return Response{StatusCode: 500, Body: "Error reading test PDF"}, err
		}
		filename = filepath.Base(testPath)
		bucket = "test-bucket"
	} else {
		// Get the S3 bucket and key
		record := s3Event.Records[0]
		bucket = record.S3.Bucket.Name
		key := record.S3.Object.Key
		filename = filepath.Base(key)

		// Initialize S3 client
		s3Client, err := getS3Client()
		if err != nil {
			return Response{StatusCode: 500, Body: "Error initializing S3 client"}, err
		}

		// Get the PDF from S3
		result, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return Response{StatusCode: 500, Body: "Error getting object from S3"}, err
		}
		defer result.Body.Close()

		// Read PDF into memory
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(result.Body); err != nil {
			return Response{StatusCode: 500, Body: "Error reading PDF"}, err
		}
		pdfBytes = buf.Bytes()
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

	// Extract images from PDF
	if err := api.ExtractImagesFile(tmpPDF, tmpDir, nil, model.NewDefaultConfiguration()); err != nil {
		return Response{StatusCode: 500, Body: "Error extracting images from PDF"}, err
	}

	// Read extracted images from temp directory
	files, err := os.ReadDir(tmpDir)
	if err != nil {
		return Response{StatusCode: 500, Body: "Error reading extracted images"}, err
	}

	// Process each image file
	for i, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) != ".pdf" {
			imgPath := filepath.Join(tmpDir, file.Name())
			imgFile, err := os.Open(imgPath)
			if err != nil {
				log.Printf("Error opening image %s: %v", file.Name(), err)
				continue
			}

			img, _, err := image.Decode(imgFile)
			imgFile.Close()
			if err != nil {
				log.Printf("Error decoding image %s: %v", file.Name(), err)
				continue
			}

			log.Printf("Processing image %d: %s", i+1, file.Name())
			barcode, err := extractBarcodeFromImage(img)
			if err != nil {
				log.Printf("Error extracting barcode from image %s: %v", file.Name(), err)
				continue
			}
			if barcode != "" {
				log.Printf("Found barcode in image %s: %s", file.Name(), barcode)
				data := BarcodeData{
					Barcode:  barcode,
					Filename: filename,
					Bucket:   bucket,
				}
				if err := callRubyEndpoint(data); err != nil {
					log.Printf("Error sending barcode data to API: %v", err)
				}
				responseBody := ResponseBody{
					Bucket:  bucket,
					Key:     filename,
					Barcode: barcode,
				}
				
				jsonBody, _ := json.Marshal(responseBody)
				return Response{
					StatusCode: 200,
					Body:       string(jsonBody),
				}, nil
			} else {
				log.Printf("No barcode found in image %s", file.Name())
			}
		}
	}

	return Response{
		StatusCode: 404,
		Body:       "No barcode found in PDF",
	}, nil
}